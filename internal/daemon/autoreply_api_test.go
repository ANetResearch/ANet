//go:build mod4_pending

package daemon

// LEC §2.3.3, §2.3.4, AV-8 and AV-10 (EXE-5): POST /autoreply merges what it names into the current
// configuration and never shows the key; /status says only whether one is set; rotateLocalTokens
// replaces both local tokens and the old ones stop working at once.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/internal/localpeer"
)

// postAutoReply sends one /autoreply body to d's handler.
func postAutoReply(t *testing.T, d *Daemon, body string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// statusOf is d's /status answer.
func statusOf(t *testing.T, d *Daemon) (map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out, rec.Body.String()
}

// §2.3.4: a body changes the members it names and keeps the others, so `set` one setting at a time does
// not undo the last; null puts a member back to its default; {"off":true} removes the block.
func TestAutoReplyMergesWhatItNames(t *testing.T) {
	d := newBareDaemon(t)
	steps := []struct {
		body string
		want AutoReplyConfig
	}{
		{`{"backend":"exec","agent":"claude","untrusted":"sandbox"}`,
			AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: "sandbox"}},
		{`{"api_key":"sk-merge-1"}`,
			AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: "sandbox", APIKey: "sk-merge-1"}},
		{`{"model":"haiku","system_prompt":"be brief"}`,
			AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: "sandbox", APIKey: "sk-merge-1", Model: "haiku", SystemPrompt: "be brief"}},
		{`{"agent":"codex","model":null}`,
			AutoReplyConfig{Backend: "exec", Agent: "codex", Untrusted: "sandbox", APIKey: "sk-merge-1", SystemPrompt: "be brief"}},
		{`{"api_key":"  sk-merge-2\n"}`,
			AutoReplyConfig{Backend: "exec", Agent: "codex", Untrusted: "sandbox", APIKey: "sk-merge-2", SystemPrompt: "be brief"}},
		{`{"off":false,"untrusted":"off","api_key":null}`,
			AutoReplyConfig{Backend: "exec", Agent: "codex", Untrusted: "off", SystemPrompt: "be brief"}},
	}
	for i, s := range steps {
		code, _, raw := postAutoReply(t, d, s.body)
		if code != http.StatusOK {
			t.Fatalf("step %d %s: %d %s", i, s.body, code, raw)
		}
		got := AutoReplyOf(d.config())
		if got == nil || !sameAutoReply(*got, s.want) {
			t.Fatalf("step %d %s: config %+v, want %+v", i, s.body, got, s.want)
		}
		saved, err := LoadConfig(d.layout)
		if err != nil || AutoReplyOf(saved) == nil || !sameAutoReply(*AutoReplyOf(saved), s.want) {
			t.Fatalf("step %d: config.json holds %+v (%v)", i, AutoReplyOf(saved), err)
		}
	}
	code, out, raw := postAutoReply(t, d, `{"off":true}`)
	if code != http.StatusOK || out["auto_reply"] != nil || AutoReplyOf(d.config()) != nil {
		t.Fatalf("off: %d %s, config %+v", code, raw, AutoReplyOf(d.config()))
	}
}

