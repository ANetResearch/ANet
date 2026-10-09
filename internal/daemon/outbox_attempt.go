package daemon

// outbox_attempt.go is the shell of the outbound engine (engine §2.4–§2.8):
// one attempt at an outbox row, under the row's lock, in the normative
// order A0–A12 — read, decide (outboxStep), ask the hub what it holds, seal
// and write the version, choose the credential, hand the bytes to a direct
// path and to the hub, record the outcome — and the two ways a row ends:
// confirmed by the recipient's delivery token, or given up.
//
// No network call is made inside a transaction of the store, and nothing of
// a version leaves the process before the transaction that wrote it and
// marked the row attempting has committed (A8, SI-83).

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// The engine's backoff (engine §0.4). Code constants: there is nothing for
// an operator to tune.
const (
	retryBaseDelay = 5 * time.Second
	retryMaxDelay  = 24 * time.Hour
	// retryScanInterval is how often the loop looks for due rows when
	// nothing kicks it.
	retryScanInterval = 5 * time.Second
	// retryAfterMin is the least wait a Retry-After is taken as, so a hub
	// answering 0 does not make the loop spin.
	retryAfterMin = time.Second
	// p2pBackoffMin and p2pBackoffMax bound the wait before a direct path
	// is tried again for an AID that took bytes without a valid token
	// (DLV-S-30).
	p2pBackoffMin = 30 * time.Second
	p2pBackoffMax = 10 * time.Minute
	// kidStaleFastPerHour is how many kid_stale answers a row is sealed and
	// sent again for at once within an hour; past it they back off (FS-O2,
	// ENG-R11).
	kidStaleFastPerHour = 3
	// attemptRounds bounds the sends one attempt makes: a kid_stale or a
	// refused credential explained, a recipient whose home moved, each
	// sends the same message once more at once.
	attemptRounds = 4
)

// The engine's counters (DLV §2.8; /status.delivery.counters).
const (
	cntDAckConfirmedP2P      = "dack-confirmed-p2p"
	cntDAckConfirmedHub      = "dack-confirmed-hub"
	cntDAckConfirmedImplicit = "dack-confirmed-implicit"
	cntDAckRefused           = "dack-refused"
	cntDAckInvalid           = "dack-invalid"
	cntDAckNoteInvalid       = "dack-note-invalid"
	cntDAckP2PNoDAck         = "dack-p2p-no-dack"
	cntDAckStatusQueued      = "dack-status-queued"
	cntDAckStatusUnknown     = "dack-status-unknown"
	cntDAckStatusError       = "dack-status-error"
	cntDAckRetransmit        = "dack-retransmit"
	cntDAckVersionEvicted    = "dack-version-evicted"
	cntDAckAbandonedSent     = "dack-abandoned-sent"
	cntDAckAbandonedNotSent  = "dack-abandoned-not-sent"
	cntHubBadRID             = "hub-bad-rid"
	cntNoticeNoGrant         = "notice-no-grant"
	cntHub400Prefix          = "hub-400-"
	cntOutboxContentGone     = "outbox-content-gone"
)

// The local codes of last_error (ENG-S4): failures no hub named, each with
// a leading "-". -no_keys is the one ENG-S4 fixes: A7 found no key of the
// recipient's to seal to.
const (
	errNoKeys    = "-no_keys"
	errNetwork   = "-network"
	errNoHub     = "-no_hub"
	errBadRID    = "-hub_bad_rid"
	errDirect    = "-direct"
	errSealing   = "-seal"
	errHubPrefix = "-hub_"
	// codeKidStale is trigger T2 (ENG-R2): the row's last answer was 409
	// kid_stale, so the next attempt seals again with keys from the
	// directory. It survives a restart in last_error.
	codeKidStale = "kid_stale"
	// codeSessionUnavailable is ENG-A6.
	codeSessionUnavailable = "session_unavailable"
	// codeCredentialRefused is SSR-D11's wait.
	codeCredentialRefused = "credential_refused"
)

// lastErrorOf is last_error for code (ENG-S4): the code, then ": " and the
// reason table's English sentence for it when the table has one; never the
// hub's own sentence or a Go error, which say nothing the user can act on
// and may carry what a hub chose to put there.
func lastErrorOf(code string) string {
	if code == "" || strings.HasPrefix(code, "-") || !knownReasonCode(code) {
		return code
	}
	return code + ": " + a2ashape.Sentence(code, a2ashape.English, nil)
}

// outboxMem is the engine's state in memory (engine ENG-F1: a restart that
// loses it loses nothing that matters): the direct paths backing off, the
// kid_stale answers sent again at once, and for each version the hub holds,
// since when its status queries fail and since when it has been queued.
type outboxMem struct {
	mu sync.Mutex
	// p2p is the backoff of the direct paths by recipient AID.
	p2p map[string]p2pBackoff
	// kidStale are the times of the fast kid_stale retries by row.
	kidStale map[int64][]uint64
	// queryFail and queued are by version.
	queryFail map[verKey]uint64
	queued    map[verKey]uint64
}

// verKey names a version.
type verKey struct {
	row int64
	ver uint64
}

// p2pBackoff is one AID's direct path backing off until until; delay is the
// next wait.
type p2pBackoff struct {
	until uint64
	delay time.Duration
}

// outboxMemMax bounds each map of outboxMem: past it the map starts afresh,
// which costs at most a query or a direct send too soon.
const outboxMemMax = 1 << 16

func (m *outboxMem) p2pOpen(aid string, now uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.p2p[aid]
	return !ok || now >= b.until
}

// p2pFailed puts aid's direct path into backoff (DLV-S-30): 30 seconds,
// doubling to 10 minutes while it keeps taking bytes without a valid token.
func (m *outboxMem) p2pFailed(aid string, now uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.p2p == nil || len(m.p2p) >= outboxMemMax {
		m.p2p = map[string]p2pBackoff{}
	}
	b := m.p2p[aid]
	if b.delay == 0 {
		b.delay = p2pBackoffMin
	}
	b.until = now + uint64(b.delay.Milliseconds())
	b.delay = min(2*b.delay, p2pBackoffMax)
	m.p2p[aid] = b
}

// p2pOK ends aid's backoff: a direct send confirmed.
func (m *outboxMem) p2pOK(aid string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.p2p, aid)
}

// kidStaleFast reports whether a kid_stale answer to row may be met with a
// new version at once (FS-O2: three times an hour), and if so notes it.
func (m *outboxMem) kidStaleFast(row int64, now uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kidStale == nil || len(m.kidStale) >= outboxMemMax {
		m.kidStale = map[int64][]uint64{}
	}
	var keep []uint64
	for _, t := range m.kidStale[row] {
		if now >= t && now-t < 3600*1000 {
			keep = append(keep, t)
		}
	}
	if len(keep) >= kidStaleFastPerHour {
		m.kidStale[row] = keep
		return false
	}
	m.kidStale[row] = append(keep, now)
	return true
}

