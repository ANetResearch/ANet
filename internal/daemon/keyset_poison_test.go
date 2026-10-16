package daemon

import (
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"
)

// T-1 RT-F1: a holder of S's signing key (before S rotates) poisons the key-set high-water mark at a peer;
// after S rotates, the peer ignores every legitimate key set of S.
func TestKeySetHighWaterPoisonDoesNotSurviveARotation(t *testing.T) {
	for _, emergency := range []bool{false, true} {
		_, _, prov := registeredPair(t)
		allowPeers(t, prov)
		s := newStranger(t)
		allowPeers(t, prov, s.aid)
		now := uint64(time.Now().UnixMilli())
		// 1. S's normal message: key set seq = now
		ix1 := "rt-p1-" + time.Now().Format("150405.000000000")
		env := craft(t, s, prov, seal.TypeDelegate, ix1, delegateBody(t, s.ctrl, ix1, "hi", ""), nil)
		if r := receive(t, prov, env); r.class != rxAccepted {
			t.Fatalf("legit: %+v", r)
		}
		row, _ := prov.ix.PeerIdentity(s.aid)
		t.Logf("emergency=%v after legit: KeySetSeq=%d", emergency, row.KeySetSeq)
		// 2. the thief (same key, S not yet rotated) sends a key set with a huge seq
		const huge = uint64(1) << 62
		thief := s
		thief.keys = signedKeysFor(t, s.ctrl, huge)
		ix2 := "rt-p2-" + time.Now().Format("150405.000000000")
		env = craft(t, thief, prov, seal.TypeDelegate, ix2, delegateBody(t, s.ctrl, ix2, "hi", ""), nil)
		r := receive(t, prov, env)
		row, _ = prov.ix.PeerIdentity(s.aid)
		t.Logf("poison message: %+v ; KeySetSeq=%d", r.class, row.KeySetSeq)
		// 2b. and a card mark of the same size, as the card path would have left in the row
		if _, err := prov.ix.UpdatePeerCardMark(s.aid, func(uint64, []byte) (uint64, []byte, bool, error) {
			return huge, make([]byte, 32), true, nil
		}); err != nil {
			t.Fatal(err)
		}
		// 3. S rotates
		rotTS := now - 30_000
		var err error
		if emergency {
			err = identitytest.RotateEmergency(s.ctrl, rotTS)
		} else {
			err = identitytest.Rotate(s.ctrl, rotTS)
		}
		if err != nil {
			t.Fatal(err)
		}
		sNew := sender{aid: s.aid, kel: s.ctrl.KEL(), keys: signedKeysFor(t, s.ctrl, 0), ksn: s.ctrl.CurrentSeq(), sign: s.ctrl.Sign, ctrl: s.ctrl,
			notice: suiteNotice(t, s.ctrl.KEL(), uint64(time.Now().UnixMilli()))}
		for i := 0; i < 2; i++ {
			ix3 := "rt-p3-" + time.Now().Format("150405.000000000") + string(rune('a'+i))
			env = craft(t, sNew, prov, seal.TypeDelegate, ix3, delegateBody(t, s.ctrl, ix3, "hi", ""), nil)
			r = receive(t, prov, env)
			row, _ = prov.ix.PeerIdentity(s.aid)
			ks, usable := prov.storedKeySet(row, prov.nowMS())
			t.Logf("after rotation, legit message %d: class=%v KELLen=%d KeySetSeq=%d usableStoredKeys=%v keysRollback=%d", i, r.class, row.KELLen, row.KeySetSeq, usable, counter(prov, keysRollback))
			_ = ks
			if r.class != rxAccepted || !usable || row.KeySetSeq >= huge || counter(prov, keysRollback) != 0 {
				t.Fatalf("emergency=%v: after the rotation: class=%v usableStoredKeys=%v KeySetSeq=%d keysRollback=%d",
					emergency, r.class, usable, row.KeySetSeq, counter(prov, keysRollback))
			}
			if row.CardSeq != 0 {
				t.Fatalf("emergency=%v: the card mark %d outlived the key state that made it", emergency, row.CardSeq)
			}
		}
		_ = identity.NoticeMaxAge
	}
}
