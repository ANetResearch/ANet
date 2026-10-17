package daemon

// autoreply_settings_test.go: the kernel side of the auto-reply settings: where budgets and trust are
// checked and what the ledger records when they change. The module is the one registered by the import in
// autoreply_alias_test.go and is driven through the control plane.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// LEC §3.2, BU-3: budgets and trusted are checked wherever they are written: SetAutoReply and the
// control plane refuse with budget_invalid or config_invalid and the reason, the start check and New
// refuse a config.json edited by hand.
func TestAutoReplySettingsAreChecked(t *testing.T) {
	d := newBareDaemon(t)
	for body, key := range map[string]string{
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","sandbox":{"runs_per_day":2000}}`:                        "budget_invalid",
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","sandbox":{"runs_per_day":3,"runs_per_peer_per_day":4}}`: "budget_invalid",
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","sandbox":{"runs_per_peer_per_day":"many"}}`:             "budget_invalid",
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","sandbox":{"runs_per_day":{"n":1}}}`:                     "budget_invalid",
		`{"backend":"exec","agent":"claude","trusted":"always"}`:                                                  "config_invalid",
	} {
		rec := httptest.NewRecorder()
		d.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply", strings.NewReader(body)))
		var eb a2ashape.ErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &eb)
		if rec.Code != http.StatusBadRequest || eb.Reason != key {
			t.Errorf("%s: %d %s, want 400 %s", body, rec.Code, rec.Body, key)
		}
		if AutoReplyOf(d.config()) != nil {
			t.Fatalf("%s was saved", body)
		}
	}
	rec := httptest.NewRecorder()
	d.hAutoReply(rec, httptest.NewRequest(http.MethodPost, "/autoreply", strings.NewReader(
		`{"model":"m","api_base":"http://127.0.0.1:1/v1","trusted":"sandbox","sandbox":{"runs_per_day":7,"runs_per_peer_per_day":2}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("a valid write: %d %s", rec.Code, rec.Body)
	}
	if got := sandboxBudgetOf(t, d); got.RunsPerDay != 7 || got.RunsPerPeerPerDay != 2 || AutoReplyOf(d.config()).TrustedMode() != TrustedSandbox {
		t.Fatalf("after the write: %+v %+v", got, AutoReplyOf(d.config()))
	}
	for _, ar := range []map[string]any{
		{"backend": "openai", "api_base": "http://127.0.0.1:1/v1", "model": "m", "sandbox": map[string]any{"runs_per_day": -1}},
		{"backend": "exec", "agent": "claude", "trusted": "yes"},
	} {
		root := t.TempDir()
		writeTestConfig(t, root, map[string]any{"control_addr": "127.0.0.1:0", "auto_reply": ar})
		var cr *ConfigRefusal
		if err := CheckConfigForStart(NewLayout(root)); !errors.As(err, &cr) {
			t.Errorf("start check for %v: %v", ar, err)
		}
		if n, err := New(NewLayout(root)); err == nil {
			n.Close()
			t.Errorf("the daemon started with %v", ar)
		}
	}
}

// LEC §2.5: a change of auto_reply.trusted or of a budget is recorded as anet.policy.changed with the
// values before and after; turning auto-reply on carries them in the auto_reply record.
func TestTrustedAndBudgetChangesAreRecorded(t *testing.T) {
	d := newBareDaemon(t)
	base := AutoReplyConfig{Model: "m", APIBase: "http://127.0.0.1:1/v1"}
	if err := d.SetAutoReply(&base); err != nil {
		t.Fatal(err)
	}
	next := base
	next.Trusted = TrustedSandbox
	next.Sandbox = &AutoReplySandboxConfig{RunsPerDay: Budget(9), RunsPerPeerPerDay: Budget(3)}
	if err := d.SetAutoReply(&next); err != nil {
		t.Fatal(err)
	}
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged, Limit: 100})
	got := map[string]string{}
	for _, r := range recs {
		f, _ := r.Payload["field"].(string)
		b, _ := json.Marshal([]any{r.Payload["from"], r.Payload["to"]})
		got[f] = string(b)
	}
	for field, want := range map[string]string{
		"auto_reply.trusted":                       `["direct","sandbox"]`,
		"auto_reply.sandbox.runs_per_day":          `[20,9]`,
		"auto_reply.sandbox.runs_per_peer_per_day": `[5,3]`,
	} {
		if got[field] != want {
			t.Errorf("%s: %s, want %s", field, got[field], want)
		}
	}
	if !strings.Contains(got["auto_reply"], `"runs_per_day":9`) || !strings.Contains(got["auto_reply"], `"trusted":"sandbox"`) {
		t.Errorf("the auto_reply record: %s", got["auto_reply"])
	}
}
