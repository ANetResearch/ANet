package daemon

// card_highwater.go is the consumer side of the A2A card params.seq rule
// (A2A-DESIGN §3.8, §10.3; plan 0014 B3-09). A card read from a hub has
// verified against its signer's KEL; this decides whether it may replace
// what this node already admitted for that agent:
//
//   - a higher seq advances the mark;
//   - the same seq with the same payload (proto-stripped hash) is the same
//     card again, and is admitted;
//   - the same seq with another payload is a fork — the signer issued two
//     cards under one seq — and is counted and refused;
//   - a lower seq is a rollback, and is refused.
//
// For a peer this node has a peer_identity row for (an authorized
// relationship, §3.8), the mark is kept in that row, so a restart does not
// forget it and a hub cannot serve an older card to a fresh process. For
// anyone else it lives only in the process's bounded cache: reading a
// directory is not contact with an agent, and writes no row.

import (
	"log"

	"github.com/ANetResearch/ANetCore/a2acard"
)

// admitCardMark applies the high-water rule to a verified card.
func (d *Daemon) admitCardMark(v *a2acard.Verified) error {
	self := d.AID()
	if d.ix != nil {
		found, err := d.ix.UpdatePeerCardMark(v.AID, func(seq uint64, hash []byte) (uint64, []byte, bool, error) {
			stored := persistedCardMark(seq, hash)
			// A mark this process admitted before the row existed (the
			// peer became known meanwhile) may be the higher one.
			if m, ok := cardMarks.get(self, v.AID); ok && (stored == nil || m.Seq > stored.Seq) {
				stored = &m
			}
			dec, err := a2acard.CheckHighWater(stored, v.Mark())
			if err != nil {
				return 0, nil, false, err
			}
			// Same: the card is refreshed, nothing to write unless the row
			// had no hash yet.
			write := dec == a2acard.Advance || len(hash) != len(v.PayloadHash)
			return v.Seq, append([]byte(nil), v.PayloadHash[:]...), write, nil
		})
		if found {
			if err == nil {
				_ = cardMarks.admit(self, v) // keep the cache in step
			}
			return cardMarks.noteFork(self, v, err)
		}
		if err != nil {
			return err
		}
	}
	return cardMarks.noteFork(self, v, cardMarks.admit(self, v))
}

// persistedCardMark is the mark a peer_identity row holds, or nil when it
// holds none (card_seq 0, or no hash kept).
func persistedCardMark(seq uint64, hash []byte) *a2acard.Mark {
	var m a2acard.Mark
	if seq == 0 || len(hash) != len(m.PayloadHash) {
		return nil
	}
	m.Seq = seq
	copy(m.PayloadHash[:], hash)
	return &m
}

// get returns the cached mark of aid's card for node self.
func (c *cardMarkCache) get(self, aid string) (a2acard.Mark, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.m[self+"\x00"+aid]
	return m, ok
}

// noteFork counts and logs a fork refusal, and returns err unchanged.
func (c *cardMarkCache) noteFork(self string, v *a2acard.Verified, err error) error {
	if !a2acard.IsCode(err, a2acard.CodeSeqFork) {
		return err
	}
	c.mu.Lock()
	if c.forks == nil {
		c.forks = map[string]uint64{}
	}
	c.forks[self]++
	c.mu.Unlock()
	log.Printf("anet: %s signed two different network cards under seq %d; the second is ignored", v.AID, v.Seq)
	return err
}

// forkCount is how many forked cards node self has refused.
func (c *cardMarkCache) forkCount(self string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.forks[self]
}
