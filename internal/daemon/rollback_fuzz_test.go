package daemon

import (
	"bytes"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
)

// FuzzSignedElsewhere feeds signedElsewhere — the check of the bytes a
// hub's directory entry holds as this identity's key set (FS-B2) — any
// bytes and any stored_seq. It never panics, and it finds a set signed
// elsewhere only when the bytes are a set this identity signed, at
// stored_seq, above every set the ring signed before its current one, and
// not the ring's current set: a hub cannot make anything else end a task.
func FuzzSignedElsewhere(f *testing.F) {
	now := uint64(time.Now().UnixMilli())
	c, err := identity.InceptAgent(now - 60_000)
	if err != nil {
		f.Fatal(err)
	}
	aid, kel := c.AID(), c.KEL()
	sign := func(seq uint64) []byte {
		e := epochStart(now)
		var keys []seal.EncKey
		for i := uint64(0); i < 2; i++ {
			kp, err := seal.GenerateKeyPair(seal.SuiteX25519, e+i*seal.EpochMS, e+(i+1)*seal.EpochMS)
			if err != nil {
				f.Fatal(err)
			}
			keys = append(keys, kp.Public)
		}
		set := &seal.EncKeySet{Type: seal.EncKeySetType, AID: aid, Seq: seq, Keys: keys, IssuedAt: now,
			Mbox: seal.Mbox{Epoch: 1, Pub: bytes.Repeat([]byte{7}, 32)}}
		signed, err := seal.SignEncKeySet(set, c.Sign)
		if err != nil {
			f.Fatal(err)
		}
		b, err := signed.Marshal()
		if err != nil {
			f.Fatal(err)
		}
		return b
	}
	const ownPrev, last = 1000, 2000
	ours, elsewhere := sign(last), sign(1500)
	f.Add(elsewhere, uint64(1500))
	f.Add(ours, uint64(last))
	f.Add(sign(900), uint64(900))
	f.Add(sign(3000), uint64(3000))
	f.Add([]byte{0xa3, 0x01, 0x40, 0x02, 0x00, 0x03, 0x40}, uint64(0))
	f.Add([]byte{}, uint64(1500))
	f.Fuzz(func(t *testing.T, b []byte, stored uint64) {
		if signedElsewhere(b, aid, kel, stored, ours, last, ownPrev) != nil {
			return
		}
		s, err := seal.UnmarshalSignedEncKeySet(b)
		if err != nil {
			t.Fatalf("taken, but does not decode: %v", err)
		}
		var head seal.EncKeySet
		if err := coredet.Unmarshal(s.Set, &head); err != nil {
			t.Fatalf("taken, but its set does not decode: %v", err)
		}
		set, err := seal.VerifyEncKeySet(s, aid, kel, head.IssuedAt)
		if err != nil {
			t.Fatalf("taken, but it does not verify: %v", err)
		}
		switch {
		case set.Seq != stored:
			t.Fatalf("taken at seq %d for stored_seq %d", set.Seq, stored)
		case stored <= ownPrev:
			t.Fatalf("taken at %d, at or below the ring's earlier sets (%d)", stored, ownPrev)
		case stored == last && sameSetContent(ours, s.Set):
			t.Fatal("the ring's own current set taken as signed elsewhere")
		}
	})
}
