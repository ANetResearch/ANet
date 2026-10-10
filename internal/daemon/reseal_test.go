package daemon

// The reseal engine's pure parts (engine §3): the triggers and suppressions
// of resealNeeded, how outboxStep keeps a version the hub holds, the rk the
// store records, and the property every version handed over has (P-ENG-2).

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// TestResealNeeded is engine ENG-R2 and ENG-R3 row by row: T0 to T4 each
// make a new version, nothing else does, and a frozen row is never sealed.
func TestResealNeeded(t *testing.T) {
	const na = uint64(1_800_000_000_000)
	sealedAt := na - 10*hourMS // the version's notice
	sealed := RowView{EpochSealed: EpochSealed{CurVer: 1, SealedKidNA: na},
		KELSealed: KELSealed{SealedKSS: 2, SealedNoticeAt: sealedAt}}
	cur := func(now, noticeAt uint64) KeyCursor {
		return KeyCursor{KeyStateSeq: 2, NoticeObservedAt: noticeAt, Now: now}
	}
	with := func(f func(r *RowView)) RowView { r := sealed; f(&r); return r }
	now := na - hourMS // within the key's day
	for _, c := range []struct {
		name string
		row  RowView
		now  uint64
		cur  KeyCursor
		want bool
	}{
		{"nothing changed", sealed, now, cur(now, sealedAt), false},
		{"T0 never sealed", with(func(r *RowView) { r.CurVer = 0 }), now, cur(now, sealedAt), true},
		{"T0 frozen (S2)", with(func(r *RowView) { r.CurVer, r.Frozen = 0, true }), now, cur(now, sealedAt), false},
		{"T1 at the deadline", sealed, na + seal.DeliveryDeadlineMS, cur(na+seal.DeliveryDeadlineMS, sealedAt), false},
		{"T1 past the deadline", sealed, na + seal.DeliveryDeadlineMS + 1, cur(na+seal.DeliveryDeadlineMS+1, sealedAt), true},
		{"T1 frozen (S2)", with(func(r *RowView) { r.Frozen = true }), na + 2*seal.DeliveryDeadlineMS,
			cur(na+2*seal.DeliveryDeadlineMS, sealedAt), false},
		{"T2 kid_stale", with(func(r *RowView) { r.LastError = "kid_stale: the recipient's key changed" }), now, cur(now, sealedAt), true},
		{"T2 another code", with(func(r *RowView) { r.LastError = "strangers_busy: busy" }), now, cur(now, sealedAt), false},
		{"T3 another key state", sealed, now, KeyCursor{KeyStateSeq: 3, NoticeObservedAt: now, Now: now}, true},
		{"T4 old notice, a newer held", sealed, sealedAt + NoticeResealAge + 1,
			cur(sealedAt+NoticeResealAge+1, sealedAt+NoticeResealAge), true},
		{"T4 old notice, none newer", sealed, sealedAt + NoticeResealAge + 1,
			cur(sealedAt+NoticeResealAge+1, sealedAt), false},
		{"T4 newer notice, version not old", sealed, sealedAt + NoticeResealAge,
			cur(sealedAt+NoticeResealAge, sealedAt+NoticeResealAge), false},
		{"T4 frozen (S2)", with(func(r *RowView) { r.Frozen = true }), sealedAt + 4*hourMS,
			cur(sealedAt+4*hourMS, sealedAt+4*hourMS), false},
	} {
		if got := resealNeeded(c.row, nil, c.now, c.cur); got != c.want {
			t.Errorf("%s: resealNeeded = %v, want %v", c.name, got, c.want)
		}
	}
}

