package tsir

import (
	"encoding/hex"
	"testing"

	"github.com/ANetResearch/ANet/internal/protocol/identity"
)

// VEC-TSIR-CID-1 (tsir-spec §6.1): the typed TaskDoc preimage + CID must reproduce the
// spec's frozen values byte-for-byte. Object: {1:{1:1}, 4:[{1:"t1", 10:{3:"do x"}}]}.
func TestVEC_TSIR_CID_1(t *testing.T) {
	d := &TaskDoc{
		Version: VersionPair{Major: 1},
		Tasks:   []Task{{ID: "t1", Intent: Intent{Body: "do x"}}},
	}
	pre, err := d.CanonicalPreimage()
	if err != nil {
		t.Fatal(err)
	}
	const wantHex = "a201a101010481a2016274310aa10364646f2078"
	if got := hex.EncodeToString(pre); got != wantHex {
		t.Fatalf("preimage_hex\n got  %s\n want %s", got, wantHex)
	}
	const wantCID = "bafyreif46dhnfcecy7fapphwd5y5fqht3c454h24haarppz6g2ez44qafq"
	cid, err := d.CID()
	if err != nil {
		t.Fatal(err)
	}
	if cid != wantCID {
		t.Fatalf("CID\n got  %s\n want %s", cid, wantCID)
	}
}

// VEC-TSIR-CID-2 (tsir-spec §6.1): adding version.minor does NOT change the CID (minor is
// dropped from the preimage); the CID equals CID-1.
func TestVEC_TSIR_CID_2_minorInvariant(t *testing.T) {
	base := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "do x"}}}}
	withMinor := &TaskDoc{Version: VersionPair{Major: 1, Minor: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "do x"}}}}
	c1, _ := base.CID()
	c2, _ := withMinor.CID()
	if c1 != c2 {
		t.Fatalf("minor must not affect CID: %s vs %s", c1, c2)
	}
}

// CID-significant optionals (negative_scope, coupling_hint) DO change the CID; the envelope
// does NOT (it is excluded from the preimage).
func TestCIDSignificance(t *testing.T) {
	base := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "do x"}}}}
	c0, _ := base.CID()

	withHint := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "do x"}, CouplingHint: 2}}}
	if ch, _ := withHint.CID(); ch == c0 {
		t.Fatal("coupling_hint is CID-significant when present")
	}
	withNeg := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "do x"},
		NegativeScope: &Predicate{Op: OpScope, Scope: &ScopeClause{Verb: VerbDelete, Kind: 1, Match: ResourceMatch{Kind: MatchGlob, Val: "secrets/**"}}}}}}
	if cn, _ := withNeg.CID(); cn == c0 {
		t.Fatal("negative_scope is CID-significant when present")
	}

	// signing adds an envelope but MUST NOT change the CID.
	c, _ := identity.Incept()
	signed := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "do x"}}}}
	if err := signed.Sign(c); err != nil {
		t.Fatal(err)
	}
	if cs, _ := signed.CID(); cs != c0 {
		t.Fatalf("envelope must not affect CID: %s vs %s", cs, c0)
	}
}

// compile() is verify-before-compile and projects the load-bearing classes (§5.4).
func TestCompile(t *testing.T) {
	c, _ := identity.Incept()
	d := &TaskDoc{
		Version: VersionPair{Major: 1},
		Tasks: []Task{{
			ID:            "t1",
			Intent:        Intent{Body: "assess structural safety"},
			Accepts:       []Accept{{Type: "artifact"}},
			NegativeScope: &Predicate{Op: OpScope, Scope: &ScopeClause{Verb: VerbDelete, Kind: 1, Match: ResourceMatch{Kind: MatchGlob, Val: "secrets/**"}}},
			CouplingHint:  2,
		}},
	}
	// unsigned → compile MUST refuse (verify-before-compile).
	if _, err := Compile(d, c.KEL(), 0); err == nil {
		t.Fatal("compile of unsigned TaskDoc must fail")
	}
	if err := d.Sign(c); err != nil {
		t.Fatal(err)
	}
	res, err := Compile(d, c.KEL(), 0)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if res.TaskCID == "" || res.Intent.Body != "assess structural safety" {
		t.Fatalf("bad projection: %+v", res)
	}
	if res.CouplingHint != 2 || res.NegativeScope == nil {
		t.Fatal("compile must project coupling_hint + negative_scope")
	}
	if !res.IntentAlignEligible {
		t.Fatal("a decidable (artifact) accept ⇒ Intent-Alignment-eligible")
	}

	// qualitative-only accepts ⇒ ineligible.
	d2 := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "x"}, Accepts: []Accept{{Type: "qualitative"}}}}}
	_ = d2.Sign(c)
	res2, err := Compile(d2, c.KEL(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res2.IntentAlignEligible {
		t.Fatal("qualitative-only accepts ⇒ Intent-Alignment-ineligible")
	}

	// a malformed negative_scope predicate fails compile.
	d3 := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "x"},
		NegativeScope: &Predicate{Op: 99}}}}
	_ = d3.Sign(c)
	if _, err := Compile(d3, c.KEL(), 0); err == nil {
		t.Fatal("compile must reject a malformed predicate")
	}
}

