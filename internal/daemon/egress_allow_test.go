package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// postAutoReply sends body to POST /autoreply and decodes the answer.
func postAutoReply(t *testing.T, d *Daemon, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	d.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// savedEgressAllow is the allowlist config.json holds now.
func savedEgressAllow(t *testing.T, d *Daemon) []string {
	t.Helper()
	b, err := os.ReadFile(d.layout.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.AutoReply == nil {
		return nil
	}
	return egressAllowOf(c.AutoReply)
}

// LEC §2.3.4, CN-1, CN-8: POST /autoreply takes sandbox.egress_allow: checked and saved in its normal
// form; a request naming only sandbox changes only that, and another request keeps it; a bad entry is a
// 400 with its sentence and changes nothing; every change of the list is recorded with its old and new
// value; /status shows it.
func TestAutoReplyEgressAllowRoute(t *testing.T) {
	d := newTestDaemon(t, "", false)
	code, out := postAutoReply(t, d, `{"backend":"exec","agent":"cursor","untrusted":"sandbox","sandbox":{"egress_allow":["PyPI.org","pypi.org"]}}`)
	if code != 200 || fmt.Sprint(savedEgressAllow(t, d)) != "[pypi.org]" {
		t.Fatalf("set: %d %v; saved %v", code, out, savedEgressAllow(t, d))
	}
	code, out = postAutoReply(t, d, `{"sandbox":{"egress_allow":["pypi.org","files.pythonhosted.org"]}}`)
	cur := d.config().AutoReply
	if code != 200 || cur.Backend != "exec" || cur.Agent != "cursor" || cur.UntrustedMode() != UntrustedSandbox ||
		fmt.Sprint(egressAllowOf(cur)) != "[pypi.org files.pythonhosted.org]" {
		t.Fatalf("sandbox only: %d %v; now %+v", code, out, cur)
	}
	if code, _ := postAutoReply(t, d, `{"backend":"exec","agent":"cursor","untrusted":"sandbox","model":"sonnet"}`); code != 200 ||
		len(egressAllowOf(d.config().AutoReply)) != 2 {
		t.Fatalf("a request without sandbox dropped the list: %d %v", code, d.config().AutoReply)
	}
	for _, tc := range []struct{ body, reason, sentence string }{
		{`{"sandbox":{"egress_allow":["10.0.0.1"]}}`, "egress_allow_invalid",
			a2ashape.Sentence("egress_allow_invalid", a2ashape.Lang(), a2ashape.Args{"value": "10.0.0.1", "why": "ip"})},
		{`{"sandbox":{"egress_allow":["pypi.org","api.openai.com"]}}`, "egress_allow_model_host",
			a2ashape.Sentence("egress_allow_model_host", a2ashape.Lang(), a2ashape.Args{"value": "api.openai.com"})},
		{`{"backend":"exec","agent":"opencode","api_base":"https://pypi.org/v1","model":"m"}`, "egress_allow_model_host",
			a2ashape.Sentence("egress_allow_model_host", a2ashape.Lang(), a2ashape.Args{"value": "pypi.org"})},
		{`{"sandbox":{"egress_allow":["a.b","*.c.d"]}}`, "egress_allow_invalid",
			a2ashape.Sentence("egress_allow_invalid", a2ashape.Lang(), a2ashape.Args{"value": "*.c.d", "why": "wildcard"})},
	} {
		code, out := postAutoReply(t, d, tc.body)
		if code != 400 || out["reason"] != tc.reason || out["error"] != tc.sentence {
			t.Errorf("%s: %d %v", tc.body, code, out)
		}
	}
	if got := fmt.Sprint(savedEgressAllow(t, d)); got != "[pypi.org files.pythonhosted.org]" {
		t.Errorf("a refused request changed the list: %s", got)
	}
	rec := httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var st struct {
		AutoReply struct {
			Sandbox struct {
				EgressAllow []string `json:"egress_allow"`
			} `json:"sandbox"`
		} `json:"auto_reply"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || fmt.Sprint(st.AutoReply.Sandbox.EgressAllow) != "[pypi.org files.pythonhosted.org]" {
		t.Errorf("/status: %v %s", err, rec.Body)
	}
	if code, _ := postAutoReply(t, d, `{"sandbox":{"egress_allow":[]}}`); code != 200 || d.config().AutoReply.Sandbox != nil {
		t.Fatalf("clear: %d %+v", code, d.config().AutoReply)
	}
	rec = httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(rec.Body.String(), `"sandbox":{"egress_allow":[]}`) {
		t.Errorf("/status after clearing: %s", rec.Body)
	}
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	var changes []string
	for _, r := range recs {
		if r.Payload["field"] == "auto_reply.sandbox.egress_allow" {
			changes = append(changes, fmt.Sprint(r.Payload["from"], "→", r.Payload["to"]))
		}
	}
	if strings.Join(changes, " ") != "[]→[pypi.org] [pypi.org]→[pypi.org files.pythonhosted.org] [pypi.org files.pythonhosted.org]→[]" {
		t.Errorf("policy changes %q", changes)
	}
}

// LEC §3.2: a config.json edited to hold an entry CN-1 refuses does not start, with the entry's sentence.
func TestEgressAllowIsCheckedAtStart(t *testing.T) {
	for _, tc := range []struct {
		autoReply map[string]any
		reason    string
	}{
		{map[string]any{"backend": "exec", "agent": "claude", "sandbox": map[string]any{"egress_allow": []string{"pypi.org"}}}, ""},
		{map[string]any{"backend": "exec", "agent": "claude", "sandbox": map[string]any{"egress_allow": []string{"PyPI.org"}}}, ""},
		{map[string]any{"backend": "exec", "agent": "claude", "sandbox": map[string]any{"egress_allow": []string{"169.254.169.254"}}}, "egress_allow_invalid"},
		{map[string]any{"backend": "exec", "agent": "claude", "sandbox": map[string]any{"egress_allow": []string{"api.anthropic.com"}}}, "egress_allow_model_host"},
		{map[string]any{"backend": "exec", "agent": "codex", "api_base": "https://llm.example.org/v1", "model": "m",
			"sandbox": map[string]any{"egress_allow": []string{"llm.example.org"}}}, "egress_allow_model_host"},
	} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "auto_reply": tc.autoReply})
		err := CheckConfigForStart(NewLayout(root))
		var cr *ConfigRefusal
		switch {
		case tc.reason == "" && err != nil:
			t.Errorf("%v: %v", tc.autoReply, err)
		case tc.reason != "" && (!errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Key != tc.reason):
			t.Errorf("%v: %v, want %s", tc.autoReply, err, tc.reason)
		}
	}
}