// clearKidStale forgets row's fast kid_stale retries: the hub took it.
func (m *outboxMem) clearKidStale(row int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.kidStale, row)
}

// versionState fills a version's view with what memory holds of it.
func (m *outboxMem) versionState(row int64, ver uint64) (queryFailSince, queuedSince uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := verKey{row, ver}
	return m.queryFail[k], m.queued[k]
}

// noteQuery records what a status query said of a version: queued (since
// when it has been), unknown, or no answer (failed).
func (m *outboxMem) noteQuery(row int64, ver uint64, state string, failed bool, now uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.queryFail == nil || len(m.queryFail) >= outboxMemMax {
		m.queryFail = map[verKey]uint64{}
	}
	if m.queued == nil || len(m.queued) >= outboxMemMax {
		m.queued = map[verKey]uint64{}
	}
	k := verKey{row, ver}
	if failed {
		if m.queryFail[k] == 0 {
			m.queryFail[k] = now
		}
		return
	}
	delete(m.queryFail, k)
	if state == interactions.OutboxQueued {
		if m.queued[k] == 0 {
			m.queued[k] = now
		}
	} else {
		delete(m.queued, k)
	}
}

// noteHanded forgets what a version's earlier hand-over left: a new 200
// starts its queries and its queued time afresh.
func (m *outboxMem) noteHanded(row int64, ver uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := verKey{row, ver}
	delete(m.queryFail, k)
	delete(m.queued, k)
}

// forgetRow drops what memory holds of a row that ended.
func (m *outboxMem) forgetRow(row int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.kidStale, row)
	for k := range m.queryFail {
		if k.row == row {
			delete(m.queryFail, k)
		}
	}
	for k := range m.queued {
		if k.row == row {
			delete(m.queued, k)
		}
	}
}

// configuredHub is this node's hub URL as rows record it, "" for none.
func (d *Daemon) configuredHub() string { return strings.TrimRight(d.config().HubURL, "/") }

// hubAskable reports whether the configured hub may be asked now: there is
// one, and it has not answered over another wire (core CORE-V1).
func (d *Daemon) hubAskable() bool {
	hub := d.configuredHub()
	return hub != "" && !d.hubWireStopped(hub)
}

// directTransports are the direct delivery paths transport modules added
// (p2p), in the order they were registered. The hub is not one: the engine
// sends to it itself (A11).
func (d *Daemon) directTransports() []module.Transport {
	d.transportMu.RLock()
	defer d.transportMu.RUnlock()
	return append([]module.Transport(nil), d.extraTransports...)
}

// p2pPath is the direct path that can take a message to `to` now: one that
// says it reaches it (a contact peer capability with an address, ENG-I7)
// while its AID is not backing off; nil when there is none.
func (d *Daemon) p2pPath(ctx context.Context, to string, now uint64) module.Transport {
	if !d.outboxMem.p2pOpen(to, now) {
		return nil
	}
	for _, t := range d.directTransports() {
		if t.Reachable(ctx, to) {
			return t
		}
	}
	return nil
}

// rowView is the engine's view of a stored row.
func rowView(r *interactions.OutboxRow) RowView {
	return RowView{ID: r.ID, IX: r.IX, To: r.ToAID, Type: r.Type, Kind: r.Kind, TS: r.TS, Exp: r.Exp,
		Attempts: r.Attempts, NextAt: uint64(max(r.NextAt, 0)), LastError: r.LastError, CreatedAt: uint64(max(r.CreatedAt, 0)),
		HandedAt: uint64(max(r.HandedAt, 0)), Attempting: r.Attempting, Frozen: r.Frozen,
		CheckAt: uint64(max(r.CheckAt, 0)), CheckN: r.CheckN,
		EpochSealed: EpochSealed{CurVer: r.CurVer, SealedKidNA: r.SealedKidNA},
		KELSealed:   KELSealed{SealedKSS: r.SealedKSS, SealedNoticeAt: r.SealedNoticeAt}}
}

// verViews are the engine's views of a row's versions: a version whose hub
// is not the configured one, or whose queries have failed for
// statusStaleMS, is stale (ENG-D5).
func (d *Daemon) verViews(row int64, vers []interactions.OutboxVer, hub string, now uint64) []VerView {
	out := make([]VerView, 0, len(vers))
	for _, v := range vers {
		failing, queued := d.outboxMem.versionState(row, v.Ver)
		vv := VerView{Ver: v.Ver, HandedAt: uint64(max(v.HandedAt, 0)), RID: v.RID, Hub: v.Hub, ViaHub: v.ViaHub,
			HubState: v.HubState, QueuedSince: queued}
		if v.RID != nil && (v.Hub != hub || (failing != 0 && now >= failing && now-failing >= statusStaleMS)) {
			vv.Stale = true
		}
		out = append(out, vv)
	}
	return out
}

// outboxLockKey is the attempt lock of row id.
func outboxLockKey(id int64) string { return strconv.FormatInt(id, 10) }

// attemptOutbox makes one attempt at an outbox row now (engine §2.4). It
// returns nil when the row is out of the way (confirmed, handed to the hub,
// gone), an error wrapping errUndeliverable when it was given up, an
// *outboxWaitingError when an earlier row of its task holds it back, and
// the failure otherwise.
func (d *Daemon) attemptOutbox(ctx context.Context, id int64) error {
	return d.attemptOutboxAt(ctx, id, false)
}

// attemptOutboxAt is attemptOutbox; noBackoff disregards the row's backoff,
// as the first pass after a start does (ENG-Q8).
func (d *Daemon) attemptOutboxAt(ctx context.Context, id int64, noBackoff bool) error {
	unlock := d.outboxLocks.lock(outboxLockKey(id))
	defer unlock()
	return d.attemptRowLocked(ctx, id, noBackoff)
}

// tryAttemptOutbox is attemptOutbox when no attempt at row id is under
// way, and nothing (false) when one is.
func (d *Daemon) tryAttemptOutbox(ctx context.Context, id int64) (bool, error) {
	unlock, ok := d.outboxLocks.tryLock(outboxLockKey(id))
	if !ok {
		return false, nil
	}
	defer unlock()
	return true, d.attemptRowLocked(ctx, id, false)
}

