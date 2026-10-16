//go:build !no_x402

package x402

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/liabtree"
)

func codes(r AuditReport) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Code)
	}
	return out
}

func mustAudit(t *testing.T, m *Module) AuditReport {
	t.Helper()
	rep, err := m.audit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// threeEpochs is a hub with three closed days in which the node held 100,
// then 130 after +30 of ledger entries, then 120 after a debit of 10.
func threeEpochs(t *testing.T) (*fakeHub, *Module, *testHost) {
	t.Helper()
	f := newFakeHub(t)
	d := today()
	f.entries = []ledgerEntry{
		{Seq: 10, Delta: 100, Reason: "operator grant", At: stamp(time.Now().Add(-70 * time.Hour).UnixMilli())},
		{Seq: 15, Delta: 30, Reason: "tx-in", At: stamp(time.Now().Add(-45 * time.Hour).UnixMilli())},
		{Seq: 25, Delta: -10, Reason: "auth-out", At: stamp(time.Now().Add(-20 * time.Hour).UnixMilli())},
	}
	f.balance = 120
	f.addEpoch(d-3, 100, 10, 50, 70)
	f.addEpoch(d-2, 130, 20, 50)
	f.addEpoch(d-1, 120, 30, 5, 5, 5)
	m, h := f.hubbed()
	return f, m, h
}

// PAY-S9, PAY-S10: a hub that tells the truth audits clean. Every epoch's
// proof reaches the signed root, the balance continues from the one before
// by exactly the ledger entries between, and the rows are kept in the
// module's state directory with the evidence of each check.
func TestAnHonestHubAuditsClean(t *testing.T) {
	f, m, h := threeEpochs(t)
	dir := t.TempDir()
	h.stateDir = dir
	m = newModule(t, h)
	// The node signed an authorization the debit belongs to.
	_ = h.RecordEvidence(EvPaymentAuthorized, map[string]any{"authorization_id": "auth-out", "amount": 10})
	rep := mustAudit(t, m)
	if !rep.Verified || !rep.ChainOK || len(rep.Findings) != 0 {
		t.Fatalf("an honest hub: %+v", rep)
	}
	if rep.Epochs != 3 || rep.Checked != 3 || rep.ProofsOK != 3 || rep.Latest == nil || rep.Latest.Balance != 120 {
		t.Fatalf("report: %+v", rep)
	}
	if got := len(h.eventsOf(EvSupplyProof)); got != 3 {
		t.Errorf("anet.supply.proof events = %d, want 3", got)
	}
	if heads := h.eventsOf(EvIssuanceHeadSeen); len(heads) != 1 || heads[0].payload["hub"] != f.ctrl.AID() {
		t.Errorf("head events: %+v", heads)
	}
	st, err := os.Stat(filepath.Join(dir, liabProofFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("proof file: %v %v", st, err)
	}
	// A second run checks nothing again, and a restart reads the file.
	reads := f.proofReads
	if rep2 := mustAudit(t, m); rep2.Checked != 0 || f.proofReads != reads || !rep2.Verified || rep2.ProofsOK != 3 {
		t.Errorf("second run: %+v, proof reads %d -> %d", rep2, reads, f.proofReads)
	}
	m3 := newModule(t, h)
	if rep3 := mustAudit(t, m3); rep3.Checked != 0 || rep3.ProofsOK != 3 {
		t.Errorf("after a restart: %+v", rep3)
	}
	if s, _ := m.AuditSummary().(map[string]any); s == nil || s["result"] != resultOK {
		t.Errorf("summary: %v", m.AuditSummary())
	}
}

// balance_mismatch: the hub shows a balance that the previous period and
// the ledger do not give.
func TestABalanceThatDoesNotContinueIsAFinding(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	f.entries = []ledgerEntry{{Seq: 15, Delta: 30, Reason: "tx-in", At: stamp(time.Now().Add(-45 * time.Hour).UnixMilli())}}
	f.addEpoch(d-3, 100, 10)
	f.addEpoch(d-2, 135, 20) // 100 + 30 would be 130
	m, _ := f.hubbed()
	rep := mustAudit(t, m)
	if rep.Verified || len(rep.Findings) != 1 || rep.Findings[0].Code != "balance_mismatch" {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	a := rep.Findings[0].Args
	if a["amount"] != uint64(135) || a["need"] != "130" {
		t.Errorf("args: %v", a)
	}
	// The failure stays the result until a later epoch checks out.
	if s, _ := m.AuditSummary().(map[string]any); s["result"] != "balance_mismatch" {
		t.Errorf("summary: %v", s)
	}
	if again := mustAudit(t, m); again.Verified || len(again.Findings) != 1 {
		t.Errorf("the failure was forgotten: %+v", again)
	}
}

// proof_invalid, in each of its ways. liabtree.Verify checks the path, the
// root and the sum only: the account and the day are the caller's to check.
func TestAProofThatIsNotThisNodesIsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		set  func(f *fakeHub)
	}{
		{"another account's leaf", "wrong_account", func(f *fakeHub) {
			f.proofOf = func(e *fakeEpoch, _ string) (int, any) {
				p, _ := e.tree.Proof("bafy-other-0")
				return 200, proofBody(p, e)
			}
		}},
		{"another period's proof", "wrong_period", func(f *fakeHub) {
			f.proofOf = func(e *fakeEpoch, aid string) (int, any) {
				other := f.epochs[0]
				if other == e {
					other = f.epochs[1]
				}
				p, _ := other.tree.Proof(aid)
				return 200, proofBody(p, other)
			}
		}},
		{"a path that does not reach the root", "path", func(f *fakeHub) {
			f.proofOf = func(e *fakeEpoch, aid string) (int, any) {
				p, _ := e.tree.Proof(aid)
				p.Path[0].Hash[0] ^= 1
				return 200, proofBody(p, e)
			}
		}},
		{"a balance that is not the leaf's", "path", func(f *fakeHub) {
			f.proofOf = func(e *fakeEpoch, aid string) (int, any) {
				p, _ := e.tree.Proof(aid)
				p.Balance++
				return 200, proofBody(p, e)
			}
		}},
		{"a record that is not the public one", "record_mismatch", func(f *fakeHub) {
			f.proofOf = func(e *fakeEpoch, aid string) (int, any) {
				p, _ := e.tree.Proof(aid)
				b := proofBody(p, e)
				bb := b.(struct {
					liabtree.WireProof
					FromDay  uint64 `json:"from_day"`
					LedgerHW uint64 `json:"ledger_hw"`
					Record   string `json:"record"`
				})
				bb.Record = base64.RawURLEncoding.EncodeToString(append([]byte{}, e.raw[:len(e.raw)-1]...))
				return 200, bb
			}
		}},
		{"a malformed spelling", "malformed", func(f *fakeHub) {
			f.proofOf = func(e *fakeEpoch, aid string) (int, any) {
				p, _ := e.tree.Proof(aid)
				w := p.Wire()
				w.Salt = strings.ToUpper(w.Salt)
				return 200, struct {
					liabtree.WireProof
					FromDay  uint64 `json:"from_day"`
					LedgerHW uint64 `json:"ledger_hw"`
					Record   string `json:"record"`
				}{w, e.ep.FromDay, e.hw, base64.RawURLEncoding.EncodeToString(e.raw)}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHub(t)
			f.addEpoch(today()-2, 100, 10, 5)
			f.addEpoch(today()-1, 100, 20, 5)
			tc.set(f)
			m, _ := f.hubbed()
			rep := mustAudit(t, m)
			if rep.Verified || len(rep.Findings) == 0 {
				t.Fatalf("an invalid proof passed: %+v", rep)
			}
			for _, fd := range rep.Findings {
				if fd.Code != "proof_invalid" || fd.Args["code"] != tc.code {
					t.Errorf("finding %+v, want proof_invalid / %s", fd, tc.code)
				}
			}
		})
	}
}

// root_sum_mismatch: the root's sum is not the outstanding the record
// states, though the path is sound.
func TestARootWhoseSumIsNotTheRecordsOutstandingIsInvalid(t *testing.T) {
	f := newFakeHub(t)
	e := f.addEpoch(today()-1, 100, 10, 5)
	e.ep.Outstanding++
	f.epochs = nil
	f.sign(e, time.Now().UnixMilli())
	f.epochs = append(f.epochs, e)
	m, _ := f.hubbed()
	rep := mustAudit(t, m)
	// The record is signed, so the chain verifies; the proof does not
	// reach its outstanding.
	if rep.Verified || len(rep.Findings) != 1 || rep.Findings[0].Args["code"] != "root_sum_mismatch" {
		t.Fatalf("findings: %+v", rep.Findings)
	}
}

// proof_unavailable is a finding only for a period the node had an account
// in; before its registration the hub rightly has no leaf.
func TestAWithheldProofIsAFindingOnlyAfterRegistration(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	f.addEpoch(d-2, 100, 10)
	f.addEpoch(d-1, 100, 20)
	f.unavailable[d-2], f.unavailable[d-1] = true, true
	m, h := f.hubbed()
	// Registered between the two closes: the first epoch is before it.
	h.registered = int64(d-1)*dayMillis + 3600_000
	rep := mustAudit(t, m)
	if got := codes(rep); len(got) != 1 || got[0] != "proof_unavailable" {
		t.Fatalf("findings: %v", got)
	}
	// Registration unknown and nothing checked before: not alleged.
	f2 := newFakeHub(t)
	f2.addEpoch(d-1, 100, 20)
	f2.unavailable[d-1] = true
	m2, _ := f2.hubbed()
	if rep := mustAudit(t, m2); len(rep.Findings) != 0 {
		t.Errorf("a withheld proof alleged against a node with no history: %+v", rep.Findings)
	}
}

// proof_expired: periods the hub no longer keeps a leaf for, which a node
// that checked before could not check in time; told once, and checking
// resumes after them.
func TestPeriodsThatWentUncheckedAreToldOnce(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	long := time.Now().Add(-20 * 24 * time.Hour).UnixMilli()
	f.addEpochAt(d-24, 100, 10, long)
	f.addEpochAt(d-23, 100, 20, long)
	f.addEpochAt(d-22, 100, 30, long)
	m, _ := f.hubbed()
	// A first run on a node that has no history alleges nothing.
	if rep := mustAudit(t, m); len(rep.Findings) != 0 {
		t.Fatalf("a node with no history was told of expired periods: %+v", rep.Findings)
	}
	// A node that checked before (a row exists) and was away while three
	// periods aged out, then finds a fresh one.
	f.addEpoch(d-1, 100, 40)
	if err := m.liab.put(LiabProof{Hub: f.ctrl.AID(), ToDay: d - 30, Result: resultOK, Balance: 100}); err != nil {
		t.Fatal(err)
	}
	m.liab = newLiabStore("")
	_ = m.liab.put(LiabProof{Hub: f.ctrl.AID(), ToDay: d - 30, Result: resultOK, Balance: 100})
	rep := mustAudit(t, m)
	var exp *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Code == "proof_expired" {
			exp = &rep.Findings[i]
		}
	}
	if exp == nil || exp.Level != levelWarn || exp.Args["n"] != 3 {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	if !rep.Verified {
		t.Errorf("a hint made the audit fail: %+v", rep)
	}
	if rep.Checked != 1 || rep.Latest == nil || rep.Latest.ToDay != d-1 {
		t.Errorf("checking did not resume: %+v", rep)
	}
	if again := mustAudit(t, m); len(again.Findings) != 0 {
		t.Errorf("told twice: %+v", again.Findings)
	}
}

// omitted_from_ledger and unknown_debit (PAY-S9 step 4).
func TestTheLedgerAgainstThisNodesOwnRecord(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	in := (int64(d-1)*dayMillis + 12*3600_000)
	f.entries = []ledgerEntry{
		{Seq: 5, Delta: 100, Reason: "operator grant", At: stamp(in - 3600_000)},
		{Seq: 6, Delta: -7, Reason: "auth-mine", At: stamp(in)},
		{Seq: 7, Delta: -9, Reason: "auth-never-signed", At: stamp(in)},
	}
	f.addEpoch(d-1, 84, 10)
	m, h := f.hubbed()
	_ = h.RecordEvidence(EvPaymentAuthorized, map[string]any{"authorization_id": "auth-mine", "amount": 7})
	// A settlement the hub signed a receipt for, in the ledger; and one that is not.
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "auth-mine", "verified": true, "amount": 7,
		"_recorded_at": in})
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "tx-gone", "verified": true, "amount": 12,
		"_recorded_at": in})
	// An unverified claim of a provider is not the hub's to answer for.
	_ = h.RecordEvidence(EvPaymentSettled, map[string]any{"transaction": "tx-claim", "verified": false, "amount": 3,
		"_recorded_at": in})
	rep := mustAudit(t, m)
	got := map[string]any{}
	for _, fd := range rep.Findings {
		got[fd.Code] = fd.Args["amount"]
	}
	if len(rep.Findings) != 2 || got["omitted_from_ledger"] != int64(12) || got["unknown_debit"] != int64(9) {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	if rep.Verified {
		t.Error("findings, and the audit is verified")
	}
}