// S1 (ENG-D6, ENG-D7): a version the hub holds queued is neither sent
// through the hub again nor sealed again for it, whatever the triggers say;
// the direct bypass after a query that found it queued for two minutes is
// the one transmission, and it seals by the triggers, direct only.
func TestAQueuedVersionIsNotSealedForTheHub(t *testing.T) {
	const now = uint64(1_800_000_000_000)
	row := RowView{ID: 1, Exp: now + dayMS, HandedAt: now - hourMS,
		EpochSealed: EpochSealed{CurVer: 1, SealedKidNA: now - 2*hourMS}, // T1
		KELSealed:   KELSealed{SealedKSS: 1, SealedNoticeAt: now - 4*hourMS}}
	cur := KeyCursor{KeyStateSeq: 2, NoticeObservedAt: now - minuteMS, Now: now} // T3 and T4
	queued := []VerView{{Ver: 1, HandedAt: now - hourMS, RID: make([]byte, 16), HubState: interactions.OutboxQueued,
		QueuedSince: now - 3*minuteMS}}
	open := GateState{Open: true, State: IdentityOK}
	in := StepInput{Now: now, Gate: open, HubOK: true, P2P: true, Reseal: resealNeeded(row, queued, now, cur)}
	if !in.Reseal {
		t.Fatal("setup: the triggers do not fire")
	}
	if dec := outboxStep(row, queued, in); dec.Action != stepWait {
		t.Fatalf("a queued version: %+v, want a wait (nothing sealed, nothing sent)", dec)
	}
	in.Bypass = true
	if dec := outboxStep(row, queued, in); dec.Action != stepTransmit || !dec.Seal || !dec.P2POnly {
		t.Fatalf("the direct bypass: %+v, want a direct-only transmission of a new version", dec)
	}
	// Once the hub no longer holds it, the hub path is open again and the
	// triggers seal for it.
	unknown := []VerView{queued[0]}
	unknown[0].HubState = interactions.OutboxUnknown
	in.Bypass = false
	if dec := outboxStep(row, unknown, in); dec.Action != stepTransmit || !dec.Seal || dec.P2POnly {
		t.Fatalf("a version the hub lost: %+v, want a new version through the hub", dec)
	}
}

