package daemon

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A payments block that leaves a key out gets that key's default, not zero;
// an explicit empty payees_file turns the payee list off.
func TestPaymentsBlockDefaults(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"payments":{"auto_max":3}}`), &c); err != nil {
		t.Fatal(err)
	}
	want := defaultPayments()
	want.AutoMax = 3
	if *c.Payments != want {
		t.Fatalf("partial block = %+v, want %+v", *c.Payments, want)
	}
	c = Config{}
	if err := json.Unmarshal([]byte(`{"payments":{"payees_file":""}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Payments.PayeesFile != "" || c.Payments.ExplicitMax != defaultExplicitMax {
		t.Fatalf("explicit empty payees_file: %+v", *c.Payments)
	}
	if got := (Config{}).payments(); got != defaultPayments() {
		t.Fatalf("no block: %+v", got)
	}
}

// A config without the block is given it at load and saved with it, so
// the file states the limits (SI-5).
func TestAConfigWithoutPaymentsIsGivenTheDefaults(t *testing.T) {
	d := newBareDaemon(t)
	b, err := os.ReadFile(d.layout.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(m["payments"], &p); err != nil || p["auto_max"] != float64(0) || p["payees_file"] != defaultPayeesFile {
		t.Fatalf("payments on disk = %s (%v)", m["payments"], err)
	}
}

// POST /payments/limits reports the limits, changes those named, saves them,
// writes anet.policy.changed per changed limit, and refuses a key it does not
// know rather than ignoring it.
func TestPaymentLimitsRoute(t *testing.T) {
	p := newPlane(t)
	type out struct {
		Payments PaymentsConfig `json:"payments"`
		Changed  []string       `json:"changed"`
	}
	call := func(body string) (int, out) {
		resp, b := p.req(t, "POST", "/payments/limits", body, p.bearer)
		var o out
		if resp.StatusCode == 200 {
			if err := json.Unmarshal(b, &o); err != nil {
				t.Fatalf("%s: %v", b, err)
			}
		}
		return resp.StatusCode, o
	}
	code, o := call("{}")
	if code != 200 || o.Payments != defaultPayments() || len(o.Changed) != 0 {
		t.Fatalf("read: %d %+v", code, o)
	}
	code, o = call(`{"auto_max":5,"daily_max":50}`)
	if code != 200 || o.Payments.AutoMax != 5 || strings.Join(o.Changed, ",") != "payments.auto_max" {
		t.Fatalf("write: %d %+v", code, o)
	}
	cfg, err := LoadConfig(p.d.layout)
	if err != nil || cfg.payments().AutoMax != 5 {
		t.Fatalf("saved auto_max = %d (%v)", cfg.payments().AutoMax, err)
	}
	if p.d.config().payments().AutoMax != 5 {
		t.Fatal("in-memory limit not updated")
	}
	_, recs := p.d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	if len(recs) != 1 || recs[0].Payload["field"] != "payments.auto_max" {
		t.Fatalf("policy change evidence = %+v", recs)
	}
	for _, body := range []string{`{"payees_file":""}`, `{"auto_max":-1}`, `{"auto_max":"9"}`} {
		if code, _ := call(body); code != 400 {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if got := p.d.config().payments(); got.AutoMax != 5 || got.PayeesFile != defaultPayeesFile {
		t.Fatalf("a refused request changed the limits: %+v", got)
	}
}
