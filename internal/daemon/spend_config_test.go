package daemon

// The payments block as configuration (A2A-DESIGN §8.6, SI-5): its defaults,
// the explicit block a fresh or migrated config is written with, and the
// route that changes it. Ported from wp/cli's payments_config_test.go when
// the two implementations were merged into spend.go and tasks_pay.go.

import (
	"encoding/json"
	"os"
	"testing"
)

// A payments block that leaves a key out gets that key's default, not zero;
// an explicit 0 is kept; an explicit empty payees_file turns the payee list
// off.
func TestPaymentsBlockDefaults(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"payments":{"auto_max":3}}`), &c); err != nil {
		t.Fatal(err)
	}
	want := SpendLimits{AutoMax: 3, ExplicitMax: defaultExplicitMax, DailyMax: defaultDailyMax, PayeesFile: defaultPayeesFile}
	if got := c.Payments.Limits(); got != want {
		t.Fatalf("partial block = %+v, want %+v", got, want)
	}
	if err := json.Unmarshal([]byte(`{"payments":{"payees_file":"","explicit_max":0}}`), &c); err != nil {
		t.Fatal(err)
	}
	if got := c.Payments.Limits(); got.PayeesFile != "" || got.ExplicitMax != 0 || got.DailyMax != defaultDailyMax {
		t.Fatalf("explicit empty payees_file and explicit_max 0: %+v", got)
	}
	var none *PaymentsConfig
	if got := none.Limits(); got != (SpendLimits{ExplicitMax: defaultExplicitMax, DailyMax: defaultDailyMax, PayeesFile: defaultPayeesFile}) {
		t.Fatalf("no block: %+v", got)
	}
	// defaultPayments states every key and means the same as no block.
	d := defaultPayments()
	if d.Limits() != none.Limits() {
		t.Fatalf("defaultPayments %+v differs from the defaults %+v", d.Limits(), none.Limits())
	}
	b, _ := json.Marshal(d)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"auto_max", "agent_max", "agent_daily_max", "explicit_max", "daily_max", "payees_file"} {
		if _, ok := m[k]; !ok {
			t.Errorf("defaultPayments leaves %s out: %s", k, b)
		}
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
	if err := json.Unmarshal(m["payments"], &p); err != nil || p["auto_max"] != float64(0) ||
		p["explicit_max"] != float64(defaultExplicitMax) || p["payees_file"] != defaultPayeesFile {
		t.Fatalf("payments on disk = %s (%v)", m["payments"], err)
	}
}

// POST /payments/limits changes the limits named, saves them, writes
// anet.policy.changed per changed limit, and refuses a key it does not know
// rather than ignoring it.
func TestPaymentLimitsRoute(t *testing.T) {
	p := newPlane(t)
	call := func(body string) (int, SpendStatus) {
		resp, b := p.req(t, "POST", "/payments/limits", body, p.bearer)
		var o SpendStatus
		if resp.StatusCode == 200 {
			if err := json.Unmarshal(b, &o); err != nil {
				t.Fatalf("%s: %v", b, err)
			}
		}
		return resp.StatusCode, o
	}
	_, prior := p.d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	code, o := call(`{"set":{"auto_max":5,"daily_max":50}}`)
	if code != 200 || o.AutoMax != 5 || o.DailyMax != 50 {
		t.Fatalf("write: %d %+v", code, o)
	}
	cfg, err := LoadConfig(p.d.layout)
	if err != nil || cfg.Payments.Limits().AutoMax != 5 {
		t.Fatalf("saved auto_max = %d (%v)", cfg.Payments.Limits().AutoMax, err)
	}
	if p.d.config().Payments.Limits().AutoMax != 5 {
		t.Fatal("in-memory limit not updated")
	}
	_, recs := p.d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	if len(recs) != len(prior)+1 || recs[len(recs)-1].Payload["field"] != "payments.auto_max" {
		t.Fatalf("policy change evidence = %+v", recs)
	}
	for _, body := range []string{`{}`, `{"set":{"payees":1}}`, `{"set":{"auto_max":-1}}`, `{"set":{"auto_max":"9"}}`} {
		if code, _ := call(body); code != 400 {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if got := p.d.config().Payments.Limits(); got.AutoMax != 5 || got.PayeesFile != defaultPayeesFile {
		t.Fatalf("a refused request changed the limits: %+v", got)
	}
}

// A limit whose save failed is not left in force: the caller was told it
// failed, and the one enforcement point reads the in-memory config.
func TestAFailedLimitWriteLeavesTheLimits(t *testing.T) {
	d := newBareDaemon(t)
	before := d.config().Payments.Limits()
	// A directory where config.json goes: the atomic rename fails.
	_ = os.Remove(d.layout.ConfigPath())
	if err := os.Mkdir(d.layout.ConfigPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	_, prior := d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged})
	if err := d.SetSpendLimits(map[string]uint64{"agent_max": 5}, nil); err == nil {
		t.Fatal("save over a directory succeeded")
	}
	if got := d.config().Payments.Limits(); got != before {
		t.Fatalf("limits after a failed save: %+v, were %+v", got, before)
	}
	if _, recs := d.ledger.Evidence(EvidenceQuery{EventType: EvPolicyChanged}); len(recs) != len(prior) {
		t.Fatalf("a failed change was recorded: %+v", recs)
	}
}
