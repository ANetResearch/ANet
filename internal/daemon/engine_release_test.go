package daemon

// ENG-1d: the property that gates early key deletion (engine P-ENG-3,
// ENG-E3) and the release-branch check that no engine stub is left (E-T36).

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
)

// hubRow is an envelope sealed to the epoch key under test that a hub holds.
type hubRow struct {
	kid     string
	exp     uint64 // true time the hub drops it (honest hub)
	heldTil uint64 // the recipient withholds it until this true time (heldKids)
}

// pEng3Run draws one world for P-ENG-3 and reports the first true time at
// which delete said yes while a row sealed to the key, accepted and not
// expired, was still at the hub; 0 when there is none. delete is the rule
// under test: it gets the recipient's clock, the drainMark and heldKids.
func pEng3Run(rng *rand.Rand, delete func(k *ringKey, now, drainMark uint64, held kidSet) bool) uint64 {
	const (
		start = uint64(1_800_000_000_000)
		min5  = uint64(5 * 60 * 1000)
	)
	notAfter := start
	kid := []byte("kid-0123456789ab")
	k := &ringKey{Pair: seal.KeyPair{Public: seal.EncKey{KID: kid, NotBefore: notAfter - 24*hourMS, NotAfter: notAfter}}}
	// The three clocks are within five minutes of true time of each other
	// (sender and recipient against the hub's, which is the true one here).
	skew := func() int64 { return int64(rng.Uint64N(2*min5+1)) - int64(min5) }
	recSkew, hubSkew := skew(), skew()
	rec := func(t uint64) uint64 { return uint64(int64(t) + recSkew) }
	var rows []hubRow
	var drainMark uint64
	t := notAfter - 2*hourMS
	end := notAfter + seal.MaxMessageLifetimeMS + 2*hourMS
	for t < end {
		t += 1 + rng.Uint64N(40*60*1000) // up to 40 minutes
		// The hub accepts envelopes until A(k), by its own clock.
		if hubNow := uint64(int64(t) + hubSkew); hubNow <= notAfter+seal.DeliveryDeadlineMS && rng.IntN(3) == 0 {
			life := 1 + rng.Uint64N(seal.MaxMessageLifetimeMS-hourMS)
			r := hubRow{kid: string(kid), exp: t + life}
			if rng.IntN(8) == 0 {
				r.heldTil = t + rng.Uint64N(3*24*hourMS)
			}
			rows = append(rows, r)
		}
		// The honest hub drops what has expired.
		kept := rows[:0]
		for _, r := range rows {
			if r.exp > t {
				kept = append(kept, r)
			}
		}
		rows = kept
		if rng.IntN(40) == 0 { // the recipient restarts: drainMark is 0
			drainMark = 0
		}
		if rng.IntN(2) == 0 { // a poll round
			held := kidSet{}
			clean := true
			var left []hubRow
			for i, r := range rows {
				switch {
				case r.heldTil > t:
					held[r.kid] = struct{}{}
					left = append(left, r)
					clean = false
				case rng.IntN(5) == 0: // the ack request failed
					left = append(left, r)
					clean = false
				case rng.IntN(25) == 0 && i < len(rows)-1: // the round ends short: more = true
					left = append(left, rows[i:]...)
					clean = false
					rows = left
					goto polled
				}
			}
			rows = left
			if clean {
				drainMark = rec(t)
			}
		polled:
			if len(held) > 0 {
				// heldKids lasts until the next round; the decision below
				// uses the set of this one.
				if delete(k, rec(t), drainMark, held) {
					return deletedWhileHeld(rows, t)
				}
				continue
			}
		}
		if delete(k, rec(t), drainMark, nil) {
			return deletedWhileHeld(rows, t)
		}
	}
	return 0
}

// deletedWhileHeld is t when a row sealed to the key is still at the hub,
// else 0.
func deletedWhileHeld(rows []hubRow, t uint64) uint64 {
	for _, r := range rows {
		if r.exp > t {
			return t
		}
	}
	return 0
}

// P-ENG-3 (a): whatever the three clocks, the page sizes, the holds, the
// failed acks and the restarts, the rule that deletes a key never does it
// while an accepted, unexpired envelope sealed to it is at the hub. The rule
// is epochKeyDeletable as it stands; FS-5's early branch is gated by this
// test when it comes (ENG-E3).
func TestPENG3_NoAcceptedUnexpiredEnvelopeLosesItsKey(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	for run := range 300 {
		bad := pEng3Run(rng, func(k *ringKey, now, drain uint64, held kidSet) bool {
			ok, _ := epochKeyDeletable(k, now, clockFull, drain, held)
			return ok
		})
		if bad != 0 {
			t.Fatalf("run %d: the key was deleted at %d with an accepted, unexpired envelope at the hub", run, bad)
		}
	}
}

// The harness bites: a rule that goes by the clock alone (no drain, no held
// envelopes) is found out.
func TestPENG3_TheHarnessFindsAnUnsoundRule(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	found := false
	for range 300 {
		bad := pEng3Run(rng, func(k *ringKey, now, _ uint64, _ kidSet) bool {
			return now >= satAdd(lastAcceptance(k), drainMarginMS)
		})
		if bad != 0 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("a rule that deletes at A(k) + M whatever the mailbox holds was never caught")
	}
}

var todoMarker = regexp.MustCompile(`v030-todo\(([A-Za-z0-9-]+)\)`)

// E-T36: the stubs the engine defined for other packages (ENG-G3) are gone.
// A stub is marked v030-todo(<package>); the packages below have delivered,
// so no source file marks one for them. On the release branch
// (ANET_RELEASE_CHECK=1) the packages that deliver after the engine are
// checked too: nothing of the engine may be left. The preview module's
// absence behavior is not a stub (MOD-P5).
func TestNoEngineStubsAtRelease(t *testing.T) {
	delivered := map[string]bool{"ENG-1a": true, "ENG-1b": true, "ENG-1c": true, "ENG-1d": true,
		"FS-7": true, "KEL-9": true, "KEL-11": true, "UX-3": true, "UX-4": true, "UX-8": true, "PAY-11": true}
	later := map[string]bool{"RC-5": true, "FS-5": true}
	release := os.Getenv("ANET_RELEASE_CHECK") == "1"
	root := filepath.Join("..", "..")
	found := map[string][]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && (info.Name() == "vendor" || info.Name() == ".git" || info.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range todoMarker.FindAllStringSubmatch(string(b), -1) {
			found[m[1]] = append(found[m[1]], p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		switch {
		case delivered[id], later[id] && release:
			t.Errorf("v030-todo(%s) is still in %v", id, found[id])
		case later[id]:
			t.Logf("v030-todo(%s) in %v: delivered after the engine, checked on the release branch", id, found[id])
		}
	}
}
