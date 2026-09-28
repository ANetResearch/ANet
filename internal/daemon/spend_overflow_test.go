package daemon

import (
	"math"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// The daily totals were compared as spent+amount > limit in plain uint64
// arithmetic. On a node whose per-payment limit is set to the top of the
// range ("no single limit") and whose daily limit is not, a payment of
// 2^64-20 after 50 spent wrapped the sum round to 29 and passed a daily
// limit of 100; the provider chooses the quoted amount. Saturating sums
// keep the daily limit a limit whatever the single one is.
func TestADailyLimitHoldsAgainstAnAmountThatWouldWrapTheSum(t *testing.T) {
	d := newTestDaemon(t, "", false)
	none := ""
	d.mu.Lock()
	d.cfg.Payments = &PaymentsConfig{AgentMax: math.MaxUint64, AgentDailyMax: 100,
		ExplicitMax: u64max(), DailyMax: u64val(100), PayeesFile: &none}
	d.mu.Unlock()
	if err := d.AdmitSpend("did:anet:a", 50, module.PurposeTaskManual); err != nil {
		t.Fatalf("50 of 100: %v", err)
	}
	for _, purpose := range []string{module.PurposeTaskManual, module.PurposeTaskAgent} {
		err := d.AdmitSpend("did:anet:a", math.MaxUint64-20, purpose)
		r, ok := isSpendRefusal(err)
		if !ok {
			t.Errorf("%s: 2^64-21 more after 50 of a daily 100 was admitted (%v)", purpose, err)
			continue
		}
		if r.Code != SpendOverDaily && r.Code != SpendOverAgentDaily {
			t.Errorf("%s: refused with %s, want a daily limit", purpose, r.Code)
		}
	}
	if st := d.SpendStatus(); st.Spent24h != 50 {
		t.Errorf("spent in 24h = %d, want 50", st.Spent24h)
	}
}

func u64val(n uint64) *uint64 { return &n }
func u64max() *uint64         { return u64val(math.MaxUint64) }
