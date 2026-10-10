package daemon

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
)

// kelSeed is the recovery-derived key seed a test reveals for seq n: the key
// event n-1 committed (the witness designation commits kelSeed(2), rot j
// commits kelSeed(j+1)).
func kelSeed(n int) []byte {
	h := sha256.Sum256([]byte(fmt.Sprintf("anet-test/kel-9/seed/%d", n)))
	return h[:]
}

// kelDigest is the next-key digest of kelSeed(n): SHA-256 of its public key.
func kelDigest(n int) []byte {
	k := ed25519.NewKeyFromSeed(kelSeed(n))
	h := sha256.Sum256(k.Public().(ed25519.PublicKey))
	return h[:]
}

// kelHistory is an agent identity built step by step, and its identity file
// after each event: files[i] holds a KEL of i+1 events.
type kelHistory struct {
	c     *identity.Controller
	files [][]byte
	hub   string
}

// newKELHistory incepts an agent at ts 1000, designates hub its witness
// (committing kelSeed(2)) and makes rots planned rotations after it.
func newKELHistory(t testing.TB, hub string, rots int) *kelHistory {
	t.Helper()
	c, err := identity.InceptAgent(1000)
	if err != nil {
		t.Fatal(err)
	}
	h := &kelHistory{c: c, hub: hub}
	h.keep(t)
	ev, err := c.BuildWitnessDesignation(hub, kelDigest(2), 2000)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	h.keep(t)
	for i := 0; i < rots; i++ {
		h.rotate(t)
	}
	return h
}

func (h *kelHistory) keep(t testing.TB) {
	t.Helper()
	b, err := h.c.Export()
	if err != nil {
		t.Fatal(err)
	}
	h.files = append(h.files, b)
}