// attemptRowLocked is attemptOutboxAt under the row's lock.
func (d *Daemon) attemptRowLocked(ctx context.Context, id int64, noBackoff bool) error {
	if err := d.recoverStaleAttempt(id); err != nil {
		return err
	}
	queried := false
	for range 3 {
		row, vers, err := d.ix.OutboxState(id) // A0
		if errors.Is(err, interactions.ErrNotFound) {
			return nil // confirmed or given up meanwhile
		}
		if err != nil {
			return err
		}
		now := d.nowMS()
		hub := d.configuredHub()
		rv, vv := rowView(row), d.verViews(id, vers, hub, now)
		if noBackoff {
			rv.NextAt = 0
		}
		in := StepInput{Now: now, Gate: d.identityGate(), HubOK: d.hubAskable(), Bypass: queried}
		if in.Gate.Open {
			in.P2P = d.p2pPath(ctx, row.ToAID, now) != nil
			in.Reseal = resealNeeded(rv, vv, now, d.keyCursor())
			ahead, err := d.rowsAhead(row)
			if err != nil {
				return err
			}
			in.OrderBlocked = orderBlocked(rv, ahead)
		}
		dec := outboxStep(rv, vv, in)
		switch dec.Action {
		case stepAbandon:
			return d.abandonOutbox(row, vers, abandonDeadline(dec.Reason), nil)
		case stepWithdraw:
			return d.withdrawFrozen(row)
		case stepQuery:
			if done := d.queryOutbox(ctx, row, vers, hub); done {
				return nil
			}
			queried = true
			continue
		case stepBlocked:
			return &outboxWaitingError{cause: errors.New("an earlier message of this task is not out yet")}
		case stepWait:
			if dec.Park > rv.NextAt && !noBackoff {
				if err := d.ix.Update(func(tx *interactions.Tx) error { return tx.SetOutboxNextAt(id, int64(dec.Park)) }); err != nil {
					return err
				}
			}
			return nil
		case stepTransmit:
			return d.transmitOutbox(ctx, row, vers, dec)
		}
		return nil
	}
	return nil
}

// recoverStaleAttempt is CR2 for row id at run time: every attempt marks a
// row attempting under the row's lock, which the caller holds, so a mark
// found now was left by an attempt that ended without recording its
// outcome (its last write failed). The row counts as handed over, as after
// a restart: a byte of it may have left.
func (d *Daemon) recoverStaleAttempt(id int64) error {
	row, _, err := d.ix.OutboxState(id)
	if err != nil || !row.Attempting {
		if errors.Is(err, interactions.ErrNotFound) {
			err = nil
		}
		return err
	}
	return d.ix.Update(func(tx *interactions.Tx) error {
		_, err := tx.RecoverOutboxAttempt(id, int64(d.nowMS()))
		return err
	})
}

// rowsAhead are the rows queued before row for its task and peer.
func (d *Daemon) rowsAhead(row *interactions.OutboxRow) ([]RowView, error) {
	queue, err := d.ix.OutboxQueue(row.IX, row.ToAID)
	if err != nil {
		return nil, err
	}
	var out []RowView
	for i := range queue {
		if queue[i].ID < row.ID {
			out = append(out, rowView(&queue[i]))
		}
	}
	return out, nil
}

// attemptState is what one attempt has done so far, for A12.
type attemptState struct {
	row  *interactions.OutboxRow
	vers []interactions.OutboxVer
	now  uint64
	// marked: A8 committed, the row is attempting and A12 must clear it.
	marked bool
	// handed are the versions a byte of left the process in this attempt.
	handed []uint64
	// accepted is the hub's 200, when it gave one.
	accepted *interactions.OutboxAccepted
	// failed counts a failure: attempts + 1 and a backoff, or retryAfter,
	// or no later than retryBy (SSR-D11).
	failed     bool
	retryAfter time.Duration
	retryBy    uint64
	lastErr    string
	// acceptedCap is the held capability a 200 was given under (SSR-D11
	// item 4).
	acceptedCap *interactions.HeldCap
}

func (a *attemptState) hand(ver uint64) {
	for _, v := range a.handed {
		if v == ver {
			return
		}
	}
	a.handed = append(a.handed, ver)
}

// handedNow reports whether a byte of the row ever left the process, this
// attempt included.
func (a *attemptState) handedNow() bool {
	return len(a.handed) > 0 || versionsHanded(rowView(a.row), viewsOf(a.vers))
}

// viewsOf are versions as views without what memory holds: enough to tell
// whether any was handed over.
func viewsOf(vers []interactions.OutboxVer) []VerView {
	out := make([]VerView, 0, len(vers))
	for _, v := range vers {
		out = append(out, VerView{Ver: v.Ver, HandedAt: uint64(max(v.HandedAt, 0)), RID: v.RID})
	}
	return out
}

// fail records a failure of the attempt with last_error lastErr.
func (a *attemptState) fail(lastErr string) {
	a.failed, a.lastErr = true, lastErr
}

// capMsgKindOf is what grantFor and accessFor take a row for (SS-8
// handover): a delegation, one of the three notices, or any other row.
func capMsgKindOf(typ, kind string) capMsgKind {
	switch {
	case typ == seal.TypeDelegate:
		return capMsgDelegate
	case isNoticeKind(kind) && typ == seal.TypeStatus:
		return capMsgNotice
	}
	return capMsgOther
}

// isNoticeKind reports whether a status row is one of the three notices
// after the fact (ENG-Q7).
func isNoticeKind(kind string) bool {
	return kind == rowKindPendingApproval || kind == rowKindPendingExpired || kind == rowKindOperatorReject
}

// accessModeOf is accessFor's mode for a row (ENG-K2).
func accessModeOf(row *interactions.OutboxRow) accessMode {
	if row.Type == seal.TypeStatus && isNoticeKind(row.Kind) {
		return accessNotice
	}
	return accessOrdinary
}

