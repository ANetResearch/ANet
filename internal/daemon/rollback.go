package daemon

// rollback.go is what a hub's 409 keys_rollback says about this node, and
// what the node does about it (forward-secrecy §4.7; core G25; engine
// ENG-K16, SI-33).
//
// The hub answers a key set publication or a registration with
// keys_rollback when it holds a set of this identity that the one sent does
// not follow. Three things can be behind it:
//
//   - a data directory that never had a task: a new machine's `anet
//     identity recover`, or an identity file copied alone. The node follows
//     the hub's seq and says nothing (G25: FS-B2 item 1). It keeps a silent
//     mark, so that a second rollback within 7 days is checked: the same
//     fresh directory copied and run twice would otherwise never be found.
//   - this node's data went back: a restored backup, or a copy of the data
//     directory running elsewhere. The hub then holds a set this identity
//     signed and this node did not. Found by reading the identity's own
//     directory entry (the anonymous bucket query the node asks about its
//     peers with, SI-12) and verifying its set against this node's KEL;
//     the tasks in flight end with local_state_lost, their peers are told,
//     the epoch keys are made anew, and the user is shown it for 7 days
//     (FS-B3, FS-B4). A second one within 7 days says the identity is
//     running somewhere else (FS-B4a).
//   - anything else, a hub that lies included: the node follows the hub's
//     seq, counts keys-rollback-unverified and ends nothing. A hub can make
//     the node's seq jump, never end a task: only a set this identity signed
//     makes the rollback a fact the node states to its user.
//
// Every key set publication holds the publish lock (FS-K5a), so this runs
// under it and the sets this node sent reached the hub in the order it
// signed them: a keys_rollback is never this node's own newer set.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/dirbox"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// localStateNoticeFor is how long a rollback this node found is shown, and
// how close two must be for the second to say the identity runs somewhere
// else; a rollback mark this recent also stops a node with no task from
// following the hub silently (FS §0, FS-B2, FS-B4, FS-B4a). Unix ms.
const localStateNoticeFor uint64 = 7 * 24 * 3600 * 1000

// counterKeysRollbackUnverified counts the hub's keys_rollback answers this
// node followed without finding that its own data went back (FS-B2).
const counterKeysRollbackUnverified = "keys-rollback-unverified"

// EvLocalStateLost records a rollback this node found and what it ended
// (FS §2.12): the hub's seq, this node's, and the tasks that failed.
const EvLocalStateLost = evtypes.LocalStateLost

// rollbackAnswer is what a hub's keys_rollback led to (answerKeysRollback).
type rollbackAnswer uint8

const (
	// rollbackNotFollowed: there is nothing to send again — the stored_seq
	// is not one to follow, key sets are held, or no set could be signed.
	rollbackNotFollowed rollbackAnswer = iota
	// rollbackResend: a set that follows the hub's is signed; the caller
	// sends once more.
	rollbackResend
	// rollbackRetryLater: the hub's set could not be checked, so nothing is
	// decided now; the next maintenance period publishes and asks again.
	rollbackRetryLater
)

