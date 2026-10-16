package daemon

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// spendCheckCase is one row of testdata/spend_check.json (V-CUX8): a fixed
// configuration, ledger and signed-price cache, and the anet.spend_check
// that must come out of them.
type spendCheckCase struct {
	Name            string            `json:"name"`
	Amount          uint64            `json:"amount"`
	Limits          SpendLimits       `json:"limits"`
	SpentToday      uint64            `json:"spent_today"`
	AgentSpentToday uint64            `json:"agent_spent_today"`
	PayeeListed     bool              `json:"payee_listed"`
	PayeesReadable  bool              `json:"payees_readable"`
	Capability      string            `json:"capability"`
	PriceHave       bool              `json:"price_have"`
	Price           uint64            `json:"price"`
	Expired         bool              `json:"expired"`
	NotPayable      bool              `json:"not_payable"`
	Want            map[string]string `json:"want"`
}

// inputFor builds spendCheck's input the way spendCheckFor does, from the
// case's ledger: the same arithmetic as SpendStatus, and the manual tier
// judged by evalSpend.
func (c spendCheckCase) inputFor() SpendCheckInput {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	start, _ := localDayBounds(now)
	at := start.UnixMilli() + 1000
	var records []spendRecord
	if c.AgentSpentToday > 0 {
		records = append(records, spendRecord{at: at, amount: c.AgentSpentToday, purpose: module.PurposeTaskAgent})
	}
	if c.SpentToday > c.AgentSpentToday {
		records = append(records, spendRecord{at: at, amount: c.SpentToday - c.AgentSpentToday, purpose: module.PurposeTaskManual})
	}
	leftToday := leftOf(c.Limits.DailyMax, c.SpentToday)
	agentLeft := min(leftOf(c.Limits.AgentDailyMax, c.AgentSpentToday), leftToday)
	var payees map[string]bool
	var perr error
	if !c.PayeesReadable {
		perr = os.ErrNotExist
	} else if c.PayeeListed {
		payees = map[string]bool{"payee": true}
	}
	in := SpendCheckInput{Amount: c.Amount, Expired: c.Expired, NotPayable: c.NotPayable}
	tier := func(purpose string, left uint64) PayInput {
		p := PayInput{Purpose: purpose, Amount: c.Amount, CapID: c.Capability, Peer: "payee", At: now,
			Left: left, ManualSingle: c.Limits.ExplicitMax, ManualLeft: leftToday, SpentToday: c.SpentToday,
			PayeesReadable: c.PayeesReadable, PayeeListed: c.PayeeListed}
		p.Single, _, _ = c.Limits.single(purpose)
		if p.Single > 0 && p.CapID != "" {
			p.Price = cardPrice{Have: c.PriceHave, Price: c.Price}
		}
		return p
	}
	in.Agent, in.Auto = tier(module.PurposeTaskAgent, agentLeft), tier(module.PurposeTaskAuto, leftToday)
	in.Manual = evalSpend(c.Limits, records, payees, perr, now, "payee", c.Amount, module.PurposeTaskManual)
	return in
}

func TestSpendCheckGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/spend_check.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []spendCheckCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got := spendCheck(c.inputFor()).Map()
		want := map[string]any{}
		for k, v := range c.Want {
			want[k] = v
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: anet.spend_check %v, want %v", c.Name, got, want)
		}
		// reason_limit, when there, is below the amount: the limit that
		// stopped the payment cannot have let it through.
		if lim, ok := got["reason_limit"].(string); ok {
			var l, a uint64
			if json.Unmarshal([]byte(lim), &l) != nil || json.Unmarshal([]byte(got["amount"].(string)), &a) != nil || l >= a {
				t.Errorf("%s: reason_limit %s is not below amount %v", c.Name, lim, got["amount"])
			}
		}
	}
}