// transmitOutbox is A7–A12 of an attempt that transmits.
func (d *Daemon) transmitOutbox(ctx context.Context, row *interactions.OutboxRow, vers []interactions.OutboxVer, dec StepDecision) error {
	a := &attemptState{row: row, vers: vers, now: d.nowMS()}
	sealNow, refresh := dec.Seal, lastErrorCode(row.LastError) == codeKidStale
	resentForHome, reselected, staleCommit := false, false, false
	var choice AccessChoice
rounds:
	for round := 0; round < attemptRounds; round++ {
		// A7 and A8.
		env, ver, err := d.outboxVersion(ctx, a, sealNow, refresh)
		sealNow, refresh = false, false
		switch {
		case errors.Is(err, errGrantStale) && !staleCommit:
			d.count(cntGrantStaleAtCommit)
			staleCommit, sealNow = true, true
			continue
		case errors.Is(err, interactions.ErrContentGone):
			// The task's content is gone (its data key deleted): the
			// message can no longer be sealed, and is given up (FS-8).
			d.countErr(cntOutboxContentGone, err)
			return d.abandonOutbox(row, vers, abandonDeadline(deadlineReason(a.handedNow())), a)
		case errors.Is(err, interactions.ErrNotFound):
			return nil // confirmed or given up meanwhile
		case errors.Is(err, interactions.ErrOutboxFrozen):
			// Canceled meanwhile: nothing more goes (ENG-O6); one that never
			// left is withdrawn (ENG-O5).
			return d.withdrawFrozen(row)
		case err != nil:
			switch v := resolveVerdictOf(err); {
			case v == verdictUnavailable:
				// The directory cannot be asked, or gives nothing that verifies
				// with a fresh notice: the row waits for the recipient's home
				// (KEL D-26), as a strict send queued so does (ENG-Q5).
				a.fail(lastErrorOf(reasonHomeUnreachable))
			case v != 0 || errors.Is(err, errNoRecipientKeys):
				a.fail(errNoKeys + ": " + noKeysDetail(err))
			default:
				a.fail(errSealing)
			}
			log.Printf("anet: %s: sealing %s to %s: %v", row.IX, row.Type, row.ToAID, err)
			break rounds
		}
		d.noteDelivery(row.IX)

		// A9: the credential, made again for every attempt (ENG-I2, K1).
		choice, err = d.accessChoiceFor(row.ToAID, row.IX, row.Type == seal.TypeDelegate, accessModeOf(row), a.now)
		if err != nil {
			log.Printf("anet: %s: the credential for %s to %s: %v", row.IX, row.Type, row.ToAID, err)
			a.fail(errSealing)
			break
		}
		if !choice.Send {
			// A notice with no credential of the requester's to go under is
			// not sent at all (ENG-K2), and gives up without touching the
			// task.
			d.count(cntNoticeNoGrant)
			return d.abandonOutbox(row, vers, abandonNoGrant, a)
		}
		digest, err := seal.EnvDigest(env)
		var access *seal.Access
		if err == nil {
			access, err = accessFromChoice(choice, digest)
			if err != nil && choice.Kind == seal.AccessKindCap && choice.Cap != nil && !reselected {
				// A stored capability this node cannot use (it does not
				// decode, its holder key is gone) is given up, and the
				// message goes under what accessFor chooses without it.
				log.Printf("anet: %s: give up the delivery credential from %s: %v", row.IX, row.ToAID, err)
				d.giveUpHeldCap(choice.Cap, a.now)
				reselected = true
				continue
			}
		}
		var wire []byte
		if err == nil {
			wire, err = seal.SetAccess(env, access)
		}
		if err != nil {
			log.Printf("anet: %s: the credential for %s to %s: %v", row.IX, row.Type, row.ToAID, err)
			a.fail(errSealing)
			break
		}
		hub := d.configuredHub()
		viaHub := !dec.P2POnly && hub != "" && !d.hubWireStopped(hub)
		if viaHub && choice.Kind == seal.AccessKindSession {
			// A first contact needs this node's session with its hub first;
			// without one nothing is handed over (ENG-A6).
			if _, err := d.firstContactSession(ctx, hub); err != nil {
				a.fail(codeSessionUnavailable + ": " + sessionRefusalCode(err))
				break
			}
		}

		// A cancel may have frozen the row since A8: nothing more of it
		// goes, on any path (ENG-O6).
		if d.frozenNow(row.ID) {
			break
		}
		// A10: a direct path first, with the same bytes (ENG-A3).
		if t := d.p2pPath(ctx, row.ToAID, a.now); t != nil {
			dack, handed, serr := t.Send(ctx, row.ToAID, wire)
			if handed {
				a.hand(ver)
			}
			if len(dack) > 0 {
				if o, n, ok := d.checkDAck(row.ID, a, dack); ok {
					d.outboxMem.p2pOK(row.ToAID)
					d.noteTransport(t.Name(), nil)
					return d.confirmOutbox(row.ID, o, n, cntDAckConfirmedP2P)
				}
			}
			if handed {
				// The bytes went, and no valid token came back: maybe a
				// stale address, maybe a liar (DLV-S-30). The hub is next.
				d.count(cntDAckP2PNoDAck)
				d.outboxMem.p2pFailed(row.ToAID, a.now)
			}
			d.noteTransport(t.Name(), serr)
			if dec.P2POnly || !viaHub {
				if !dec.P2POnly {
					a.fail(errDirect)
				}
				break
			}
		} else if dec.P2POnly {
			break
		}
		if !viaHub {
			if hub == "" {
				a.fail(errNoHub)
			} else {
				a.fail(errHubPrefix + "wire")
			}
			break
		}

		if d.frozenNow(row.ID) {
			break
		}
		// A11: the hub.
		res, handed, serr := d.relaySendV3(ctx, wire, choice)
		if handed {
			a.hand(ver)
		}
		ans := hubAnswerOf(res, handed, serr)
		switch ans.kind {
		case ansAccepted:
			acc := &interactions.OutboxAccepted{Ver: ver, Hub: hub, ViaHub: res.ViaHub}
			if res.HasRID {
				acc.RID = bytes.Clone(res.RID[:])
				a.lastErr = ""
			} else {
				// Taken, but under no rid this node can ask about: the
				// version is handed over without one (DLV-W-12b).
				d.count(cntHubBadRID)
				acc.RID = nil
				a.fail(errBadRID)
			}
			a.accepted = acc
			if choice.Kind == seal.AccessKindCap {
				a.acceptedCap = choice.Cap
			}
			if res.Quiet {
				d.noteQuietPeer(row.ToAID)
			} else {
				d.noteLivePeer(row.ToAID)
			}
			d.outboxMem.clearKidStale(row.ID)
			d.outboxMem.noteHanded(row.ID, ver)
		case ansKidStale:
			// T2: sealed to a key the hub no longer takes for the
			// recipient. A new version with keys from the directory goes at
			// once, three times an hour (ENG-R11, FS-O2).
			a.lastErr = lastErrorOf(codeKidStale)
			if d.outboxMem.kidStaleFast(row.ID, a.now) {
				sealNow, refresh = true, true
				continue
			}
			a.fail(lastErrorOf(codeKidStale))
		case ansAccessInvalid:
			if choice.Cap == nil || reselected {
				a.fail(lastErrorOf(codeCredentialRefused))
				break rounds
			}
			dec, err := d.credentialRefused(ctx, row.ToAID, choice.Cap)
			if err != nil {
				a.fail(lastErrorOf(codeCredentialRefused))
				break rounds
			}
			if dec.Reselect() {
				// Explained, or given up: the same version again at once,
				// under what accessFor chooses now (SSR-D11).
				reselected = true
				continue
			}
			a.fail(lastErrorOf(dec.WaitCode()))
			a.retryBy = dec.RetryBy
		case ansUnknownRecipient:
			capHome := choice.HintHome
			if choice.Kind == seal.AccessKindCap {
				capHome = choice.Home
			}
			if !resentForHome && d.refreshOnUnknown(ctx, row.ID, row.ToAID, capHome) {
				// The recipient's home moved: once more at once, not
				// counted (SSR-D12).
				resentForHome = true
				continue
			}
			return d.abandonOutbox(row, vers, abandonHub("recipient_unknown"), a)
		case ansTooLarge:
			return d.abandonOutbox(row, vers, abandonHub(ans.code), a)
		case ansBadEnvelope:
			// The hub refuses the envelope itself; an engine that sends no
			// outer exp can only be wrong here (§2.5).
			d.countErr(cntHub400Prefix+ans.code, serr)
			return d.abandonOutbox(row, vers, abandonHub("bad_envelope"), a)
		case ansWait:
			a.fail(lastErrorOf(ans.code))
			a.retryAfter = ans.retryAfter
		case ansSessionUnavailable:
			a.fail(codeSessionUnavailable + ": " + ans.code)
		case ansWire:
			a.fail(errHubPrefix + "wire")
		case ansNotSent:
			a.fail(errNetwork)
		default:
			a.fail(ans.lastErr())
			a.retryAfter = ans.retryAfter
		}
		if serr != nil && ans.kind != ansAccepted {
			log.Printf("anet: %s: %s to %s: %v", row.IX, row.Type, row.ToAID, serr)
		}
		break
	}
	if err := d.endAttempt(a); err != nil {
		return err
	}
	if row.HandedAt == 0 && len(a.handed) > 0 {
		d.kickOutbox() // what waited behind it may go now (ENG-O1)
	}
	// A cancel that came while the attempt ran froze the row: if none of
	// it left, it is withdrawn now (A12, ENG-O5).
	if err := d.withdrawFrozen(row); err != nil {
		return err
	}
	if a.failed {
		return fmt.Errorf("anet: %s to %s not delivered yet (%s)", row.Type, row.ToAID, lastErrorCode(a.lastErr))
	}
	return nil
}