// answerKeysRollback is FS-B2: the hub at hubURL answered a key set
// publication or a registration with 409 keys_rollback and stored_seq. The
// caller holds the publish lock.
func (d *Daemon) answerKeysRollback(ctx context.Context, hubURL string, storedSeq uint64) (rollbackAnswer, error) {
	if d.keysHeld() {
		return rollbackNotFollowed, errKeysHeld
	}
	now := d.nowMS()
	mark := d.enc.rollbackMark()
	if mark != nil && mark.StoredSeq == storedSeq {
		// The same rollback again: the steps a failure cut short are
		// finished, and the ring follows it as before. No new mark.
		if !mark.Silent {
			if err := d.finishRollback(*mark); err != nil {
				return rollbackNotFollowed, err
			}
			return rollbackResend, nil
		}
		return d.followStoredSeq(hubURL, storedSeq, nil)
	}
	rows, err := d.ix.Count(interactions.ListFilter{})
	if err != nil {
		return rollbackRetryLater, fmt.Errorf("anet: count this node's tasks: %w", err)
	}
	if rows == 0 && !recentMark(mark, now) {
		// core G25: nothing here could have gone back. Followed silently,
		// with a mark only a second rollback within 7 days reads.
		m := &rollbackMark{At: now, StoredSeq: storedSeq, Logged: true, Silent: true}
		ans, err := d.followStoredSeq(hubURL, storedSeq, m)
		if ans == rollbackResend {
			log.Printf("anet: %s holds key set seq %d of this identity, which this new data directory follows", hubURL, storedSeq)
		}
		return ans, err
	}
	check, why := d.checkStoredKeySet(ctx, hubURL, storedSeq)
	switch check {
	case setUnavailable:
		log.Printf("anet: %s answered keys_rollback (seq %d) and its directory could not be read to check it: %v (anet tries again on its own)",
			hubURL, storedSeq, why)
		return rollbackRetryLater, why
	case setSignedElsewhere:
		if err := d.rollBack(storedSeq, mark, now); err != nil {
			return rollbackNotFollowed, err
		}
		return rollbackResend, nil
	}
	ans, err := d.followStoredSeq(hubURL, storedSeq, nil)
	if ans == rollbackResend {
		d.countErr(counterKeysRollbackUnverified, fmt.Errorf("%s holds key set seq %d of this identity, which is not a set this identity signed elsewhere (%v); the next set follows it",
			hubURL, storedSeq, why))
	}
	return ans, err
}

// recentMark reports whether the ring holds a rollback mark of the last 7
// days, silent or not (FS-B2 item 1).
func recentMark(m *rollbackMark, now uint64) bool {
	return m != nil && withinNotice(m.At, now)
}

// withinNotice reports whether a mark made at `at` is within the 7 days of
// localStateNoticeFor at now, the bound included. A mark dated after now —
// the wall clock went back — counts as recent.
func withinNotice(at, now uint64) bool {
	return at != 0 && (now <= at || now-at <= localStateNoticeFor)
}

// followStoredSeq signs the ring's set again above storedSeq, writing mark
// with it when not nil (adoptStoredSeq).
func (d *Daemon) followStoredSeq(hubURL string, storedSeq uint64, mark *rollbackMark) (rollbackAnswer, error) {
	in := d.ringInputsAt(d.nowMS())
	in.hold = d.keysHeld()
	adopted, err := d.enc.adoptStoredSeq(storedSeq, in, mark)
	if err != nil {
		log.Printf("anet: sign a key set above the one %s holds: %v", hubURL, err)
		return rollbackNotFollowed, err
	}
	if !adopted {
		return rollbackNotFollowed, nil
	}
	return rollbackResend, nil
}

// storedSetCheck is what the identity's directory entry said of the hub's
// set (checkStoredKeySet).
type storedSetCheck uint8

const (
	// setNotShown: no entry holds a set at stored_seq that this identity
	// signed and this node did not.
	setNotShown storedSetCheck = iota
	// setSignedElsewhere: an entry holds one: this node's data went back.
	setSignedElsewhere
	// setUnavailable: the directory could not be read.
	setUnavailable
)

