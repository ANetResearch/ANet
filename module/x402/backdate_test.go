package x402

import (
	"testing"
	"time"
)

// A hub backdates the Timestamp of a new epoch record and understates this node balance.
func TestABackdatedEpochCannotSwitchOffTheAudit(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	f.addEpoch(d-3, 100, 10)
	m, _ := f.hubbed()
	if rep := mustAudit(t, m); !rep.Verified || rep.Checked != 1 {
		t.Fatalf("baseline: %+v", rep)
	}
	// the hub now cheats: balance 1 instead of 100 (no ledger entries explain it), timestamp 30 days back
	old := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	f.addEpochAt(d-1, 1, 10, old)
	rep := mustAudit(t, m)
	t.Logf("verified=%v checked=%d findings=%+v", rep.Verified, rep.Checked, rep.Findings)
	if rep.Verified {
		t.Errorf("a backdated epoch hid a 100->1 balance drop: verified=%v findings=%+v", rep.Verified, rep.Findings)
	}
}

// control: same cheat with an honest timestamp is caught
func TestAnHonestlyTimedEpochDropIsCaught(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	f.addEpoch(d-3, 100, 10)
	m, _ := f.hubbed()
	mustAudit(t, m)
	f.addEpoch(d-1, 1, 10)
	rep := mustAudit(t, m)
	t.Logf("verified=%v findings=%+v", rep.Verified, rep.Findings)
	if rep.Verified {
		t.Errorf("control not caught")
	}
}
