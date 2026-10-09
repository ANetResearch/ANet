package daemon

// epoch_delete.go decides when this node may delete the private half of one
// of its encryption epoch keys, and how far it may trust its own clock to do
// so (forward-secrecy FS-K7 to FS-K16, FS-O1; engine §5.2). Everything here
// is a pure function of its arguments: the key ring (enckeys.go) and the
// engine call them, and the tests drive them with any clocks they like.
//
// The clock gate has two levels (FS §4.3.5):
//
//   - The hub gate says that this node's clock was just checked against its
//     hub: the hub's last answer carried a Date within ClockSkewMS of the
//     local wall clock, it came at most clockCheckMaxAge ago by the boot
//     clock, and the wall clock has not jumped since. Only this gate lets a
//     key go before its hard bound (the early branch, FS-5): that branch's
//     argument needs the two clocks to agree within a known bound.
//   - The local gate says the node may delete by its own clock what has a
//     hard bound (an epoch key at not_after + L, an interaction key's
//     close_after, content past its retention): the hub gate is open, or it
//     has been closed for clockFallbackAfter of running time without the wall
//     clock jumping in this process. It opens whatever the hub answers, so a
//     hub that lies about the time, or does not answer, only delays these
//     deletions by a day of running time (SI-27).
//
// The level is full when the hub gate is open, local when only the local one
// is, closed otherwise. A node without a hub has no hub to check against and
// runs at full (FS-K8).
//
// Running time is counted on the boot clock (bootClock, FS-K13a): it keeps
// counting while the machine sleeps, so a suspended laptop is not taken for
// a wall clock that jumped, and it never goes back.