// giveUpHeldCap marks the held capability c invalid (SS §4.3.8): no
// attempt chooses it again.
func (d *Daemon) giveUpHeldCap(c *interactions.HeldCap, now uint64) {
	err := d.ix.Update(func(tx *interactions.Tx) error {
		_, err := tx.MarkHeldInvalid(c.Issuer, c.Scope, c.IX, c.CapID, now)
		return err
	})
	if err != nil {
		log.Printf("anet: give up the delivery credential from %s: %v", c.Issuer, err)
	}
}

// frozenNow reports whether a cancel froze row id since it was read (an
// error reading it is taken for frozen: nothing goes on a row the store
// cannot vouch for).
func (d *Daemon) frozenNow(id int64) bool {
	frozen, err := d.ix.OutboxFrozen(id)
	return frozen || err != nil
}

// noKeysDetail is what -no_keys says of why: the directory's verdict, a
// code and no more.
func noKeysDetail(err error) string {
	if v := resolveVerdictOf(err); v != 0 {
		return v.String()
	}
	return "none_usable"
}

// sessionRefusalCode is the detail of session_unavailable (ENG-A6): the
// hub's code for refusing a session, or why there was no answer.
func sessionRefusalCode(err error) string {
	switch {
	case hubReason(err) != "":
		return hubReason(err)
	case errors.Is(err, errHubWire):
		return errHubPrefix + "wire"
	case hubStatus(err) != 0:
		return errHubPrefix + strconv.Itoa(hubStatus(err))
	}
	return errNetwork
}

// credentialRefused is SSR-D11 for a 403 access_invalid to a send under
// held: the peer's material is looked at again (reviewPeer), and the
// decision applied to the held row in a transaction of its own.
func (d *Daemon) credentialRefused(ctx context.Context, to string, held *interactions.HeldCap) (RefusalDecision, error) {
	review := d.reviewPeer(ctx, to)
	now := d.nowMS()
	var dec RefusalDecision
	err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		dec, err = accessRefusedTx(tx, held.Issuer, held.Scope, held.IX, held.CapID, review, now)
		return err
	})
	if err != nil {
		return RefusalDecision{}, err
	}
	d.noteRefusal(dec, now)
	return dec, nil
}

// outboxVersion is A7 and A8: the version to send and the transaction that
// marks the row attempting before any byte of it leaves (SI-83). With
// sealNow a new version is sealed (engine §3) — with keys fetched afresh
// from the directory when refresh (T2) — and written with its ack_key and
// the capability its grant gives, in that transaction (SSR-D10); otherwise
// the current version is sent again.
func (d *Daemon) outboxVersion(ctx context.Context, a *attemptState, sealNow, refresh bool) ([]byte, uint64, error) {
	row := a.row
	if !sealNow && row.CurVer > 0 {
		_, env, err := d.ix.OutboxContent(row.ID)
		if err != nil {
			return nil, 0, err
		}
		if err := d.ix.Update(func(tx *interactions.Tx) error { return tx.BeginOutboxAttempt(row.ID) }); err != nil {
			return nil, 0, err
		}
		a.marked = true
		return env, row.CurVer, nil
	}
	body, _, err := d.ix.OutboxContent(row.ID)
	if err != nil {
		return nil, 0, err
	}
	sv, err := d.sealVersion(ctx, row, body, refresh)
	clear(body) // ENG-R6
	if err != nil {
		return nil, 0, err
	}
	var ver uint64
	var evicted int64
	err = d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		ver, evicted, err = tx.SealOutboxVersion(row.ID, interactions.OutboxSeal{AckKey: sv.ackKey[:], Envelope: sv.env,
			KID: sv.kid, KidNA: sv.kidNA, KSS: sv.kss, NoticeAt: sv.noticeAt, Handed: a.handed, HandedAt: int64(a.now)})
		if err != nil {
			return err
		}
		return d.commitGrantTx(tx, sv.grant, a.now)
	})
	clear(sv.ackKey[:])
	if err != nil {
		return nil, 0, err
	}
	if evicted > 0 {
		d.count(cntDAckVersionEvicted)
	}
	a.marked = true
	// The views this attempt decides on follow the new version: the one
	// replaced before it was handed over is gone (ENG-S5).
	if _, vers, err := d.ix.OutboxState(row.ID); err == nil {
		a.vers = vers
	}
	row.CurVer = ver
	return sv.env, ver, nil
}

// checkDAck checks a delivery token against every version's ack_key of row
// (ENG-I4, ENG-K7): ok with its outcome and note when one made it.
func (d *Daemon) checkDAck(rowID int64, a *attemptState, dack []byte) (seal.Outcome, seal.Note, bool) {
	_, vers, err := d.ix.OutboxState(rowID)
	if err != nil {
		return 0, seal.Note{}, false
	}
	keys := ackKeysOf(vers)
	defer clearAckKeys(keys)
	_, o, n, noteOK, ok := seal.VerifyDAck(keys, dack)
	if !ok {
		d.count(cntDAckInvalid)
		return 0, seal.Note{}, false
	}
	if !noteOK {
		d.count(cntDAckNoteInvalid)
	}
	return o, n, true
}

// ackKeysOf are the ack_keys of every version, in order.
func ackKeysOf(vers []interactions.OutboxVer) []seal.AckKey {
	keys := make([]seal.AckKey, 0, len(vers))
	for _, v := range vers {
		var k seal.AckKey
		copy(k[:], v.AckKey)
		keys = append(keys, k)
	}
	return keys
}

