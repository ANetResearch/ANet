package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
)

// P-ENG-6 (engine §8.1): rxTokenFor gives every row of engine §4.2 and §4.5
// the token the tables say — none, tok(1), or tok(2) with its note — and
// none at all to a one-shot envelope, to one whose first layer did not open,
// and to a temporary failure.
func TestRxTokenForTable(t *testing.T) {
	tok1 := tokenDecision{Make: true, Outcome: seal.OutcomeProcessed}
	tok2 := func(reason string, retry uint32) tokenDecision {
		return tokenDecision{Make: true, Outcome: seal.OutcomeRefused, Note: seal.Note{Reason: reason, RetryAfterS: retry}}
	}
	none := tokenDecision{}
	p := func(step rxStep, reason string) rxVerdict {
		return rxVerdict{Class: rxDropped, Step: step, Reason: reason, Layer1: true}
	}
	noted := func(step rxStep, reason string, n seal.Note) rxVerdict {
		v := p(step, reason)
		v.Note = n
		return v
	}
	cases := []struct {
		name string
		v    rxVerdict
		want tokenDecision
	}{
		{"accepted at step 10", rxVerdict{Class: rxAccepted, Step: rxStep10, Layer1: true}, tok1},
		// Before the first layer (ENG-V4), temporary (ENG-V9), one-shot (ENG-V6).
		{"step 1 outer", rxVerdict{Class: rxDropped, Step: rxStep1, Reason: seal.ReasonBadOuter}, none},
		{"step 2 unknown key", rxVerdict{Class: rxDropped, Step: rxStep2, Reason: seal.ReasonUnknownKey}, none},
		{"step 3 first layer", rxVerdict{Class: rxDropped, Step: rxStep3, Reason: seal.ReasonDecrypt}, none},
		{"step 0 rate limit", rxVerdict{Class: rxTransient, Step: rxStep0, Reason: transientP2PRate}, none},
		{"store error", rxVerdict{Class: rxTransient, Step: rxStep10, Reason: transientStore, Layer1: true}, none},
		{"ixLookup store error", rxVerdict{Class: rxTransient, Step: rxStep3a, Reason: transientStore, Layer1: true}, none},
		{"witness-unknown", rxVerdict{Class: rxTransient, Step: rxStep6b, Reason: seal.ReasonWitnessUnknown, Layer1: true}, none},
		{"one-shot accepted", rxVerdict{Class: rxAccepted, Step: rxStep9, Layer1: true, OneShot: true}, none},
		{"one-shot refused", rxVerdict{Class: rxDropped, Step: rxStep4a, Reason: dropBadKindKeys, Layer1: true, OneShot: true}, none},
		{"one-shot replayed", rxVerdict{Class: rxDropped, Step: rxStep5a, Reason: dropRefusedReplay, Layer1: true, OneShot: true}, none},
		// Step 3a.
		{"bad-wrap", p(rxStep3a, seal.ReasonBadWrap), tok2(seal.NoteInvalid, 0)},
		{"second layer decrypt-failed", p(rxStep3a, seal.ReasonDecrypt), tok2(seal.NoteInvalid, 0)},
		{"sealed-to-closed-interaction", rxVerdict{Class: rxAccepted, Step: rxStep3a, Reason: seal.ReasonSealedToClosedInteraction, Layer1: true}, tok1},
		// Step 4.
		{"bad-inner", p(rxStep4, seal.ReasonBadInner), tok2(seal.NoteInvalid, 0)},
		{"unknown-critical-field", p(rxStep4, seal.ReasonUnknownField), tok2(seal.NoteInvalid, 0)},
		{"to-mismatch", p(rxStep4, seal.ReasonToMismatch), tok2(seal.NoteInvalid, 0)},
		{"exp-mismatch", p(rxStep4, seal.ReasonExpMismatch), tok2(seal.NoteInvalid, 0)},
		{"bad-rk", p(rxStep4, seal.ReasonBadRK), tok2(seal.NoteInvalid, 0)},
		{"ikey-mismatch", p(rxStep4, seal.ReasonIKeyMismatch), tok2(seal.NoteInvalid, 0)},
		{"kel-too-large", p(rxStep4, seal.ReasonKELTooLarge), tok2(seal.NoteInvalid, 0)},
		// Step 4a.
		{"bad-kind-keys", p(rxStep4a, dropBadKindKeys), tok2(seal.NoteInvalid, 0)},
		// Step 5.
		{"expired", p(rxStep5, seal.ReasonExpired), tok2(seal.NoteInvalid, 0)},
		{"from-future", p(rxStep5, seal.ReasonFromFuture), tok2(seal.NoteInvalid, 0)},
		{"bad-lifetime", p(rxStep5, seal.ReasonBadLifetime), tok2(seal.NoteInvalid, 0)},
		// Steps 5a and 8½.
		{"refused-replay keeps the reason, not the wait", noted(rxStep5a, dropRefusedReplay,
			seal.Note{Reason: seal.NoteQuotaCallerPerMin, RetryAfterS: 60}), tok2(seal.NoteQuotaCallerPerMin, 0)},
		{"refused-replay of an empty reason", noted(rxStep8b, dropRefusedReplay, seal.Note{}), tok2("", 0)},
		{"duplicate", p(rxStep8b, dropDuplicate), tok1},
		{"refused-floor", p(rxStep8b, dropRefusedFloor), tok2("", 0)},
		// Step 6.
		{"kel-fork", p(rxStep6, dropKELFork), tok2(seal.NoteInvalid, 0)},
		{"bad-kel", p(rxStep6, seal.ReasonBadKEL), tok2(seal.NoteInvalid, 0)},
		{"kel-from-future", p(rxStep6, seal.ReasonKELFromFuture), tok2(seal.NoteKeyState, 0)},
		// Step 6½.
		{"witness-missing", p(rxStep6b, seal.ReasonWitnessMissing), tok2(seal.NoteKeyState, 0)},
		{"witness-stale", p(rxStep6b, seal.ReasonWitnessStale), tok2(seal.NoteKeyState, 0)},
		{"witness-mismatch", p(rxStep6b, seal.ReasonWitnessMismatch), tok2(seal.NoteKeyState, 0)},
		{"witness-invalid", p(rxStep6b, seal.ReasonWitnessInvalid), tok2(seal.NoteKeyState, 0)},
		// Step 7.
		{"bad-sig", p(rxStep7, seal.ReasonBadSig), tok2(seal.NoteInvalid, 0)},
		{"from-mismatch", p(rxStep7, seal.ReasonFromMismatch), tok2(seal.NoteInvalid, 0)},
		{"bad-key-state", p(rxStep7, seal.ReasonBadKeyState), tok2(seal.NoteInvalid, 0)},
		{"revoked-key", p(rxStep7, seal.ReasonRevokedKey), tok2(seal.NoteKeyState, 0)},
		{"rotation-grace-expired", p(rxStep7, seal.ReasonGraceExpired), tok2(seal.NoteKeyState, 0)},
		// Step 9: the policy and admission, with noteReasonFor's note.
		{"not accepting", noted(rxStep9, dropNotAccepting, seal.Note{Reason: seal.NoteNotAccepting}), tok2(seal.NoteNotAccepting, 0)},
		{"admission with a wait", noted(rxStep9, dropRefusedPrefix+admitCallerMinute,
			seal.Note{Reason: seal.NoteQuotaCallerPerMin, RetryAfterS: 60}), tok2(seal.NoteQuotaCallerPerMin, 60)},
		{"pending_full", noted(rxStep10, dropRefusedPrefix+reasonPendingFull, seal.Note{Reason: seal.NotePendingFull}), tok2(seal.NotePendingFull, 0)},
		{"a note that is not a code", noted(rxStep9, dropNotAccepting, seal.Note{Reason: "Not Accepting"}), tok2("", 0)},
		{"canceled (tombstone)", p(rxStep9, dropCanceled), tok2(seal.NoteCanceled, 0)},
		{"ix-collision", p(rxStep9, dropIXCollision), tok2("", 0)},
		{"pending-collision", p(rxStep9, dropPendingCollision), tok2("", 0)},
		{"unknown interaction", p(rxStep9, dropUnknownIX), tok2(seal.NoteTaskNotFound, 0)},
		{"after the end", p(rxStep9, dropAfterTerminal), tok2(seal.NoteTaskEnded, 0)},
		{"rk-mismatch", p(rxStep9, dropRKMismatch), tok2(seal.NoteInvalid, 0)},
		{"bad-body", p(rxStep9, dropBadBody), tok2(seal.NoteInvalid, 0)},
		{"bad-taskdoc", p(rxStep9, dropBadTaskDoc), tok2(seal.NoteInvalid, 0)},
		{"signer-mismatch", p(rxStep9, dropSignerIsOther), tok2(seal.NoteInvalid, 0)},
		{"ix-mismatch", p(rxStep9, dropIXMismatch), tok2(seal.NoteInvalid, 0)},
		{"unknown-type", p(rxStep9, dropUnknownType), tok2(seal.NoteInvalid, 0)},
		{"bad-status-state", p(rxStep9, dropBadStatus), tok2(seal.NoteInvalid, 0)},
		{"empty-ix", p(rxStep9, dropEmptyIX), tok2(seal.NoteInvalid, 0)},
		{"result-refused", p(rxStep10, dropResultRefused), tok2(seal.NoteInvalid, 0)},
		{"reply-mismatch", p(rxStep10, dropReplyMismatch), tok2(seal.NoteInvalid, 0)},
		{"not-peer", p(rxStep9, dropNotPeer), tok2("", 0)},
		{"not-allowed", p(rxStep9, dropNotAllowed), tok2("", 0)},
		{"public-cap-text", p(rxStep9, dropPublicCapText), tok2("", 0)},
		{"cancel-from-provider", p(rxStep9, dropCancelFromProvider), tok2("", 0)},
		{"wrong-role", p(rxStep9, dropWrongRole), tok2("", 0)},
		{"denied", p(rxStep9, dropDenied), tok2("", 0)},
		{"end_request from the provider", p(rxStep9, dropEndRequestIgnored), tok2("", 0)},
		{"pending-followups-full", p(rxStep10, dropPendingFollowupsFull), tok2("", 0)},
		// ENG-V5: a temporary failure that outlived its message.
		{"expired-while-waiting", p(rxStep5, dropExpiredWait), tok2(seal.NoteInvalid, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rxTokenFor(tc.v); got != tc.want {
				t.Fatalf("rxTokenFor(%+v) = %+v, want %+v", tc.v, got, tc.want)
			}
		})
	}
}

