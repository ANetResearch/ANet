package daemon

// LEC §2.3.3, §2.3.4, AV-8 and AV-10 (EXE-5): POST /autoreply merges what it names into the current
// configuration and never shows the key; /status says only whether one is set; rotateLocalTokens
// replaces both local tokens and the old ones stop working at once.

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/anethome"
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
		got := d.config().AutoReply
		if got == nil || !sameAutoReply(*got, s.want) {
			t.Fatalf("step %d %s: config %+v, want %+v", i, s.body, got, s.want)
		}
		saved, err := LoadConfig(d.layout)
		if err != nil || saved.AutoReply == nil || !sameAutoReply(*saved.AutoReply, s.want) {
			t.Fatalf("step %d: config.json holds %+v (%v)", i, saved.AutoReply, err)
		}
	}
	code, out, raw := postAutoReply(t, d, `{"off":true}`)
	if code != http.StatusOK || out["auto_reply"] != nil || d.config().AutoReply != nil {
		t.Fatalf("off: %d %s, config %+v", code, raw, d.config().AutoReply)
	}
}

func sameAutoReply(a, b AutoReplyConfig) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
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
	if d.config().AutoReply.APIKey != key {
		t.Fatal("the key was not kept")
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
		if ar := d.config().AutoReply; ar.APIKey != "sk-1" || ar.Model != "" {
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
	before := *d.config().AutoReply
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
		if !sameAutoReply(*d.config().AutoReply, before) {
			t.Errorf("%s changed the configuration: %+v", c.body, d.config().AutoReply)
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