import (
	"math"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The daemon's forward-secrecy constants (FS §0). None is configurable: the
// public bounds of §21 follow from them (TestFSConstantInvariants).
const (
	// drainMarginMS is M: how long past a key's last acceptance A(k) the
	// mailbox must have been read before the key may go early.
	drainMarginMS uint64 = 15 * 60 * 1000
	// keyMaintainEveryMS is S: the maintenance period of the key ring, the
	// interaction keys' close_after sweep and the retention sweep (FS-K4).
	keyMaintainEveryMS uint64 = 5 * 60 * 1000
	// clockCheckMaxAge is how old the last check against the hub may be,
	// on the boot clock, for the hub gate to stay open.
	clockCheckMaxAge uint64 = 10 * 60 * 1000
	// clockJumpTolerance is how far the wall clock may move apart from the
	// boot clock before it counts as a jump.
	clockJumpTolerance uint64 = 60 * 1000
	// clockFallbackAfter is how long the hub gate must have been closed, in
	// running time, before the local gate opens on its own.
	clockFallbackAfter uint64 = 24 * 60 * 60 * 1000
)

// clockLevel is the level of the clock gate (FS-K14b): full, local or
// closed. It is the interactions store's type, so the deletions there take
// it as it is (SweepCloseAfter; FS-9's contentDeletable). The zero value is
// closed: a caller that does not know the level deletes nothing.
type clockLevel = interactions.ClockLevel

const (
	clockClosed = interactions.ClockClosed // nothing is deleted
	clockLocal  = interactions.ClockLocal  // hard-bound deletions only, by the local clock
	clockFull   = interactions.ClockFull   // the clock was just checked with the hub
)

// The clock states of /status.privacy.clock (FS-K16).
const (
	clockNoHub      = "no_hub"     // no hub is configured
	clockOK         = "ok"         // the hub gate is open
	clockSkewed     = "skewed"     // the last check found the clocks more than ClockSkewMS apart
	clockUnverified = "unverified" // the clock could not be checked lately
)

// deletionOf is /status.privacy.deletion for a level (FS-K16): what the user
// is told about deletions.
func deletionOf(level clockLevel) string {
	switch level {
	case clockFull:
		return "normal"
	case clockLocal:
		return "local_clock"
	}
	return "paused"
}

// clockObs is the latest check of this node's clock against its hub (FS-K14):
// what the last answer of the configured hub that carried a Date said.
type clockObs struct {
	// hub: a hub is configured. Without one there is nothing to check
	// against, and the gate is full.
	hub bool
	// checked: an answer with a Date came from the configured hub in this
	// process.
	checked bool
	// wall and mono are the local wall clock (unix ms) and the boot clock
	// (ms) when the answer arrived; offset is wall minus its Date, in ms.
	wall, mono uint64
	offset     int64
}

// hubGateOpen is the hub gate at nowWall and nowMono (FS-K14): no hub is
// configured, or the last check is at most clockCheckMaxAge old on the boot
// clock, found the clocks at most ClockSkewMS apart, and the wall clock has
// moved with the boot clock since, within clockJumpTolerance.
func hubGateOpen(obs clockObs, nowWall, nowMono uint64) bool {
	if !obs.hub {
		return true
	}
	if !obs.checked || nowMono < obs.mono {
		return false
	}
	since := nowMono - obs.mono
	return since <= clockCheckMaxAge &&
		absMS(obs.offset) <= seal.ClockSkewMS &&
		!wallJumped(obs.wall, obs.mono, nowWall, nowMono)
}

// wallJumped reports whether the wall clock moved by more than
// clockJumpTolerance apart from the boot clock between (wall0, mono0) and
// (wall1, mono1). A boot clock that went back counts as a jump: it never
// does, so the readings cannot be trusted.
func wallJumped(wall0, mono0, wall1, mono1 uint64) bool {
	if mono1 < mono0 {
		return true
	}
	monoDelta := mono1 - mono0
	var drift uint64
	if wall1 >= wall0 {
		wallDelta := wall1 - wall0
		if wallDelta >= monoDelta {
			drift = wallDelta - monoDelta
		} else {
			drift = monoDelta - wallDelta
		}
	} else {
		drift = satAdd(wall0-wall1, monoDelta)
	}
	return drift > clockJumpTolerance
}

// cycleMark is the two clocks at the start of a maintenance period, kept in
// memory from one period to the next.
type cycleMark struct{ wall, mono uint64 }

// nextUnverifiedRun is FS-K14a: unverified_run at the start of a maintenance
// period, from its value run at the previous one. prev is the clocks at the
// previous period of this process, nil at the first.
//
//  1. No hub, or the hub gate is open: 0.
//  2. Otherwise run grows by the boot clock's advance since prev; the first
//     period of a process adds nothing — time the daemon was stopped is not
//     running time — but keeps what earlier processes counted, so restarting
//     often cannot keep it from reaching clockFallbackAfter.
//  3. The wall clock jumped since prev (wallJumped): 0, and the day of
//     running time starts again.
func nextUnverifiedRun(run uint64, hubOpen bool, prev *cycleMark, nowWall, nowMono uint64) uint64 {
	if hubOpen {
		return 0
	}
	if prev == nil {
		return run
	}
	if wallJumped(prev.wall, prev.mono, nowWall, nowMono) {
		return 0
	}
	return satAdd(run, nowMono-prev.mono)
}

// clockGate is FS-K14b: the level of the clock gate and the clock state
// (FS-K16) for the last check obs and run, unverified_run after this
// period's update (nextUnverifiedRun). The hub gate open is full; else
// run of at least clockFallbackAfter is local; else closed. The state is
// no_hub without a hub, ok while the hub gate is open, skewed when the last
// check found the clocks too far apart, unverified otherwise.
func clockGate(obs clockObs, run, nowWall, nowMono uint64) (clockLevel, string) {
	if !obs.hub {
		return clockFull, clockNoHub
	}
	if hubGateOpen(obs, nowWall, nowMono) {
		return clockFull, clockOK
	}
	state := clockUnverified
	if obs.checked && absMS(obs.offset) > seal.ClockSkewMS {
		state = clockSkewed
	}
	if run >= clockFallbackAfter {
		return clockLocal, state
	}
	return clockClosed, state
}

// Why a key is or is not deleted (FS-K7): the reason /status.privacy and
// doctor show for the key kept longest.
const (
	keyDeletable         = "deletable"
	keyMailboxNotDrained = "mailbox_not_drained" // the mailbox has not been read past A(k) + M
	keyHeldMessages      = "held_messages"       // an envelope sealed to it waits in the mailbox
	keyClock             = "clock"               // the clock gate does not allow it
	keyNotYet            = "not_yet"
)

// kidSet is a set of key ids (heldKids, FS-K10).
type kidSet map[string]struct{}

func (s kidSet) has(kid []byte) bool {
	_, ok := s[string(kid)]
	return ok
}

// lastAcceptance is A(k) (FS §0): the last moment a hub may accept an
// envelope sealed to k, min(not_after + D, removed_at), where removed_at 0
// is never. While an old hub is drained after a hub change, the caller
// passes k with removed_at 0 (FS-K6a): that hub still holds a set with k in
// it.
func lastAcceptance(k *ringKey) uint64 {
	a := satAdd(k.Pair.Public.NotAfter, seal.DeliveryDeadlineMS)
	if k.RemovedAt != 0 && k.RemovedAt < a {
		a = k.RemovedAt
	}
	return a
}

// epochKeyDeletable is FS-K7 for key k at now under the gate level gate.
// drainMark is the time of the last read that emptied every polled mailbox
// (FS-K9; the largest uint64 for a node without a hub, FS-K8), heldKids the
// kids of the envelopes a mailbox holds back (FS-K10).
//
//   - closed: never (clock).
//   - local: at the hard bound not_after + L; before it, clock once past
//     A(k) + M, not_yet until then.
//   - full: at the hard bound; past A(k) + M the early branch decides (it
//     lets the key go once the mailbox was read past A(k) + M with nothing
//     sealed to the key held back), and says why it waits.
//
// The hard bound is safe whatever the mailbox holds: an honest hub has
// dropped every envelope sealed to k by then (FS-T1 item 1).
func epochKeyDeletable(k *ringKey, now uint64, gate clockLevel, drainMark uint64, heldKids kidSet) (bool, string) {
	if gate != clockFull && gate != clockLocal {
		return false, keyClock
	}
	if now >= satAdd(k.Pair.Public.NotAfter, seal.MaxMessageLifetimeMS) {
		return true, keyDeletable
	}
	due := satAdd(lastAcceptance(k), drainMarginMS)
	if now < due {
		return false, keyNotYet
	}
	if gate != clockFull {
		return false, keyClock
	}
	switch {
	case drainMark < due:
		return false, keyMailboxNotDrained
	case heldKids.has(k.Pair.Public.KID):
		return false, keyHeldMessages
	}
	// v030-todo(FS-5): the early branch deletes here, (true, keyDeletable);
	// it is merged after the engine's dlv-tx (engine ENG-E3). Until then the
	// key waits for its hard bound.
	return false, keyNotYet
}

// EpochSealed is what an outbox version records about the epoch key it is
// sealed to (engine ENG-S3): the row's cur_ver, and sealed_kid_na, the
// not_after of that key. The engine's row view embeds it and hands it to
// fsResealDue.
type EpochSealed struct {
	// CurVer is the version transmitted now; 0 before the first sealing.
	CurVer uint64
	// SealedKidNA is the not_after of the epoch key the version is sealed to.
	SealedKidNA uint64
}

// fsResealDue is trigger T1 (FS-O1, engine ENG-R2): the current version is
// sealed to a key whose delivery deadline has passed, not_after + D, so a hub
// would refuse it as kid_stale and the engine seals a new version first.
func fsResealDue(row EpochSealed, now uint64) bool {
	return row.CurVer > 0 && now > satAdd(row.SealedKidNA, seal.DeliveryDeadlineMS)
}

// satAdd is a + b, or the largest uint64 when that overflows.
func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// absMS is |v| as a uint64.
func absMS(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}