// A page that does not reach back to the previous close cannot show the
// entries between: told as a hint, never as a mismatch.
func TestContinuityThePageCannotShowIsOnlyAHint(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	for i := uint64(1); i <= 6; i++ {
		f.entries = append(f.entries, ledgerEntry{Seq: 10 + i, Delta: 10, Reason: "tx", At: stamp(time.Now().Add(-30 * time.Hour).UnixMilli())})
	}
	f.page = 2
	f.addEpoch(d-2, 100, 10)
	f.addEpoch(d-1, 160, 30)
	m, _ := f.hubbed()
	rep := mustAudit(t, m)
	if !rep.Verified || len(rep.Findings) != 0 {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	if len(rep.Notes) != 1 || rep.Notes[0] != "continuity_unverifiable" {
		t.Errorf("notes: %v", rep.Notes)
	}
}

// --- The chain ---

func TestAChainWithAnEntryDeletedFromTheMiddleIsRefused(t *testing.T) {
	f := newFakeHub(t)
	for i := uint64(0); i < 4; i++ {
		f.addEpoch(today()-4+i, 10, i)
	}
	f.epochs = append(f.epochs[:1], f.epochs[2:]...)
	m, _ := f.hubbed()
	rep := mustAudit(t, m)
	if rep.Verified || rep.ChainOK || len(rep.Findings) == 0 || rep.Findings[0].Code != "proof_invalid" ||
		rep.Findings[0].Args["code"] != "chain" || !strings.Contains(rep.Findings[0].Detail, "missing") {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	if rep.ProofsOK != 0 || rep.Checked != 0 {
		t.Errorf("proofs were checked against an unsound chain: %+v", rep)
	}
}

func TestAChainSignedBySomebodyElseIsRefused(t *testing.T) {
	f := newFakeHub(t)
	f.addEpoch(today()-1, 10, 1)
	other := newFakeHub(t)
	other.addEpoch(today()-1, 10, 1)
	// Served under this hub's name, signed by another key.
	f.epochs[0] = other.epochs[0]
	m, _ := f.hubbed()
	rep := mustAudit(t, m)
	if rep.Verified || rep.ChainOK || len(rep.Findings) == 0 {
		t.Fatalf("a forged chain verified: %+v", rep)
	}
}

func TestAHubThatRewroteItsHistoryContradictsWhatWasRecorded(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	f.addEpoch(d-2, 10, 1)
	f.addEpoch(d-1, 10, 2)
	m, h := f.hubbed()
	if rep := mustAudit(t, m); !rep.Verified {
		t.Fatalf("honest: %+v", rep)
	}
	// The hub re-signs the last epoch differently: a fork of its own chain.
	last := f.epochs[1]
	last.ep.Outstanding += 5
	f.epochs = f.epochs[:1]
	f.sign(last, time.Now().UnixMilli()+1)
	f.epochs = append(f.epochs, last)
	rep := mustAudit(t, m)
	if rep.Verified || rep.ChainOK {
		t.Fatalf("a rewritten history verified: %+v", rep)
	}
	found := false
	for _, fd := range rep.Findings {
		if strings.Contains(fd.Detail, "rewrote") {
			found = true
		}
	}
	if !found {
		t.Errorf("no finding says the hub rewrote it: %+v", rep.Findings)
	}
	// And a hub that drops the epoch this node saw.
	f.epochs = f.epochs[:1]
	if rep := mustAudit(t, m); rep.Verified {
		t.Errorf("a dropped head passed: %+v", rep)
	}
	_ = h
}

// A hub that does not advance its pages is not followed for ever.
func TestAHubThatRepeatsAPageEndsTheRead(t *testing.T) {
	f := newFakeHub(t)
	f.addEpoch(today()-1, 10, 1)
	m, _ := f.hubbed()
	chain, err := m.fetchChain(context.Background())
	if err != nil || len(chain) != 1 {
		t.Fatalf("chain %d, %v", len(chain), err)
	}
}

// --- RC1 on the module's requests ---

// A hub that answers ts_stale is obeyed once: the node raises its clock for
// that hub above the floor and the request is signed again.
func TestAStaleTimeIsSignedAgainOnce(t *testing.T) {
	f := newFakeHub(t)
	f.addEpoch(today()-1, 100, 10)
	m, h := f.hubbed()
	f.staleFloor = time.Now().UnixMilli() + 20_000
	rep := mustAudit(t, m)
	if !rep.Verified || rep.Checked != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if f.stale == 0 || h.stales == 0 {
		t.Errorf("stale answers %d, reported to the node %d", f.stale, h.stales)
	}
}

// A floor beyond what the other hubs accept is not adopted: the hub is
// unreachable for the request, and the error says the clock is wrong.
func TestAFloorBeyondTheSkewIsNotAdopted(t *testing.T) {
	f := newFakeHub(t)
	f.addEpoch(today()-1, 100, 10)
	m, _ := f.hubbed()
	f.staleFloor = time.Now().UnixMilli() + 3_600_000
	_, err := m.audit(context.Background())
	if err == nil || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("err = %v", err)
	}
}

// A hub that rotated after its history was pinned signs its newer epochs
// under a key state the pinned history does not reach: the node reads the
// history again once, and those epochs verify (CORE-L8, KEL H-34). The
// earlier epoch stays valid under the key of its time.
func TestAnEpochSignedAfterTheHubRotatedVerifiesAfterOneRefresh(t *testing.T) {
	f := newFakeHub(t)
	d := today()
	f.addEpoch(d-2, 100, 10)
	m, h := f.hubbed()
	h.pinnedKEL = f.ctrl.KEL() // what the node pinned
	rot := uint64(time.Now().UnixMilli() + 1000)
	if err := identitytest.Rotate(f.ctrl, rot); err != nil {
		t.Fatal(err)
	}
	f.addEpochAt(d-1, 100, 20, int64(rot)+1000)
	rep := mustAudit(t, m)
	if !rep.Verified || !rep.ChainOK || rep.Epochs != 2 {
		t.Fatalf("after a rotation: %+v", rep)
	}
	// A forged record is not rescued by the refresh.
	if h.pinnedKEL != nil {
		t.Error("the node did not read the hub's history again")
	}
}