// storedRK takes exactly the canonical rk the receive path recorded, and
// nothing a damaged record could pass off as one.
func TestStoredRK(t *testing.T) {
	pub := bytes.Repeat([]byte{7}, 32)
	good, err := coredet.Marshal(&seal.RK{Suite: seal.SuiteX25519, Pub: pub})
	if err != nil {
		t.Fatal(err)
	}
	rk, err := storedRK(good)
	if err != nil || rk.Suite != seal.SuiteX25519 || !bytes.Equal(rk.Pub, pub) {
		t.Fatalf("storedRK(canonical) = %+v, %v", rk, err)
	}
	enc := func(v any) []byte {
		b, err := coredet.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for name, b := range map[string][]byte{
		"empty":         nil,
		"suite 2":       enc(&seal.RK{Suite: 2, Pub: pub}),
		"short key":     enc(&seal.RK{Suite: seal.SuiteX25519, Pub: pub[:31]}),
		"extra key":     enc(map[uint64]any{1: uint64(1), 2: pub, 3: uint64(0)}),
		"not canonical": append([]byte{0xa2, 0x01, 0x18, 0x01}, good[3:]...), // suite as a two-byte uint
		"trailing":      append(bytes.Clone(good), 0),
		"an array":      enc([]any{uint64(1), pub}),
	} {
		if rk, err := storedRK(b); err == nil {
			t.Errorf("%s: storedRK = %+v, want an error", name, rk)
		}
	}
}

// FuzzStoredRK: storedRK never panics, and what it accepts is an rk of
// suite 1 whose canonical encoding is the input.
func FuzzStoredRK(f *testing.F) {
	good, _ := coredet.Marshal(&seal.RK{Suite: seal.SuiteX25519, Pub: bytes.Repeat([]byte{7}, 32)})
	f.Add(good)
	f.Add([]byte{0xa0})
	f.Add([]byte{0xa2, 0x01, 0x02, 0x02, 0x40})
	f.Fuzz(func(t *testing.T, b []byte) {
		rk, err := storedRK(b)
		if err != nil {
			return
		}
		re, err := coredet.Marshal(rk)
		if err != nil || !bytes.Equal(re, b) || rk.Suite != seal.SuiteX25519 || len(rk.Pub) != 32 {
			t.Fatalf("accepted %x as %+v (re-encoded %x, %v)", b, rk, re, err)
		}
	})
}

// P-ENG-2 (engine §8.1) at the level of the engine's decisions: one queued
// message over random days of the sender's clock, with planned rotations,
// notices the witness issues while it can be reached, a hub that loses or
// holds what it took, kid_stale answers and direct bypasses. Every attempt
// decides with the real outboxStep and resealNeeded; a new version records
// the key, the key state and the notice of its moment, as sealVersion does.
// Every version handed over must, at that moment:
//
//	(i)   be sealed to a key not past its delivery deadline: now ≤ na + D;
//	(ii)  be signed under the key state of then;
//	(iii) carry a notice at most NoticeResealAge old, or the newest this
//	      node holds;
//	(iv)  carry a notice no older than the stop line (the gate).
func TestResealModel(t *testing.T) {
	const runs, steps = 400, 300
	for run := 0; run < runs; run++ {
		seed := int64(run)
		t.Run(fmt.Sprint(seed), func(t *testing.T) { resealModelRun(t, seed, steps) })
	}
}

// resealModel is one run's world: the sender's clock and key state, the
// notice it holds, the witness's reachability and the row.
type resealModel struct {
	now      uint64
	seq      uint64
	notice   uint64 // observed_at of the notice for seq; 0 for none
	hubUp    bool
	row      RowView
	vers     []VerView
	handed   int
	resealed int
}

func (m *resealModel) gate() GateState {
	if m.notice == 0 || m.now > m.notice && m.now-m.notice > NoticeStopLine {
		return GateState{State: IdentityNoticeStale}
	}
	return GateState{Open: true, State: IdentityOK}
}

func (m *resealModel) cursor() KeyCursor {
	return KeyCursor{KeyStateSeq: m.seq, NoticeObservedAt: m.notice, Now: m.now}
}

// seal is sealVersion's record of a new version: today's epoch key, the key
// state and the notice of now.
func (m *resealModel) seal(t *testing.T) {
	t.Helper()
	na := (m.now/seal.EpochMS + 1) * seal.EpochMS
	if m.row.CurVer > 0 && lastErrorCode(m.row.LastError) != codeKidStale &&
		na == m.row.SealedKidNA && m.seq == m.row.SealedKSS && m.notice == m.row.SealedNoticeAt {
		// A new version the same as the last but for its randomness: what
		// T4 without "a newer notice" makes while the witness is away (KEL
		// D-51(b), E-T10(b)).
		t.Fatalf("at %d a new version with the key, key state and notice of the last", m.now)
	}
	m.row.CurVer++
	m.row.SealedKidNA = na
	m.row.SealedKSS = m.seq
	m.row.SealedNoticeAt = m.notice
	m.row.LastError = ""
	m.vers = append(m.vers, VerView{Ver: m.row.CurVer})
	m.resealed++
}

// hand checks P-ENG-2 for the current version at the moment it is handed
// over, and records it as handed.
func (m *resealModel) hand(t *testing.T, path string) {
	t.Helper()
	r := m.row
	switch {
	case m.now > r.SealedKidNA+seal.DeliveryDeadlineMS:
		t.Fatalf("(i) %s at %d: sealed to a key whose deadline passed at %d", path, m.now, r.SealedKidNA+seal.DeliveryDeadlineMS)
	case r.SealedKSS != m.seq:
		t.Fatalf("(ii) %s: signed under key state %d, the node is at %d", path, r.SealedKSS, m.seq)
	case m.now-r.SealedNoticeAt > NoticeResealAge && m.notice > r.SealedNoticeAt:
		t.Fatalf("(iii) %s: a notice %d ms old, a newer one held", path, m.now-r.SealedNoticeAt)
	case m.now-r.SealedNoticeAt > NoticeStopLine:
		t.Fatalf("(iv) %s: a notice %d ms old, past the stop line", path, m.now-r.SealedNoticeAt)
	}
	v := &m.vers[len(m.vers)-1]
	if v.HandedAt == 0 {
		v.HandedAt = m.now
	}
	if m.row.HandedAt == 0 {
		m.row.HandedAt = m.now
	}
	m.handed++
}

func resealModelRun(t *testing.T, seed int64, steps int) {
	rng := rand.New(rand.NewSource(seed))
	start := uint64(1_800_000_000_000) + uint64(rng.Int63n(int64(seal.EpochMS)))
	m := &resealModel{now: start, seq: 1, notice: start, hubUp: true,
		row: RowView{ID: 1, TS: start, Exp: start + messageLifetimeMS}}
	for i := 0; i < steps && m.now <= m.row.Exp; i++ {
		switch rng.Intn(10) {
		case 0, 1: // time passes: minutes to hours
			m.now += uint64(rng.Int63n(int64(4 * hourMS)))
		case 2: // a planned rotation: the notice for the new tip comes later
			if rng.Intn(4) == 0 {
				m.seq++
				m.notice = 0
			}
		case 3: // the witness goes away or comes back
			m.hubUp = rng.Intn(3) != 0
		case 4: // a poll or a registration brings a notice
			if m.hubUp {
				m.notice = m.now
			}
		case 5: // the hub's last answer was kid_stale
			if m.row.CurVer > 0 && rng.Intn(3) == 0 {
				m.row.LastError = "kid_stale: model"
			}
		case 6: // the hub reports what it holds: queued for a while, or lost
			for j := range m.vers {
				v := &m.vers[j]
				if v.RID == nil {
					continue
				}
				if rng.Intn(2) == 0 {
					v.HubState = interactions.OutboxQueued
					if v.QueuedSince == 0 {
						v.QueuedSince = m.now - p2pBypassMS
					}
				} else {
					v.HubState, v.QueuedSince = interactions.OutboxUnknown, 0
				}
			}
		default: // an attempt
			m.attempt(t, rng)
		}
	}
	if m.handed > 0 && m.resealed == 0 {
		t.Fatalf("handed over %d times, never sealed", m.handed)
	}
}

// attempt is one attempt: the engine's decision, a new version when it says
// so, and the hand-over through the paths it allows.
func (m *resealModel) attempt(t *testing.T, rng *rand.Rand) {
	t.Helper()
	g := m.gate()
	in := StepInput{Now: m.now, Gate: g, HubOK: m.hubUp, P2P: rng.Intn(2) == 0, Bypass: rng.Intn(3) == 0}
	if g.Open {
		in.Reseal = resealNeeded(m.row, m.vers, m.now, m.cursor())
	}
	dec := outboxStep(m.row, m.vers, in)
	if dec.Action != stepTransmit {
		return
	}
	if !g.Open {
		t.Fatalf("a transmission while the gate is closed: %+v", dec)
	}
	if dec.Seal {
		m.seal(t)
	}
	if in.P2P {
		m.hand(t, "direct")
		if rng.Intn(4) == 0 {
			m.row.LastError = ""
			return // a token came back: confirmed, and the model goes on as if not
		}
	}
	if dec.P2POnly || !m.hubUp {
		return
	}
	m.hand(t, "hub")
	v := &m.vers[len(m.vers)-1]
	switch rng.Intn(4) {
	case 0: // 409 kid_stale
		m.row.LastError = "kid_stale: model"
	default: // 200: queued under a rid, not asked about yet
		v.RID, v.HubState, v.QueuedSince = make([]byte, 16), "", 0
		m.row.LastError = ""
	}
}