// The fresh-install example of CUX-S3 comes out byte for byte.
func TestSpendCheckFreshInstallBytes(t *testing.T) {
	raw, _ := os.ReadFile("testdata/spend_check.json")
	var cases []spendCheckCase
	_ = json.Unmarshal(raw, &cases)
	b, err := json.Marshal(spendCheck(cases[0].inputFor()).Map())
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"amount":"3","reason":"over_single_limit","reason_limit":"0","reason_tier":"agent","who":"operator"}`
	if string(b) != want {
		t.Errorf("fresh install: %s, want %s", b, want)
	}
}

// CheckSpend answers as AdmitSpend would and records nothing.
func TestCheckSpendWritesNothing(t *testing.T) {
	d := newTestDaemon(t, "", false)
	d.mu.Lock()
	d.cfg.Payments = &PaymentsConfig{AgentMax: 10, AgentDailyMax: 10, ExplicitMax: u64val(10), DailyMax: u64val(50)}
	d.mu.Unlock()
	appendPeerFile(t, d, payeesFile, "did:anet:a")
	for i := 0; i < 5; i++ {
		if r := d.CheckSpend("did:anet:a", 10, module.PurposeTaskAgent); r != nil {
			t.Fatalf("check %d refused: %v", i, r)
		}
	}
	if st := d.SpendStatus(); st.SpentToday != 0 {
		t.Fatalf("CheckSpend recorded %d", st.SpentToday)
	}
	if err := d.AdmitSpend("did:anet:a", 10, module.PurposeTaskAgent); err != nil {
		t.Fatal(err)
	}
	r := d.CheckSpend("did:anet:a", 1, module.PurposeTaskAgent)
	if r == nil || r.Code != SpendOverAgentDaily {
		t.Fatalf("after the day's agent limit: %v", r)
	}
	if r := d.CheckSpend("did:anet:b", 1, module.PurposeTaskManual); r != nil {
		t.Errorf("the manual tier is not bound by the payee list: %v", r)
	}
	if st := d.SpendStatus(); st.SpentToday != 10 {
		t.Errorf("spent %d, want 10", st.SpentToday)
	}
}

// The automatic tier counts against daily_max only (HPP PAY-D6).
func TestAutoTierCountsOnlyDailyMax(t *testing.T) {
	d := newTestDaemon(t, "", false)
	d.mu.Lock()
	d.cfg.Payments = &PaymentsConfig{AutoMax: 5, AgentMax: 5, AgentDailyMax: 5, ExplicitMax: u64val(10), DailyMax: u64val(50)}
	d.mu.Unlock()
	appendPeerFile(t, d, payeesFile, "did:anet:a")
	for i := 0; i < 3; i++ {
		if err := d.AdmitSpend("did:anet:a", 5, module.PurposeTaskAuto); err != nil {
			t.Fatalf("automatic payment %d: %v", i, err)
		}
	}
	if st := d.SpendStatus(); st.AgentSpentToday != 0 || st.SpentToday != 15 || st.AgentLeftToday != 5 {
		t.Errorf("status %+v: automatic payments must not count against agent_daily_max", st)
	}
	if countsAgentDaily(module.PurposeTaskAuto) || countsAgentDaily(module.PurposeTaskManual) || !countsAgentDaily(module.PurposeTaskAgent) {
		t.Error("countsAgentDaily is true for the agent tier only")
	}
}

func TestLimitsSummaryIsTheOneSentence(t *testing.T) {
	off := SpendStatus{ExplicitMax: 10, DailyMax: 50, LeftToday: 50}
	if got, want := off.LimitsSummary(a2ashape.English), "Automatic and agent payments are off (limit 0); manual payments (anet pay): ≤10 each, 50 left today."; got != want {
		t.Errorf("off: %q, want %q", got, want)
	}
	on := SpendStatus{AutoMax: 5, AgentMax: 8, ExplicitMax: 10, DailyMax: 50, LeftToday: 44, AgentLeftToday: 20, Payees: 2}
	if got, want := on.LimitsSummary(a2ashape.English), "Automatic payments: ≤5 each; agent payments: ≤8 each, 20 left today for the agent; manual payments (anet pay): ≤10 each; 44 left today across all three; automatic and agent payments go only to the 2 payees on your list."; got != want {
		t.Errorf("on: %q, want %q", got, want)
	}
}