// checkStoredKeySet is FS-B2 item 2's check: this identity's entry in the
// hub's directory, asked for on the anonymous side as any peer's would be
// (resolvePeer's bucket query, a fresh answer), holds a key set at
// storedSeq that verifies against this node's KEL as it was at the set's
// issued_at, and that this node's ring did not sign (signedElsewhere).
func (d *Daemon) checkStoredKeySet(ctx context.Context, hubURL string, storedSeq uint64) (storedSetCheck, error) {
	aid := d.AID()
	h, err := dirbox.Hash(aid)
	if err != nil {
		return setNotShown, err
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	ans, _, err := d.bucketOf(ctx, strings.TrimRight(hubURL, "/"), h, true)
	if err != nil {
		return setUnavailable, err
	}
	opened, _ := hubapi.OpenEntries(aid, h, ans.entries)
	if len(opened) == 0 {
		if ans.partial {
			return setUnavailable, errors.New("the hub's directory answer was partial and holds no entry of this identity")
		}
		return setNotShown, errors.New("the hub's directory holds no entry of this identity")
	}
	kel := d.self.KEL()
	ours, last, ownPrev := d.enc.ownSets()
	var why error
	for _, e := range opened {
		err := signedElsewhere(e.Keys, aid, kel, storedSeq, ours, last, ownPrev)
		if err == nil {
			return setSignedElsewhere, nil
		}
		if why == nil {
			why = err
		}
	}
	return setNotShown, why
}

// signedElsewhere returns nil when signed (a SignedEncKeySet encoding) is a
// set at storedSeq that this identity signed — it verifies against kel,
// this node's KEL, at its own issued_at — and that this node's ring did
// not: its seq is above ownPrev, and it is not the ring's current set
// (ours, at last). Otherwise it says why not.
//
// FS-B2 states the second half as "stored_seq > last_seq". A seq is the
// unix ms of the signing, and a node signs a set at the moment it publishes
// one — at a new epoch, or when it starts on a restored backup — so its
// last_seq is above a set its other copy signed a little earlier more often
// than not, and that rule would let both the restored backup and the copy
// running elsewhere pass as nothing. What the rule is for is that a hub
// must not be able to make one of this node's own sets read as somebody
// else's; the ring knows every seq it signed is at ownPrev or below, or is
// its current set, and that is what is checked.
func signedElsewhere(signed []byte, aid string, kel []identity.SignedEvent, storedSeq uint64, ours []byte, last, ownPrev uint64) error {
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	if err != nil {
		return fmt.Errorf("the directory's key set does not decode: %w", err)
	}
	var head seal.EncKeySet
	if err := coredet.Unmarshal(s.Set, &head); err != nil {
		return fmt.Errorf("the directory's key set does not decode: %w", err)
	}
	set, err := seal.VerifyEncKeySet(s, aid, kel, head.IssuedAt)
	if err != nil {
		return fmt.Errorf("the directory's key set does not verify against this node's KEL: %w", err)
	}
	if set.Seq != storedSeq {
		return fmt.Errorf("the directory's key set is at seq %d, not the %d the hub named", set.Seq, storedSeq)
	}
	if storedSeq <= ownPrev {
		return fmt.Errorf("seq %d is not above %d, below which this node signed its earlier sets", storedSeq, ownPrev)
	}
	if storedSeq == last && sameSetContent(ours, s.Set) {
		return errors.New("it is this node's own current set")
	}
	return nil
}

// sameSetContent reports whether signed (a SignedEncKeySet encoding) holds
// exactly the set bytes set.
func sameSetContent(signed, set []byte) bool {
	s, err := seal.UnmarshalSignedEncKeySet(signed)
	return err == nil && bytes.Equal(s.Set, set)
}

// rollBack is FS-B3 for a rollback checkStoredKeySet found: this node's
// data went back. prev is the ring's mark before it (nil: none), now the
// time of the finding. Step 1 marks it in the ring file; finishRollback
// does the rest; the caller sends the new set (step 5).
func (d *Daemon) rollBack(storedSeq uint64, prev *rollbackMark, now uint64) error {
	if storedSeq >= maxFollowedSeq {
		return fmt.Errorf("anet: stored_seq %d is past what a key set of this identity has", storedSeq)
	}
	tasks, err := d.ix.Count(interactions.ListFilter{Active: true})
	if err != nil {
		return fmt.Errorf("anet: count the tasks in flight: %w", err)
	}
	_, last, _ := d.enc.ownSets()
	m := rollbackMark{At: now, StoredSeq: storedSeq, Tasks: uint64(tasks), PrevAt: 0}
	if prev != nil {
		m.PrevAt = prev.At
	}
	log.Printf("anet: the hub holds key set seq %d, which this identity signed and this node did not (it signed up to %d): "+
		"this node's data went back, from a restored backup or a copy of the data directory running elsewhere; "+
		"ending %d task(s) in flight and making new encryption keys", storedSeq, last, tasks)
	if err := d.enc.setRollback(m); err != nil { // step 1
		return err
	}
	if err := d.rollbackStep(rbMarked); err != nil {
		return err
	}
	return d.finishRollbackFrom(m, last, true)
}

// finishRollback finishes the rollback marked m: steps 2 to 4 of FS-B3,
// each a no-op when done already. A mark whose evidence is not written yet
// (finishLeftRollback), or a second answer naming its stored_seq, comes
// here; the last_seq from before it is not known then.
func (d *Daemon) finishRollback(m rollbackMark) error {
	return d.finishRollbackFrom(m, 0, false)
}

// The points of a rollback a test can stop it at (setRollbackStep): after
// each of FS-B3's steps 1 to 3, between the evidence and the mark that
// says it is written, and after that mark.
const (
	rbMarked = iota + 1
	rbClosed
	rbRegenerated
	rbRecorded
	rbLogged
)

// finishRollbackFrom is steps 2 to 4 of FS-B3 for mark m. localSeq is the
// ring's last_seq before the rollback, recorded as the evidence's local_seq
// when known (0 when the steps resume after a failure or a crash: the ring
// file keeps no copy of it).
func (d *Daemon) finishRollbackFrom(m rollbackMark, localSeq uint64, known bool) error {
	if m.Logged {
		// Its evidence is written: the steps before it are done.
		return nil
	}
	// Step 2: one transaction ends every task in flight with
	// local_state_lost, deletes the interaction keys and queues the notices
	// (ENG-K16); with nothing open it only deletes keys that are left. Once
	// step 3 is done, so is this one, and the tasks begun since go on.
	if !d.enc.regeneratedFor(m.StoredSeq) {
		var c *closedSet
		err := d.ix.Update(func(tx *interactions.Tx) error {
			var err error
			c, err = d.closeInteractionsTx(tx, closeLocalStateLost)
			return err
		})
		if err != nil {
			return fmt.Errorf("anet: end the tasks in flight after the local state went back: %w", err)
		}
		d.afterClose(c)
		if err := d.rollbackStep(rbClosed); err != nil {
			return err
		}
	}
	// Step 3: the epoch keys made anew, the set above the hub's (FS-K21);
	// a no-op when regen_tag is stored_seq already.
	if _, err := d.enc.regenerate(m.StoredSeq, m.StoredSeq, d.ringInputsAt(d.nowMS())); err != nil {
		return fmt.Errorf("anet: make new encryption keys after the local state went back: %w", err)
	}
	if err := d.rollbackStep(rbRegenerated); err != nil {
		return err
	}
	// Step 4: the evidence, once, then the mark says so.
	if known || !d.rollbackLogged(m.StoredSeq) {
		if d.ledger == nil {
			return errors.New("anet: no evidence log to record the local state rollback in")
		}
		if _, err := d.ledger.Append(EvLocalStateLost, map[string]any{
			"hub_seq": m.StoredSeq, "local_seq": localSeq, "failed_tasks": m.Tasks,
		}); err != nil {
			return fmt.Errorf("anet: record the local state rollback: %w", err)
		}
	}
	if err := d.rollbackStep(rbRecorded); err != nil {
		return err
	}
	m.Logged = true
	if err := d.enc.setRollback(m); err != nil {
		return err
	}
	return d.rollbackStep(rbLogged)
}

// rollbackLogged reports whether the evidence log holds the
// anet.local_state.lost of the rollback to storedSeq already: a crash came
// between the record and the mark that says it is written.
func (d *Daemon) rollbackLogged(storedSeq uint64) bool {
	if d.ledger == nil {
		return false
	}
	want, found := strconv.FormatUint(storedSeq, 10), false
	d.ledger.scan(EvLocalStateLost, 0, func(_ int64, p map[string]any) {
		if fmt.Sprint(p["hub_seq"]) == want {
			found = true
		}
	})
	return found
}

// finishLeftRollback finishes a rollback that a crash or a failure cut
// short — its mark is there, its evidence not written (FS-B3: "from step
// 2"). The start runs it before anything is sent, after the close of a
// stopped identity and before the outbox; every publication runs it first,
// under the publish lock, so a failure the process survived is finished
// too. The rollback's set goes out with the publication.
func (d *Daemon) finishLeftRollback() {
	if d.enc == nil || d.keysHeld() {
		return
	}
	m := d.enc.rollbackMark()
	if m == nil || m.Silent || m.Logged {
		return
	}
	if err := d.finishRollback(*m); err != nil {
		log.Printf("anet: finish handling the local state rollback found earlier: %v (anet tries again at its next publication)", err)
	}
}

// rollbackStep runs the test hook at point n of a rollback (rbMarked …).
func (d *Daemon) rollbackStep(n int) error {
	if h := d.enc.rbStep.Load(); h != nil {
		return (*h)(n)
	}
	return nil
}

// localStateNotice is /status.privacy.local_state_lost (FS §2.10): a
// rollback this node found, for the 7 days it is shown.
type localStateNotice struct {
	At       string `json:"at"` // RFC 3339, UTC
	Tasks    uint64 `json:"tasks"`
	Repeated bool   `json:"repeated"`
}

// localStateLost is the rollback to show now (FS-B4, FS-B4a), or nil.
func (d *Daemon) localStateLost() *localStateNotice {
	if d.enc == nil {
		return nil
	}
	return localStateNoticeAt(d.enc.rollbackMark(), d.nowMS())
}

// localStateNoticeAt is FS-B4 and FS-B4a for mark m at now: a mark that is
// not silent is shown for 7 days from its at, whether or not it ended any
// task — a set this identity signed and this node did not means another
// copy of it may be running. It is repeated when the mark before it was at
// most 7 days earlier: from a data directory that runs on, only another
// copy signing key sets explains two (a restored backup brings back its
// older mark too). A forged keys_rollback writes no mark, so it can make
// neither.
func localStateNoticeAt(m *rollbackMark, now uint64) *localStateNotice {
	if m == nil || m.Silent || !withinNotice(m.At, now) {
		return nil
	}
	repeated := m.PrevAt != 0 && (m.At <= m.PrevAt || m.At-m.PrevAt <= localStateNoticeFor)
	return &localStateNotice{
		At:       time.UnixMilli(int64(min(m.At, math.MaxInt64))).UTC().Format(time.RFC3339),
		Tasks:    m.Tasks,
		Repeated: repeated,
	}
}

// said is the sentence the privacy line and doctor's local_state show for
// n (FS-U7 rows 0 and 1; ux §1.4 E4): local_state.repeated (fail) when
// repeated, local_state.detected (warn) otherwise; both say what to run.
func (n *localStateNotice) said() *a2ashape.Said {
	if n == nil {
		return nil
	}
	key := "local_state.detected"
	if n.Repeated {
		key = "local_state.repeated"
	}
	return &a2ashape.Said{Key: key, Args: a2ashape.Args{"time": n.At, "n": n.Tasks}}
}

// setPublishWait makes a publication that finds the publish lock held call
// f before it waits; nil removes it. Tests only.
func (d *Daemon) setPublishWait(f func()) {
	if f == nil {
		d.enc.pubWait.Store(nil)
		return
	}
	d.enc.pubWait.Store(&f)
}

// setPublishSent makes a publication call f after it read the set it sends,
// before it sends it; nil removes it. Tests only.
func (d *Daemon) setPublishSent(f func()) {
	if f == nil {
		d.enc.pubSent.Store(nil)
		return
	}
	d.enc.pubSent.Store(&f)
}

// setRollbackStep makes a rollback call f after each step; an error stops it
// there, as a crash would. nil removes it. Tests only.
func (d *Daemon) setRollbackStep(f func(step int) error) {
	if f == nil {
		d.enc.rbStep.Store(nil)
		return
	}
	d.enc.rbStep.Store(&f)
}