// rotate adds a planned rot revealing kelSeed(j) at seq j.
func (h *kelHistory) rotate(t testing.TB) {
	t.Helper()
	j := len(h.c.Accepted())
	ev, err := h.c.BuildRotation(ed25519.NewKeyFromSeed(kelSeed(j)), kelDigest(j+1), h.hub, false, uint64(1000*(j+1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	h.keep(t)
}

// at is a controller holding the first n events (a copy).
func (h *kelHistory) at(t testing.TB, n int) *identity.Controller {
	t.Helper()
	c, err := identity.Restore(h.files[n-1])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// forkAt is a valid KEL of the same identity that agrees with this one up
// to seq j-1 and differs at seq j (j ≥ 1): made by the holder of the key
// event j-1 committed, with another commitment and time.
func (h *kelHistory) forkAt(t testing.TB, j int) []identity.SignedEvent {
	t.Helper()
	c := h.at(t, j)
	var ev identity.SignedEvent
	var err error
	if j == 1 {
		ev, err = c.BuildWitnessDesignation(h.hub, kelDigest(901), 2500)
	} else {
		ev, err = c.BuildRotation(ed25519.NewKeyFromSeed(kelSeed(j)), kelDigest(900+j), h.hub, false, uint64(1000*(j+1)+500))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	return c.Accepted()
}

// extended is a valid KEL that extends this one by a planned rot of the
// holder of the next key, with another commitment than the pending event
// a test may hold.
func (h *kelHistory) extended(t testing.TB) []identity.SignedEvent {
	t.Helper()
	c := h.at(t, len(h.files))
	j := len(c.Accepted())
	ev, err := c.BuildRotation(ed25519.NewKeyFromSeed(kelSeed(j)), kelDigest(800+j), h.hub, false, uint64(1000*(j+1)+700))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	return c.Accepted()
}

// pendingRot builds, on a copy, the planned rot this identity would publish
// next, and returns it.
func (h *kelHistory) pendingRot(t testing.TB) identity.SignedEvent {
	t.Helper()
	c := h.at(t, len(h.files))
	j := len(c.Accepted())
	ev, err := c.BuildRotation(ed25519.NewKeyFromSeed(kelSeed(j)), kelDigest(j+1), h.hub, false, uint64(1000*(j+1)+300))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// forged is a longer KEL whose last event's signature is broken.
func forged(kel []identity.SignedEvent) []identity.SignedEvent {
	out := append([]identity.SignedEvent(nil), kel...)
	last := out[len(out)-1]
	sig := append([]byte(nil), last.Sig...)
	sig[0] ^= 1
	last.Sig = sig
	out[len(out)-1] = last
	return out
}

// KEL D-27: classifyHubKEL, row by row.
func TestClassifyHubKELRows(t *testing.T) {
	hub := identitytest.Witness
	h := newKELHistory(t, hub, 2) // icp, designation, rot 2, rot 3
	local := h.c.Accepted()
	pending := h.pendingRot(t)
	other := newKELHistory(t, hub, 1).c.Accepted()
	for _, c := range []struct {
		name    string
		pending *identity.SignedEvent
		hub     []identity.SignedEvent
		want    HubKELVerdict
	}{
		{"a: nothing", nil, nil, hubKELInvalid},
		{"a: another identity's valid KEL", nil, other, hubKELInvalid},
		{"a: a longer KEL with a broken signature", nil, forged(h.extended(t)), hubKELInvalid},
		{"a: a broken prefix", nil, forged(local[:2]), hubKELInvalid},
		{"b: local and the pending rot", &pending, append(append([]identity.SignedEvent(nil), local...), pending), hubKELAccepted},
		{"c: a strict prefix", nil, local[:2], hubKELBehind},
		{"c: the icp alone", nil, local[:1], hubKELBehind},
		{"c: a strict prefix with a pending rot", &pending, local[:3], hubKELBehind},
		{"d: the same", nil, local, hubKELSame},
		{"d: the same with a pending rot", &pending, local, hubKELSame},
		{"e: an extension made elsewhere", nil, h.extended(t), hubKELNewer},
		{"e: an extension other than the pending rot", &pending, h.extended(t), hubKELNewer},
		{"f: a fork at the designation", nil, h.forkAt(t, 1), hubKELFork},
		{"f: a fork at seq 2", nil, h.forkAt(t, 2), hubKELFork},
		{"f: a fork at the tip", nil, h.forkAt(t, 3), hubKELFork},
	} {
		if got := classifyHubKEL(local, c.pending, c.hub); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// P9 (KEL §7.2): over random local KELs, pending events and candidates, the
// verdict is newer or fork exactly when the candidate replays to the local
// AID and extends it (other than the pending event) or forks from it at
// seq 1 or later. Run with -fuzz for the property's ten minutes.
func FuzzClassifyHubKEL(f *testing.F) {
	for _, s := range [][]byte{{0, 0, 0, 0}, {1, 1, 2, 0}, {2, 0, 3, 1}, {3, 1, 4, 2}, {1, 0, 5, 0}, {0, 1, 6, 0}, {2, 1, 7, 9}} {
		f.Add(s)
	}
	hub := identitytest.Witness
	histories := map[int]*kelHistory{}
	other := newKELHistory(f, hub, 1).c.Accepted()
	f.Fuzz(func(t *testing.T, data []byte) {
		at := func(i int) int {
			if i < len(data) {
				return int(data[i])
			}
			return 0
		}
		rots := at(0) % 3
		h := histories[rots]
		if h == nil {
			h = newKELHistory(t, hub, rots)
			histories[rots] = h
		}
		local := h.c.Accepted()
		var pending *identity.SignedEvent
		if at(1)%2 == 1 {
			p := h.pendingRot(t)
			pending = &p
		}
		var cand []identity.SignedEvent
		stops := false
		switch at(2) % 8 {
		case 0: // a prefix (or the whole)
			n := 1 + at(3)%len(local)
			cand = local[:n]
		case 1: // the same
			cand = local
		case 2: // an extension made elsewhere
			cand, stops = h.extended(t), true
		case 3: // a fork at seq j ≥ 1
			j := 1 + at(3)%(len(local)-1)
			cand, stops = h.forkAt(t, j), true
		case 4: // another identity
			cand = other
		case 5: // a forged extension
			cand = forged(h.extended(t))
		case 6: // bytes that are not a KEL
			cand, _ = identity.UnmarshalKEL(data)
		case 7: // local and the pending event
			if pending != nil {
				cand = append(append([]identity.SignedEvent(nil), local...), *pending)
			} else {
				cand = local
			}
		}
		v := classifyHubKEL(local, pending, cand)
		if v.stops() != stops {
			t.Fatalf("candidate kind %d: verdict %s, stops %v, want %v", at(2)%8, v, v.stops(), stops)
		}
		if _, err := identity.Replay(cand); err != nil && v != hubKELInvalid {
			t.Fatalf("a candidate that does not replay is %s", v)
		}
	})
}