// F10: Compile range-checks coupling_hint — a non-zero hint outside 1..4 is MALFORMED; a hint
// inside 1..4 (and the absent/0 case) compiles.
func TestCompileCouplingHintRange(t *testing.T) {
	c, _ := identity.Incept()
	mk := func(hint int) *TaskDoc {
		d := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1",
			Intent: Intent{Body: "x"}, Accepts: []Accept{{Type: "artifact"}}, CouplingHint: hint}}}
		_ = d.Sign(c)
		return d
	}
	for _, bad := range []int{5, 99, -1} {
		if _, err := Compile(mk(bad), c.KEL(), 0); err != ErrMalformed {
			t.Fatalf("F10: coupling_hint=%d must be MALFORMED, got %v", bad, err)
		}
	}
	for _, ok := range []int{0, 1, 2, 3, 4} {
		if _, err := Compile(mk(ok), c.KEL(), 0); err != nil {
			t.Fatalf("F10: coupling_hint=%d must compile, got %v", ok, err)
		}
	}
}

// F4: Verify/Compile thread msgTime into the time-aware revocation gate (§5.2). A TaskDoc
// signed under the retiring seq0 key, bound (msgTime) AFTER the rotation → REVOKED_KEY; bound
// before the rotation (grace) still verifies; msgTime=0 baseline still verifies.
func TestVerifyMsgTimeRevocation(t *testing.T) {
	c, _ := identity.Incept()
	d := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "do x"}}}}
	if err := d.Sign(c); err != nil { // signs under key_state_seq 0
		t.Fatal(err)
	}
	// rotate at T=5000: seq0 is now retired as-of 5000.
	if err := c.Rotate(5000); err != nil {
		t.Fatal(err)
	}
	// baseline (msgTime=0): grace — still verifies.
	if err := d.Verify(c.KEL(), 0); err != nil {
		t.Fatalf("F4: msgTime=0 baseline must verify (grace), got %v", err)
	}
	// bound before the rotation (msgTime=4999): grace — verifies.
	if err := d.Verify(c.KEL(), 4999); err != nil {
		t.Fatalf("F4: msgTime<rotation must verify (grace), got %v", err)
	}
	// bound at/after the rotation (msgTime=6000): REVOKED_KEY.
	err := d.Verify(c.KEL(), 6000)
	ve, ok := err.(*identity.VErr)
	if !ok || ve.Reason != "REVOKED_KEY" {
		t.Fatalf("F4: msgTime>=rotation must be REVOKED_KEY, got %v", err)
	}
	// Compile threads the same gate.
	if _, err := Compile(d, c.KEL(), 6000); err == nil {
		t.Fatal("F4: Compile must refuse a TaskDoc bound after key retirement")
	}
}

// Tampering a signed TaskDoc fails verification.
func TestTaskDocTamper(t *testing.T) {
	c, _ := identity.Incept()
	d := &TaskDoc{Version: VersionPair{Major: 1}, Tasks: []Task{{ID: "t1", Intent: Intent{Body: "do x"}}}}
	if err := d.Sign(c); err != nil {
		t.Fatal(err)
	}
	if err := d.Verify(c.KEL(), 0); err != nil {
		t.Fatalf("verify good: %v", err)
	}
	d.Tasks[0].Intent.Body = "do y"
	if err := d.Verify(c.KEL(), 0); err == nil {
		t.Fatal("tampered intent must fail verify")
	}
}