func clearAckKeys(keys []seal.AckKey) {
	for i := range keys {
		clear(keys[i][:])
	}
}

// endAttempt is A12: what the attempt handed over, the hub's answer, the
// failure and its backoff, in one transaction; the capability a 200 was
// given under forgets its refusals in it (SSR-D11 item 4).
func (d *Daemon) endAttempt(a *attemptState) error {
	row := a.row
	e := interactions.OutboxEnd{At: int64(a.now), Handed: a.handed, Accepted: a.accepted, Attempts: row.Attempts,
		NextAt: row.NextAt, LastError: a.lastErr, CheckAt: row.CheckAt, CheckN: row.CheckN}
	if !a.failed && a.accepted == nil && len(a.handed) == 0 {
		e.LastError = row.LastError
	}
	if a.accepted != nil {
		// The hub took it: its status is asked about 2 seconds on (ENG-D1),
		// and the row is looked at no sooner.
		e.CheckN = 0
		e.CheckAt = int64(a.now + statusOffsetsMS[0])
		if a.accepted.RID == nil {
			e.CheckAt = row.CheckAt
		}
		e.NextAt = max(e.NextAt, e.CheckAt)
	}
	if a.failed {
		e.Attempts = row.Attempts + 1
		wait := retryDelay(e.Attempts)
		if a.retryAfter > 0 {
			wait = min(max(a.retryAfter, retryAfterMin), retryMaxDelay)
		}
		next := a.now + uint64(wait.Milliseconds())
		if a.retryBy != 0 && a.retryBy < next {
			next = max(a.retryBy, a.now+uint64(retryAfterMin.Milliseconds()))
		}
		// Not past the deadline: the row is given up when it passes, and
		// the task it leaves waiting fails then (§0.4).
		next = min(next, row.Exp+1)
		e.NextAt = int64(next)
		if n := e.Attempts; n == 1 || n%10 == 0 {
			log.Printf("anet: %s: %s to %s not delivered (attempt %d, next in %s): %s", row.IX, row.Type, row.ToAID, n,
				time.Duration(next-a.now)*time.Millisecond, lastErrorCode(a.lastErr))
		}
	}
	if !a.marked && len(a.handed) == 0 && a.accepted == nil {
		// Nothing was written for this attempt (A7 failed): only the
		// failure is.
		err := d.ix.Update(func(tx *interactions.Tx) error { return tx.EndOutboxAttempt(row.ID, e) })
		if errors.Is(err, interactions.ErrNotFound) {
			return nil
		}
		return err
	}
	err := d.ix.Update(func(tx *interactions.Tx) error {
		if err := tx.EndOutboxAttempt(row.ID, e); err != nil {
			return err
		}
		if c := a.acceptedCap; c != nil {
			return accessAcceptedTx(tx, c.Issuer, c.Scope, c.IX, c.CapID)
		}
		return nil
	})
	if errors.Is(err, interactions.ErrNotFound) {
		return nil // confirmed meanwhile (ENG-C3: idempotent)
	}
	if err != nil {
		return err
	}
	d.noteDelivery(row.IX)
	return nil
}

// retryDelay is the wait after the n-th failed attempt (n ≥ 1): 5 seconds,
// doubling, at most a day (engine §0.4).
func retryDelay(n int) time.Duration {
	d := retryBaseDelay
	for i := 1; i < n; i++ {
		d *= 2
		if d >= retryMaxDelay {
			return retryMaxDelay
		}
	}
	return d
}

// ---- the hub's answer (engine §2.5)

// hubAnswerKind is what a hub's answer to /relay/send makes of the attempt.
type hubAnswerKind uint8

const (
	ansAccepted           hubAnswerKind = iota + 1 // 200 queued or duplicate
	ansKidStale                                    // 409 kid_stale: trigger T2
	ansAccessInvalid                               // 403 access_invalid: SSR-D11
	ansWait                                        // 429, 507, 502 with their codes: a wait
	ansUnknownRecipient                            // 404 recipient_unknown
	ansTooLarge                                    // 413 too_large, too_large_for_stranger
	ansBadEnvelope                                 // any other 400 of /relay/send
	ansWire                                        // another wire (CORE-V1)
	ansSessionUnavailable                          // no first-contact session (ENG-A6)
	ansNotSent                                     // DNS, dial or TLS failed: nothing left
	ansTemporary                                   // any other 5xx or failure after the connection
)

// hubAnswer is one answer as the engine takes it: its kind, the hub's code,
// its Retry-After and the HTTP status.
type hubAnswer struct {
	kind       hubAnswerKind
	code       string
	status     int
	retryAfter time.Duration
}

// lastErr is last_error of a temporary answer (ENG-S4): the hub's code
// when it is one of the reason table's, else -hub_ and the status.
func (a hubAnswer) lastErr() string {
	switch {
	case a.code != "" && knownReasonCode(a.code):
		return lastErrorOf(a.code)
	case a.status != 0:
		return errHubPrefix + strconv.Itoa(a.status)
	}
	return errNetwork
}

// The hub codes of §2.5 the engine waits on.
var hubWaitCodes = map[int]map[string]bool{
	http.StatusTooManyRequests:     {"rate_limited": true, "strangers_busy": true, "redelivery_limited": true},
	http.StatusInsufficientStorage: {"mailbox_full": true},
	http.StatusBadGateway:          {"home_unreachable": true},
}

// hubAnswerOf maps relaySendV3's result to the table of engine §2.5. Only
// the hub's reason code is read, never its sentence (core G1).
func hubAnswerOf(_ relaySendResult, handed bool, err error) hubAnswer {
	if err == nil {
		return hubAnswer{kind: ansAccepted, status: http.StatusOK}
	}
	if errors.Is(err, errSessionUnavailable) {
		return hubAnswer{kind: ansSessionUnavailable, code: sessionRefusalCode(errors.Unwrap(err))}
	}
	if errors.Is(err, errHubWire) {
		return hubAnswer{kind: ansWire}
	}
	var he *hubError
	if errors.As(err, &he) {
		a := hubAnswer{code: he.reason, status: he.code}
		if wait, ok := retryAfter(err); ok {
			a.retryAfter = wait
		}
		switch {
		case he.code == http.StatusConflict && he.reason == codeKidStale:
			a.kind = ansKidStale
		case he.code == http.StatusForbidden && he.reason == "access_invalid":
			a.kind = ansAccessInvalid
		case hubWaitCodes[he.code][he.reason]:
			a.kind = ansWait
		case he.code == http.StatusNotFound && he.reason == "recipient_unknown":
			a.kind = ansUnknownRecipient
		case he.code == http.StatusRequestEntityTooLarge:
			a.kind = ansTooLarge
			if he.reason != "too_large_for_stranger" {
				a.code = "too_large"
			}
		case he.code == http.StatusBadRequest && he.reason == "wire_too_new":
			a.kind = ansWire
		case he.code == http.StatusBadRequest:
			a.kind = ansBadEnvelope
			if a.code == "" {
				a.code = "bad_request"
			}
		default:
			a.kind = ansTemporary
		}
		return a
	}
	if !handed {
		return hubAnswer{kind: ansNotSent}
	}
	return hubAnswer{kind: ansTemporary}
}

