package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cardSeq is this node's next card sequence: the anet-card/v1 seq of its A2A
// network card (a2a_card.go), seeded from the clock, guaranteed to exceed
// every sequence this process has already minted.
//
// A hub admits a card only if its seq EXCEEDS the highest it has seen for
// that agent, which is what makes a replayed older card a no-op rather
// than a rollback. The number was the registration time in
// seconds, and the comment here claimed that "two registrations a second
// apart cannot tie" — true, and beside the point. Two registrations in
// the SAME second tie, and are then refused with STALE_SEQ.
//
// That is not an edge case. `anet hub-register` twice in a row does it;
// so does a node that registers and immediately re-registers after its
// capability list changes. The second call failed for a reason that had
// nothing to do with what it was doing. Found by the admission joint test
// (scripts/joint-invite.sh, now joint-admission.sh), whose "an existing node re-registers without
// an invite" step failed against a hub that had nothing to do with it.
//
// The fix belongs here rather than in the hub: the high-water rule is
// correct, and the party that must mint strictly increasing numbers is
// the party that signs them.
//
// Seconds are kept as the unit on purpose. Switching to milliseconds
// would also break the tie, and would multiply every sequence by a
// thousand — after which a node that ever downgraded to an older build
// could never update its card again, because its second-resolution
// sequence would sit permanently below the hub's high water. The counter
// only ever advances past real time when there are ties, by one per tie,
// and drifts back to the clock as soon as they stop.
//
// The counter is also PERSISTED, which the first version of this fix left
// out on the reading that a restart takes longer than the one-second
// resolution. It does not: a restart inside the same second starts a fresh
// in-memory counter from the same clock value and collides with the hub's
// high water. That was survivable while registration only happened when an
// operator asked for it; once the daemon began re-publishing its
// capabilities at startup, every quick restart hit it.
//
// Persisting also closes the other half — a clock that steps BACKWARDS
// across a restart no longer lands below the high water, because the file
// remembers where the node had got to.
func (d *Daemon) cardSeq() uint64 {
	for {
		last := d.lastCardSeq.Load()
		seq := uint64(time.Now().Unix())
		if seq <= last {
			seq = last + 1
		}
		if d.lastCardSeq.CompareAndSwap(last, seq) {
			d.persistCardSeq(seq)
			return seq
		}
	}
}

// cardSeqPath is where the counter survives a restart. Its own file rather
// than a config field: it is written on every registration and read once,
// and rewriting the whole config for a counter would put an operator's
// hand-edited settings in the path of a background refresh.
func (d *Daemon) cardSeqPath() string { return filepath.Join(d.layout.Root, "card_seq") }

func (d *Daemon) persistCardSeq(seq uint64) {
	if err := os.WriteFile(d.cardSeqPath(), []byte(strconv.FormatUint(seq, 10)), 0o600); err != nil {
		// Not fatal: the clock still supplies a value, and the failure mode
		// is the one this file exists to narrow, not a new one.
		log.Printf("anet: could not persist the card sequence: %v", err)
	}
}

// loadCardSeq seeds the counter from the last sequence this node minted.
func (d *Daemon) loadCardSeq() {
	b, err := os.ReadFile(d.cardSeqPath())
	if err != nil {
		return
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return
	}
	d.lastCardSeq.Store(n)
}
