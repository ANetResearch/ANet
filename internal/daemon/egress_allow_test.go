//go:build mod4_pending

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
	if AutoReplyOf(c) == nil {
		return nil
	}
	return egressAllowOf(AutoReplyOf(c))
}

// LEC §2.3.4, CN-1, CN-8: POST /autoreply takes sandbox.egress_allow: checked and saved in its normal
// form; a request naming only sandbox changes only that, and another request keeps it; a bad entry is a
// 400 with its sentence and changes nothing; every change of the list is recorded with its old and new
// value; /status shows it.
func TestAutoReplyEgressAllowRoute(t *testing.T) {
	d := newTestDaemon(t, "", false)
	code, out, _ := postAutoReply(t, d, `{"backend":"exec","agent":"cursor","untrusted":"sandbox","sandbox":{"egress_allow":["PyPI.org","pypi.org"]}}`)
	if code != 200 || fmt.Sprint(savedEgressAllow(t, d)) != "[pypi.org]" {
		t.Fatalf("set: %d %v; saved %v", code, out, savedEgressAllow(t, d))
	}
	code, out, _ = postAutoReply(t, d, `{"sandbox":{"egress_allow":["pypi.org","files.pythonhosted.org"]}}`)
	cur := AutoReplyOf(d.config())
	if code != 200 || cur.Backend != "exec" || cur.Agent != "cursor" || cur.UntrustedMode() != UntrustedSandbox ||
		fmt.Sprint(egressAllowOf(cur)) != "[pypi.org files.pythonhosted.org]" {
		t.Fatalf("sandbox only: %d %v; now %+v", code, out, cur)
	}
	if code, _, _ := postAutoReply(t, d, `{"backend":"exec","agent":"cursor","untrusted":"sandbox","model":"sonnet"}`); code != 200 ||
		len(egressAllowOf(AutoReplyOf(d.config()))) != 2 {
		t.Fatalf("a request without sandbox dropped the list: %d %v", code, AutoReplyOf(d.config()))
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
		code, out, _ := postAutoReply(t, d, tc.body)
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
	if code, _, _ := postAutoReply(t, d, `{"sandbox":{"egress_allow":[]}}`); code != 200 || AutoReplyOf(d.config()).Sandbox != nil {
		t.Fatalf("clear: %d %+v", code, AutoReplyOf(d.config()))
	}
	rec = httptest.NewRecorder()
	d.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if !strings.Contains(rec.Body.String(), `"egress_allow":[]`) {
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