// P-ENG-6: every copy of an envelope the replay table holds is answered
// tok(1), and every one-shot envelope is answered nothing, whatever else the
// verdict says (engine ENG-V6, V13).
func TestRxTokenForReplayAndOneShotProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	reasons := []string{dropDuplicate, dropRefusedReplay, dropUnknownIX, seal.ReasonBadSig, dropNotAccepting, ""}
	for i := 0; i < 5000; i++ {
		v := rxVerdict{Class: rxClass(1 + r.IntN(3)), Step: rxStep(r.IntN(int(rxStep10) + 1)),
			Reason: reasons[r.IntN(len(reasons))], OneShot: r.IntN(2) == 0, Layer1: r.IntN(2) == 0,
			Note: seal.Note{Reason: seal.NoteBusy, RetryAfterS: uint32(r.IntN(100))}}
		got := rxTokenFor(v)
		switch {
		case v.OneShot || !v.Layer1 || v.Class == rxTransient:
			if got.Make {
				t.Fatalf("%+v answered %+v; want none", v, got)
			}
		case v.Reason == dropDuplicate || v.Class == rxAccepted:
			if got != (tokenDecision{Make: true, Outcome: seal.OutcomeProcessed}) {
				t.Fatalf("%+v answered %+v; want tok(1)", v, got)
			}
		case v.Reason == dropRefusedReplay:
			if !got.Make || got.Outcome != seal.OutcomeRefused || got.Note.RetryAfterS != 0 {
				t.Fatalf("%+v answered %+v; want tok(2) without a wait", v, got)
			}
		}
		if got.Make && got.Outcome == seal.OutcomeProcessed && got.Note != (seal.Note{}) {
			t.Fatalf("tok(1) with a note: %+v", got)
		}
	}
}