// retryAfter is the wait a hub asked for with Retry-After (seconds or an
// HTTP date), bounded to [retryAfterMin, retryMaxDelay].
func retryAfter(err error) (time.Duration, bool) {
	var he *hubError
	if !errors.As(err, &he) || he.retryAfter == "" {
		return 0, false
	}
	return parseRetryAfterHeader(he.retryAfter)
}

// parseRetryAfterHeader reads a Retry-After header, bounded to
// [retryAfterMin, retryMaxDelay].
func parseRetryAfterHeader(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	var wait time.Duration
	if secs, perr := strconv.ParseInt(v, 10, 64); perr == nil {
		if secs < 0 {
			return 0, false
		}
		wait = time.Duration(min(secs, int64(retryMaxDelay/time.Second))) * time.Second
	} else if at, perr := http.ParseTime(v); perr == nil {
		wait = time.Until(at)
	} else {
		return 0, false
	}
	return min(max(wait, retryAfterMin), retryMaxDelay), true
}

// ---- the end of a row: confirmed (engine §2.7)

// confirmOutbox ends row id confirmed by its recipient (engine ENG-C3), in
// one transaction: the row and every version go; the message it carried is
// marked anet.delivery = confirmed; a delegation's task counts its silence
// from now (ENG-T3); a refusal (outcome 2) takes effect by the table of
// §2.7. A row already gone was confirmed by a racing copy: nothing happens
// (DLV-S-18). counter is the path that confirmed it.
func (d *Daemon) confirmOutbox(id int64, o seal.Outcome, n seal.Note, counter string) error {
	var row *interactions.OutboxRow
	var publish bool
	err := d.ix.Update(func(tx *interactions.Tx) error {
		r, _, err := tx.OutboxState(id)
		if errors.Is(err, interactions.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		row = r
		publish, err = d.confirmRowTx(tx, r, o, n)
		return err
	})
	if err != nil || row == nil {
		return err
	}
	d.outboxMem.forgetRow(id)
	d.count(counter)
	if o == seal.OutcomeRefused {
		d.count(cntDAckRefused)
	}
	if publish {
		d.publishResult(row.IX)
	} else {
		d.publishState(row.IX)
	}
	d.noteDelivery(row.IX)
	d.kickOutbox() // what waited behind it may go now
	return nil
}

// confirmRowTx is the confirmation of row in tx (ENG-C3, C4): it reports
// whether the task reached a terminal state by it.
func (d *Daemon) confirmRowTx(tx *interactions.Tx, row *interactions.OutboxRow, o seal.Outcome, n seal.Note) (bool, error) {
	if ok, err := tx.DeleteOutboxRow(row.ID); err != nil || !ok {
		return false, err
	}
	msgID := hex.EncodeToString(row.MID)
	meta := map[string]any{metaDelivery: a2ashape.DeliveryConfirmed, metaDeliveryDetail: nil}
	if err := tx.MergeMessageMetaByID(row.IX, msgID, meta); err != nil && !errors.Is(err, interactions.ErrNotFound) &&
		!errors.Is(err, interactions.ErrContentGone) {
		return false, err
	}
	if row.Type == seal.TypeDelegate {
		if err := tx.TouchUpdatedAt(row.IX, time.UnixMilli(int64(d.nowMS())).UTC().Format(time.RFC3339Nano)); err != nil {
			return false, err
		}
	}
	if o != seal.OutcomeRefused {
		return false, nil
	}
	return d.applyRefusalTx(tx, row, n)
}

// applyRefusalTx is outcome 2 (engine §2.7 table): the recipient refused
// the message for good, with the note's reason ("" shows as refused).
func (d *Daemon) applyRefusalTx(tx *interactions.Tx, row *interactions.OutboxRow, n seal.Note) (bool, error) {
	reason := n.Reason
	ix, err := tx.Get(row.IX)
	if errors.Is(err, interactions.ErrNotFound) || errors.Is(err, interactions.ErrContentGone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	noteOnMessage := func() error {
		if reason == "" {
			reason = "refused"
		}
		err := tx.MergeMessageMetaByID(row.IX, hex.EncodeToString(row.MID), map[string]any{a2ashape.KeyReason: reason})
		if errors.Is(err, interactions.ErrNotFound) {
			return nil
		}
		return err
	}
	switch {
	case row.Type == seal.TypeDelegate:
		if ix.IsTerminal() {
			return false, nil
		}
		if reason == "" {
			reason = "refused"
		}
		m := map[string]any{a2ashape.KeyState: string(interactions.StateRejected), a2ashape.KeyReason: reason}
		if n.RetryAfterS > 0 {
			m[a2ashape.KeyRetryAfterMS] = int64(n.RetryAfterS) * 1000
		}
		if ix.IsCapability {
			m[a2ashape.KeyEffectStatus] = string(effect.Unavailable)
		}
		mb, _ := json.Marshal(m)
		err := tx.Finish(ix.ID, interactions.Finish{State: interactions.StateRejected,
			Result: []byte(a2ashape.Sentence(reason, a2ashape.English, nil)), Verified: interactions.VerificationUnknown,
			Meta: mb}, interactions.CausePeerEnded)
		if errors.Is(err, interactions.ErrTerminal) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if _, err := tx.DeleteOutboxFollowUps(row.IX, row.ToAID); err != nil {
			return false, err
		}
		return true, nil
	case row.Type == seal.TypeMessage && reason == "task_not_found" &&
		(row.Kind == rowKindText || row.Kind == rowKindEndRequest || row.Kind == rowKindPayment):
		if ix.IsTerminal() {
			return false, noteOnMessage()
		}
		m := map[string]any{a2ashape.KeyState: string(interactions.StateFailed), a2ashape.KeyReason: reason}
		if ix.IsCapability {
			m[a2ashape.KeyEffectStatus] = string(effect.Unverified)
		}
		mb, _ := json.Marshal(m)
		err := tx.Finish(ix.ID, interactions.Finish{State: interactions.StateFailed,
			Result: []byte(a2ashape.Sentence(reason, a2ashape.English, nil)), Verified: interactions.VerificationUnknown,
			Meta: mb}, interactions.CausePeerEnded)
		if errors.Is(err, interactions.ErrTerminal) {
			return false, noteOnMessage()
		}
		return err == nil, err
	case row.Type == seal.TypeMessage && row.Kind == rowKindCancel:
		return false, nil
	}
	return false, noteOnMessage()
}

// The message metadata the engine writes (core §2.13; DLV-P-2): the
// delivery state of a message this node sent, written when its row is
// confirmed or given up.
const (
	metaDelivery       = "anet.delivery"
	metaDeliveryDetail = "anet.delivery_detail"
)

// ---- the end of a row: given up (engine §2.8)

// abandonWhy is why a row is given up, and what that does.
type abandonWhy struct {
	// reason is anet.reason of the task it fails (when it fails one) and
	// the evidence reason.
	reason string
	// trigger is for the variant of delivery_unconfirmed (DLV-S-35):
	// a2ashape.DeliveryCauseDeadline or DeliveryCauseScan; "" otherwise.
	trigger string
	// touchTask: the task waiting on the row is ended by it. A notice with
	// no credential (ENG-K2) ends no task.
	touchTask bool
}

// abandonDeadline is the deadline (A1, ENG-T2): undeliverable or
// delivery_unconfirmed by whether it was handed over.
func abandonDeadline(reason string) abandonWhy {
	return abandonWhy{reason: reason, trigger: a2ashape.DeliveryCauseDeadline, touchTask: true}
}

// deadlineReason is the reason of a row given up without its hub's word:
// undeliverable when none of it ever left, delivery_unconfirmed otherwise.
func deadlineReason(handed bool) string {
	if handed {
		return reasonDeliveryUnconfirmed
	}
	return reasonUndeliverable
}

// abandonHub is a refusal of the hub's for good (404, 413, 400).
func abandonHub(code string) abandonWhy { return abandonWhy{reason: code, touchTask: true} }

// abandonNoGrant is a notice with no credential to go under (ENG-K2).
var abandonNoGrant = abandonWhy{reason: reasonUndeliverable}

// abandonOutbox gives row up (engine ENG-X1), in one transaction: the row,
// every version, and what goes with it (ENG-X3); the message it carried is
// marked unconfirmed; the task it leaves waiting fails (undelivered.go);
// then the evidence. a is the attempt that gave it up, nil for none: what
// it handed over counts. The error returned wraps errUndeliverable.
func (d *Daemon) abandonOutbox(row *interactions.OutboxRow, vers []interactions.OutboxVer, why abandonWhy, a *attemptState) error {
	handed := versionsHanded(rowView(row), viewsOf(vers))
	if a != nil {
		handed = a.handedNow()
	}
	var failed bool
	var dropped int64
	err := d.ix.Update(func(tx *interactions.Tx) error {
		var err error
		failed, dropped, err = d.abandonRowTx(tx, row, vers, why, handed)
		return err
	})
	if err != nil {
		return err
	}
	d.outboxMem.forgetRow(row.ID)
	d.afterAbandon(row, why.reason, handed, failed, dropped)
	return &undeliverableError{reason: why.reason, maybe: handed}
}

// abandonRowTx is the transaction of abandonOutbox. It reports whether a
// task failed by it, and how many rows went with it.
func (d *Daemon) abandonRowTx(tx *interactions.Tx, row *interactions.OutboxRow, vers []interactions.OutboxVer,
	why abandonWhy, handed bool) (failed bool, dropped int64, err error) {
	if ok, err := tx.DeleteOutboxRow(row.ID); err != nil || !ok {
		return false, 0, err
	}
	detail := a2ashape.DeliveryNotSent
	if handed {
		detail = a2ashape.DeliverySent
	}
	err = tx.MergeMessageMetaByID(row.IX, hex.EncodeToString(row.MID),
		map[string]any{metaDelivery: a2ashape.DeliveryUnconfirmed, metaDeliveryDetail: detail})
	if err != nil && !errors.Is(err, interactions.ErrNotFound) && !errors.Is(err, interactions.ErrContentGone) {
		return false, 0, err
	}
	if why.touchTask {
		if failed, err = failUndeliveredTx(tx, row, why.reason, handed); err != nil {
			return false, 0, err
		}
		if failed {
			wait := deliveryWait(rowView(row), viewsOf(vers), d.identityGate())
			if err := d.writeReasonVariantTx(tx, row.IX, abandonVariant{reason: why.reason, trigger: why.trigger,
				wait: wait, queued: anyQueued(vers), noKeys: lastErrorCode(row.LastError) == errNoKeys}); err != nil {
				return false, 0, err
			}
		}
	}
	if row.Type == seal.TypeDelegate {
		// A delegation that never left takes its whole queue with it; one
		// that left takes the follow-ups that did not, and the cancel stays
		// (ENG-X3).
		var n int64
		if handed {
			n, err = tx.DeleteOutboxFollowUps(row.IX, row.ToAID)
		} else {
			n, err = tx.DeleteOutboxQueue(row.IX, row.ToAID)
		}
		if err != nil {
			return false, 0, err
		}
		dropped = n
	}
	return failed, dropped, nil
}

// anyQueued reports whether a version was last answered queued.
func anyQueued(vers []interactions.OutboxVer) bool {
	for _, v := range vers {
		if v.HubState == interactions.OutboxQueued {
			return true
		}
	}
	return false
}

// abandonVariant is what DLV-S-35 stores with a task an abandonment fails:
// the variant inputs of engine ENG-U3.
type abandonVariant struct {
	reason, trigger, wait string
	queued, noKeys        bool
}

// writeReasonVariantTx writes the variant of the reason a task failed with
// in the abandonment's transaction (DLV-S-35): interaction.reason_variant
// and reason_arg, from a2ashape.DeliveryVariant and UndeliverableVariant of
// v.
//
// v030-todo(RC-5): the two interaction columns and their write.
func (d *Daemon) writeReasonVariantTx(tx *interactions.Tx, ix string, v abandonVariant) error {
	return nil
}

// afterAbandon is what follows an abandonment's commit: the counters, the
// evidence (core §2.14), the events.
func (d *Daemon) afterAbandon(row *interactions.OutboxRow, reason string, handed, failed bool, dropped int64) {
	if handed {
		d.count(cntDAckAbandonedSent)
	} else {
		d.count(cntDAckAbandonedNotSent)
	}
	if dropped > 0 {
		log.Printf("anet: %s: %d message(s) queued after the given-up delegation went with it", row.IX, dropped)
	}
	if d.ledger != nil {
		if _, lerr := d.ledger.Append(evtypes.DeliveryExpired, map[string]any{
			"interaction_id": row.IX, "type": row.Type, "to_aid": row.ToAID, "attempts": row.Attempts,
			"last_error": lastErrorCode(row.LastError), "reason": reason, "handed": handed,
		}); lerr != nil {
			log.Printf("anet: delivery expiry evidence: %v", lerr)
		}
	}
	log.Printf("anet: %s: %s to %s given up after %d attempts (%s)", row.IX, row.Type, row.ToAID, row.Attempts, reason)
	if failed {
		d.publishResult(row.IX)
	}
	d.noteDelivery(row.IX)
	d.kickOutbox()
}