func sameAutoReply(a, b AutoReplyConfig) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// §2.3.4: settings sent at once all land. Each request merges into what the request before it saved,
// not into a copy of the configuration read before that save, which would put back what the other
// request changed.
func TestAutoReplySettingsSentAtOnceAllLand(t *testing.T) {
	d := newBareDaemon(t)
	if code, _, raw := postAutoReply(t, d, `{"backend":"exec","agent":"claude"}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	bodies := []string{`{"model":"m1"}`, `{"system_prompt":"s"}`, `{"max_history":5}`,
		`{"api_timeout_seconds":9}`, `{"api_key":"sk-at-once"}`, `{"untrusted":"sandbox"}`, `{"openclaw_agent":"o"}`}
	var wg sync.WaitGroup
	for _, b := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _, raw := postAutoReply(t, d, b); code != http.StatusOK {
				t.Errorf("%s: %d %s", b, code, raw)
			}
		}()
	}
	wg.Wait()
	want := AutoReplyConfig{Backend: "exec", Agent: "claude", Model: "m1", SystemPrompt: "s",
		MaxHistory: 5, APITimeoutSeconds: 9, APIKey: "sk-at-once", Untrusted: "sandbox", OpenClawAgent: "o"}
	if got := AutoReplyOf(d.config()); got == nil || !sameAutoReply(*got, want) {
		t.Fatalf("after %d settings at once: %+v, want %+v", len(bodies), got, want)
	}
	if saved, err := LoadConfig(d.layout); err != nil || AutoReplyOf(saved) == nil || !sameAutoReply(*AutoReplyOf(saved), want) {
		t.Fatalf("config.json holds %+v (%v)", AutoReplyOf(saved), err)
	}
}

// "off" is a boolean. Another value is refused (config_invalid naming it) and nothing changes: taken as
// false it would leave auto-reply running for a caller who believes it stopped. null is its default.
func TestAutoReplyOffIsABoolean(t *testing.T) {
	d := newBareDaemon(t)
	if code, _, raw := postAutoReply(t, d, `{"backend":"exec","agent":"claude"}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	before := *AutoReplyOf(d.config())
	for _, body := range []string{`{"off":"true"}`, `{"off":1}`, `{"off":"yes","model":"m"}`, `{"off":{}}`} {
		code, out, raw := postAutoReply(t, d, body)
		if code != http.StatusBadRequest || out["reason"] != "config_invalid" || !strings.Contains(raw, "off") {
			t.Errorf("%s: %d %s", body, code, raw)
		}
		if ar := AutoReplyOf(d.config()); ar == nil || !sameAutoReply(*ar, before) {
			t.Errorf("%s changed the configuration: %+v", body, ar)
		}
	}
	if code, _, raw := postAutoReply(t, d, `{"off":null,"model":"m"}`); code != http.StatusOK || AutoReplyOf(d.config()).Model != "m" {
		t.Fatalf(`{"off":null,"model":"m"}: %d %s`, code, raw)
	}
	if code, _, raw := postAutoReply(t, d, `{"off":true}`); code != http.StatusOK || AutoReplyOf(d.config()) != nil {
		t.Fatalf(`{"off":true}: %d %s`, code, raw)
	}
}

// AV-8, M-AV-1: neither /autoreply's answer nor /status shows the key, only api_key_set; the key is kept
// (config.json, 0600) for the node to use.
func TestAutoReplyNeverShowsTheKey(t *testing.T) {
	d := newBareDaemon(t)
	const key = "sk-CANARY-a7d1e0"
	code, out, raw := postAutoReply(t, d, `{"backend":"exec","agent":"claude","api_key":"`+key+`"}`)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	if strings.Contains(raw, key) {
		t.Fatalf("/autoreply showed the key: %s", raw)
	}
	if ar, _ := out["auto_reply"].(map[string]any); ar["api_key_set"] != true {
		t.Fatalf("/autoreply: api_key_set is not true: %s", raw)
	}
	st, sraw := statusOf(t, d)
	if strings.Contains(sraw, key) {
		t.Fatalf("/status showed the key: %s", sraw)
	}
	if ar, _ := st["auto_reply"].(map[string]any); ar["api_key_set"] != true {
		t.Fatalf("/status: api_key_set is not true: %v", st["auto_reply"])
	}
	if AutoReplyOf(d.config()).APIKey != key {
		t.Fatal("the key was not kept")
	}
	// The policy change the write records (anet.policy.changed, LEC §2.5) says whether a key is set, never
	// the key: the evidence chain is read and exported by anet audit.
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	ev, _ := json.Marshal(recs)
	if strings.Contains(string(ev), key) {
		t.Fatalf("the policy change evidence holds the key: %s", ev)
	}
	if !strings.Contains(string(ev), `"api_key_set":true`) {
		t.Fatalf("the policy change evidence does not say a key is set: %s", ev)
	}
	// Without a key: false, on both.
	if code, out, raw = postAutoReply(t, d, `{"api_key":null}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	if ar, _ := out["auto_reply"].(map[string]any); ar["api_key_set"] != false {
		t.Fatalf("api_key_set after removing the key: %s", raw)
	}
	if st, _ = statusOf(t, d); st["auto_reply"].(map[string]any)["api_key_set"] != false {
		t.Fatalf("/status api_key_set after removing the key: %v", st["auto_reply"])
	}
}

// §2.3.4: an empty key is api_key_empty, and nothing changes.
func TestAutoReplyRefusesAnEmptyKey(t *testing.T) {
	d := newBareDaemon(t)
	if code, _, raw := postAutoReply(t, d, `{"backend":"exec","agent":"claude","api_key":"sk-1"}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	for _, body := range []string{`{"api_key":""}`, `{"api_key":"  \n "}`, `{"model":"m","api_key":""}`} {
		code, out, raw := postAutoReply(t, d, body)
		if code != http.StatusBadRequest || out["reason"] != "api_key_empty" ||
			out["error"] != a2ashape.Sentence("api_key_empty", a2ashape.Lang(), nil) {
			t.Errorf("%s: %d %s", body, code, raw)
		}
		if ar := AutoReplyOf(d.config()); ar.APIKey != "sk-1" || ar.Model != "" {
			t.Errorf("%s changed the configuration: %+v", body, ar)
		}
	}
}

// §2.3.4, §3.2: a value the configuration cannot take is refused with its key and rule (config_invalid)
// or api_base_insecure, and nothing changes.
func TestAutoReplyRefusesWhatCannotRun(t *testing.T) {
	d := newBareDaemon(t)
	if code, _, raw := postAutoReply(t, d, `{"backend":"exec","agent":"claude"}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, raw)
	}
	before := *AutoReplyOf(d.config())
	for _, c := range []struct{ body, reason, key string }{
		{`{"backend":"telepathy"}`, "config_invalid", "auto_reply.backend"},
		{`{"agent":"opencodex"}`, "config_invalid", "auto_reply.agent"},
		{`{"agent":null}`, "config_invalid", "auto_reply.agent"},
		{`{"untrusted":"maybe"}`, "config_invalid", "auto_reply.untrusted"},
		{`{"backend":"openai"}`, "config_invalid", "auto_reply.api_base"},
		{`{"backend":"openai","api_base":"https://models.example/v1"}`, "config_invalid", "auto_reply.model"},
		{`{"max_history":"twenty"}`, "config_invalid", "auto_reply.max_history"},
		{`{"api_kee":"sk-typo"}`, "config_invalid", "auto_reply.api_kee"},
		{`{"Agent":"codex"}`, "config_invalid", "auto_reply.Agent"},
		{`{"api_key":7}`, "config_invalid", "api_key"},
		{`{"api_base":"http://192.0.2.7:8000/v1"}`, "api_base_insecure", ""},
		{`{"api_base":"https://u:p@models.example/v1"}`, "api_base_insecure", ""},
	} {
		code, out, raw := postAutoReply(t, d, c.body)
		if code != http.StatusBadRequest || out["reason"] != c.reason {
			t.Errorf("%s: %d %s, want 400 %s", c.body, code, raw, c.reason)
			continue
		}
		if c.key != "" && !strings.Contains(raw, c.key) {
			t.Errorf("%s: the sentence does not name %s: %s", c.body, c.key, raw)
		}
		if strings.Contains(raw, "sk-typo") {
			t.Errorf("%s: a refusal repeated the value: %s", c.body, raw)
		}
		if !sameAutoReply(*AutoReplyOf(d.config()), before) {
			t.Errorf("%s changed the configuration: %+v", c.body, AutoReplyOf(d.config()))
		}
	}
	// A loopback http model service is fine.
	if code, _, raw := postAutoReply(t, d, `{"api_base":"http://127.0.0.1:11434/v1"}`); code != http.StatusOK {
		t.Fatalf("loopback http api_base: %d %s", code, raw)
	}
}

// §3.2: a config.json edited by hand to another untrusted mode, or to a model service on plain http
// elsewhere than this machine, does not start.
func TestAutoReplyConfigIsCheckedAtStart(t *testing.T) {
	for _, c := range []struct {
		ar     map[string]any
		reason string
	}{
		{map[string]any{"backend": "exec", "agent": "claude", "untrusted": "always"}, "config_invalid"},
		{map[string]any{"backend": "openai", "api_base": "http://192.0.2.7/v1", "model": "m"}, "api_base_insecure"},
		{map[string]any{"backend": "openai", "api_base": "http://localhost:11434/v1", "model": "m"}, ""},
		{map[string]any{"backend": "exec", "agent": "claude", "untrusted": "sandbox"}, ""},
	} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "auto_reply": c.ar})
		err := CheckConfigForStart(NewLayout(root))
		var cr *ConfigRefusal
		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%v: refused: %v", c.ar, err)
		case c.reason != "" && (!errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Code() != c.reason):
			t.Errorf("%v: %v, want %s", c.ar, err, c.reason)
		}
	}
}

// AV-10, M-AV-3: rotateLocalTokens replaces control_token.txt and the local A2A token file with new
// values (0600); from then on the old control token is refused (401) and the new one works, without a
// restart; a console session opened under the old token has ended.
func TestRotateLocalTokensRefusesBothOldTokens(t *testing.T) {
	p := newPlane(t)
	a2aDir := anethome.A2ADir(p.d.layout.Root)
	if err := os.MkdirAll(a2aDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a2aPath := filepath.Join(a2aDir, anethome.A2ATokenFile)
	oldA2A := strings.Repeat("ab", 32)
	if err := os.WriteFile(a2aPath, []byte(oldA2A+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldCtl := p.token
	if resp, b := p.req(t, "GET", "/status", "", p.bearer); resp.StatusCode != http.StatusOK {
		t.Fatalf("before: %d %s", resp.StatusCode, b)
	}
	sess := p.session(t)
	if resp, b := p.req(t, "POST", "/threads", "{}", sess.as(p)); resp.StatusCode != http.StatusOK {
		t.Fatalf("the console session before: %d %s", resp.StatusCode, b)
	}

	if err := p.d.rotateLocalTokens(); err != nil {
		t.Fatal(err)
	}

	newCtl := readTokenFile(t, p.d.layout.ControlTokenPath())
	newA2A := readTokenFile(t, a2aPath)
	if newCtl == oldCtl || newA2A == oldA2A || newCtl == newA2A || len(newA2A) != 64 || len(newCtl) < 32 {
		t.Fatalf("tokens after rotation: control %q (was %q), A2A %q (was %q)", newCtl, oldCtl, newA2A, oldA2A)
	}
	if resp, b := p.req(t, "GET", "/status", "", p.bearer); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old control token after rotation: %d %s", resp.StatusCode, b)
	}
	if resp, b := p.req(t, "GET", "/status", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+newCtl)
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("the new control token: %d %s", resp.StatusCode, b)
	}
	if resp, b := p.req(t, "POST", "/threads", "{}", sess.as(p)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a console session from before the rotation: %d %s", resp.StatusCode, b)
	}
	// /ping proves the token in force: where the socket table cannot be read (macOS, the BSDs), a CLI
	// that read the new token sends it only after this proof checks out (internal/localpeer).
	nonce := bytes.Repeat([]byte{0x5a}, 32)
	resp, b := p.req(t, "GET", "/ping?"+localpeer.ProofParam+"="+hex.EncodeToString(nonce), "", nil)
	var ping struct {
		Proof string `json:"proof"`
	}
	if err := json.Unmarshal(b, &ping); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("/ping after rotation: %d %s", resp.StatusCode, b)
	}
	listener := netip.MustParseAddrPort(p.srv.Listener.Addr().String())
	if ping.Proof != localpeer.Proof(newCtl, nonce, listener) {
		t.Fatalf("/ping after rotation proves %q, not the new token", ping.Proof)
	}
	// Idempotent: a rotation resumed after a crash calls it again.
	if err := p.d.rotateLocalTokens(); err != nil {
		t.Fatal(err)
	}
	if readTokenFile(t, p.d.layout.ControlTokenPath()) == newCtl || readTokenFile(t, a2aPath) == newA2A {
		t.Fatal("a second rotation kept a token")
	}
}

// A data dir whose local A2A interface never ran has no A2A token to replace, and none is made.
func TestRotateLocalTokensWithoutAnA2AInterface(t *testing.T) {
	p := newPlane(t)
	if err := p.d.rotateLocalTokens(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(anethome.A2ADir(p.d.layout.Root)); !os.IsNotExist(err) {
		t.Fatalf("an A2A state dir was made: %v", err)
	}
}

// A token that could not be replaced is still the compromised one, so the caller (KEL's emergency
// rotation) hears about it; the other token is replaced all the same.
func TestRotateLocalTokensReportsWhatItCouldNotReplace(t *testing.T) {
	p := newPlane(t)
	// a2a_token.txt is a directory with something in it: no file can be renamed over it, whoever runs
	// the test.
	stuck := filepath.Join(anethome.A2ADir(p.d.layout.Root), anethome.A2ATokenFile)
	if err := os.MkdirAll(filepath.Join(stuck, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := p.d.rotateLocalTokens()
	if err == nil || !strings.Contains(err.Error(), "local A2A token") {
		t.Fatalf("rotateLocalTokens with an A2A token it cannot replace: %v", err)
	}
	if resp, b := p.req(t, "GET", "/status", "", p.bearer); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old control token after a rotation that failed only for the A2A token: %d %s", resp.StatusCode, b)
	}
	if readTokenFile(t, p.d.layout.ControlTokenPath()) == p.token {
		t.Fatal("the control token was not replaced")
	}
}

func readTokenFile(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", path, fi, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// The exact-name rule holds inside a block of settings too (an object member whose field is a struct,
// merged member by member): "Hosts" sent next to a stored hosts would be overridden by it in silence.
func TestAutoReplyMemberNamesAreExactAtEveryDepth(t *testing.T) {
	type block struct {
		Hosts []string `json:"hosts,omitempty"`
		Limit int      `json:"limit,omitempty"`
	}
	type conf struct {
		Name  string `json:"name,omitempty"`
		Block *block `json:"block,omitempty"`
	}
	for _, c := range []struct{ body, want string }{
		{`{"name":"x","block":{"hosts":["a"],"limit":1}}`, ""},
		{`{"block":null}`, ""},
		{`{"block":{"Hosts":["a"]}}`, "block.Hosts"},
		{`{"block":{"hosts":["a"],"other":1}}`, "block.other"},
		{`{"Name":"x","block":{"hosts":[]}}`, "Name"},
		{`{"name":{"Hosts":1}}`, ""}, // not a block: its type is checked when the merge is decoded
	} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(c.body), &m); err != nil {
			t.Fatal(err)
		}
		if got := inexactMember(reflect.TypeFor[conf](), m); got != c.want {
			t.Errorf("%s: %q, want %q", c.body, got, c.want)
		}
	}
}