// Every reason the pipeline refuses with has its row in the token table
// (engine ENG-V11: the table test covers every counter reason, DLV P5). A
// call d.drop(X, …) or rxPermanent(X, …) in the package names its reason by
// a constant; each one is in rxTokenRules or is a policy refusal's
// refused-<code>, except those of a step before the first layer opens or of
// a one-shot envelope, which no token answers whatever the table says.
func TestEveryReceiveReasonHasATokenRule(t *testing.T) {
	noToken := map[string]bool{
		// The envelope bytes did not decode: before step 1.
		dropBadTransport: true,
		// One-shot envelopes.
		previewRejected: true, cardGetNoGrant: true, cardRejected: true,
	}
	consts := map[string]string{}
	var calls []*ast.CallExpr
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.ValueSpec:
				for i, id := range x.Names {
					if i < len(x.Values) {
						if lit, ok := x.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							if v, err := strconv.Unquote(lit.Value); err == nil {
								consts[id.Name] = v
							}
						}
					}
				}
			case *ast.CallExpr:
				name := ""
				switch fn := x.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				case *ast.Ident:
					name = fn.Name
				}
				if (name == "drop" || name == "rxPermanent") && len(x.Args) == 2 {
					calls = append(calls, x)
				}
			}
			return true
		})
	}
	if len(calls) < 40 {
		t.Fatalf("found %d refusals in the package; the scan is broken", len(calls))
	}
	for _, c := range calls {
		var reason string
		switch a := c.Args[0].(type) {
		case *ast.Ident:
			v, ok := consts[a.Name]
			if !ok {
				continue // a variable: a seal reason (reasonOf), checked below
			}
			reason = v
		case *ast.BasicLit:
			v, err := strconv.Unquote(a.Value)
			if err != nil {
				t.Fatal(err)
			}
			reason = v
		case *ast.SelectorExpr:
			if x, ok := a.X.(*ast.Ident); !ok || x.Name != "seal" {
				continue // a field carrying a reason given elsewhere (rxPermanentErr)
			}
			reason = sealReasons[a.Sel.Name]
			if reason == "" {
				t.Errorf("%s: seal.%s is not in this test's list", fset.Position(c.Pos()), a.Sel.Name)
				continue
			}
		case *ast.BinaryExpr: // dropRefusedPrefix + code
			continue
		default: // reasonOf(err): a seal reason, below
			continue
		}
		if noToken[reason] {
			continue
		}
		if _, ok := rxTokenRules[reason]; !ok && !strings.HasPrefix(reason, dropRefusedPrefix) &&
			!strings.HasPrefix(reason, "t-") {
			t.Errorf("%s: %q has no row in rxTokenRules", fset.Position(c.Pos()), reason)
		}
	}
	// The seal reasons a step after the first layer gives through reasonOf.
	for _, r := range []string{seal.ReasonBadWrap, seal.ReasonDecrypt, seal.ReasonBadInner, seal.ReasonUnknownField,
		seal.ReasonToMismatch, seal.ReasonExpMismatch, seal.ReasonKELTooLarge, seal.ReasonBadKEL, seal.ReasonBadRK,
		seal.ReasonIKeyMismatch, seal.ReasonExpired, seal.ReasonFromFuture, seal.ReasonBadLifetime,
		seal.ReasonWitnessMissing, seal.ReasonWitnessStale, seal.ReasonWitnessMismatch, seal.ReasonWitnessInvalid,
		seal.ReasonBadSig, seal.ReasonFromMismatch, seal.ReasonBadKeyState, seal.ReasonRevokedKey, seal.ReasonGraceExpired,
		seal.ReasonKELFromFuture, seal.ReasonSealedToClosedInteraction} {
		if _, ok := rxTokenRules[r]; !ok {
			t.Errorf("seal reason %q has no row in rxTokenRules", r)
		}
	}
}

// sealReasons are the seal.Reason… constants the pipeline names directly.
var sealReasons = map[string]string{
	"ReasonBadKEL": seal.ReasonBadKEL, "ReasonFromMismatch": seal.ReasonFromMismatch,
	"ReasonKELFromFuture": seal.ReasonKELFromFuture, "ReasonBadInner": seal.ReasonBadInner,
	"ReasonBadRK": seal.ReasonBadRK,
}
