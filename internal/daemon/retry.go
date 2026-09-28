package daemon

// retry.go delivers the outbound retry queue (interactions outbox table,
// A2A-DESIGN §3.5 step 4 and §4.2): every message this node sends to a
// peer on an interaction — a delegation, a conversation message, a status,
// a result, a cancel (0017 Q5).
//
// A row is written in the same transaction as the state change that
// produced it, then attempted at once. A failed attempt is retried with
// exponential backoff from retryBaseDelay up to retryMaxDelay (24 hours),
// or after the hub's Retry-After when it gave one. The loop reads the
// table, so a restart resumes where the previous process stopped. Every
// attempt sends the same envelope bytes.
//
// A row is abandoned, and the abandonment recorded as evidence
// (anet.delivery.expired with a reason), when
//
//   - its deadline has passed: the envelope's exp, or for a row not yet
//     sealed the exp it will be sealed with, 14 days after it was queued.
//     The recipient would refuse the envelope as expired;
//   - the hub refused the envelope for a reason a retry cannot change: 400
//     (not an envelope it accepts), 404 (no such recipient, here or
//     federated), 413 (over its size cap), or it answered 404 for the
//     recipient's keys.
//
// A task this node asked for and whose delegation or input was abandoned
// cannot go on: it is failed with anet.reason=undeliverable
// (undelivered.go).
//
// Refusal notices never enter this queue (§2 X2).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Backoff bounds of the retry queue.
const (
	retryBaseDelay = 5 * time.Second
	retryMaxDelay  = 24 * time.Hour
	// retryScanInterval is how often the loop looks for due rows when
	// nothing kicks it.
	retryScanInterval = 5 * time.Second
	// retryAfterMin is the least wait a Retry-After is taken as, so a hub
	// answering 0 does not make the loop spin.
	retryAfterMin = time.Second
)

// EvDeliveryExpired records a queued message that was abandoned: its
// deadline passed, or the hub refused it for good. reason says which.
const EvDeliveryExpired = "anet.delivery.expired"

// Why a queued message was abandoned (EvDeliveryExpired's reason).
const (
	undeliveredExpired  = "expired"           // the deadline passed
	undeliveredBad      = "refused"           // 400: the hub does not accept the envelope
	undeliveredUnknown  = "recipient_unknown" // 404: no such recipient, or no keys for it
	undeliveredTooLarge = "too_large"         // 413: over the hub's envelope cap
)

// errUndeliverable is what an attempt at an abandoned row returns: the
// message will not be delivered, and nothing is left to retry.
var errUndeliverable = errors.New("not deliverable")

// undeliverableError is errUndeliverable with the reason and the cause.
// maybe is set when an earlier attempt may have delivered the message
// all the same (OutboxItem.MaybeDelivered): nothing more will be sent, but
// "not delivered" would be more than is known ([redteam:F12]).
type undeliverableError struct {
	reason string
	cause  error
	maybe  bool
}

func (e *undeliverableError) Error() string {
	head := "anet: not delivered (" + e.reason + ")"
	if e.maybe {
		head = "anet: delivery not confirmed (" + e.reason + "; an earlier attempt may have reached the recipient)"
	}
	if e.cause == nil {
		return head
	}
	return head + ": " + e.cause.Error()
}

func (e *undeliverableError) Is(target error) bool { return target == errUndeliverable }
func (e *undeliverableError) Unwrap() error        { return e.cause }

// retryDelay is the wait after the n-th failed attempt (n ≥ 1).
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

// wireSend is one message for queueSendAs.
type wireSend struct {
	to, typ, ix string
	body        []byte
	// mid is the inner message id; nil mints one. A message this node also
	// records passes the id it recorded (newWireMID), so the msg_id on its
	// row is the id on the wire (0017 Q9), however late it is sealed.
	mid []byte
	// pin is the peer_identity pin for a recipient resolved while sealing
	// (see sealOpts.pin).
	pin string
	// strict refuses the send, writing nothing, when sealing fails for a
	// reason other than an unreachable hub: a recipient whose keys do not
	// verify or do not exist is an error for the caller now (§3.5 step 1),
	// not a row retried for 14 days. A new delegation is strict.
	strict bool
	// sealed is an envelope the caller sealed itself (with its exp); the
	// row carries it as it is.
	sealed    []byte
	sealedExp uint64
}

// queueSend seals body to toAID and records it in the outbox inside the
// caller's transaction function. write runs in the same transaction; if it
// returns an error nothing is queued. The caller then attempts delivery
// with deliverQueued.
func (d *Daemon) queueSend(ctx context.Context, toAID, typ, ix string, body []byte,
	write func(tx *interactions.Tx) error) (int64, error) {
	return d.queueSendAs(ctx, wireSend{to: toAID, typ: typ, ix: ix, body: body}, write)
}

// queueSendAs is queueSend with the message id, pin or envelope fixed.
//
// The envelope is sealed before the transaction, because sealing may fetch
// the recipient's keys from the hub and a transaction must not wait on the
// network. A sealing failure queues the body instead, under the message id
// and deadline it will be sealed with, and the retry loop seals it later
// (unless s.strict refuses it).
func (d *Daemon) queueSendAs(ctx context.Context, s wireSend, write func(tx *interactions.Tx) error) (int64, error) {
	if s.mid == nil && len(s.sealed) == 0 {
		s.mid = seal.NewMID()
	}
	item := interactions.OutboxItem{IX: s.ix, ToAID: s.to, Type: s.typ, MID: s.mid,
		Digest: interactions.OutboxDigest(s.body)}
	if len(s.sealed) > 0 {
		item.Envelope, item.Exp = s.sealed, s.sealedExp
	} else {
		exp := d.nowMS() + messageLifetimeMS
		env, serr := d.sealEnvelopeOpts(ctx, s.to, s.typ, s.ix, s.body, sealOpts{mid: s.mid, exp: exp, pin: s.pin})
		switch {
		case serr == nil:
			item.Envelope, item.Exp = env, exp
		case s.strict && !hubUnreachable(serr):
			return 0, serr
		default:
			log.Printf("anet: %s: sealing %s to %s failed (%v); queued for a later attempt", s.ix, s.typ, s.to, serr)
			item.Body, item.Exp = s.body, exp
		}
	}
	var id int64
	err := d.ix.Update(func(tx *interactions.Tx) error {
		if write != nil {
			if err := write(tx); err != nil {
				return err
			}
		}
		var err error
		id, err = tx.EnqueueOutbox(item)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// deliverQueued makes one attempt at an outbox row now. It is called right
// after queueSend by the path that queued the row, so a message is sent
// without waiting for the loop, and by the loop for rows that are due. It
// returns an error wrapping errUndeliverable when the row was abandoned.
func (d *Daemon) deliverQueued(ctx context.Context, id int64) error {
	unlock := d.outboxLocks.lock(fmt.Sprint(id))
	defer unlock()
	it, err := d.ix.GetOutbox(id)
	if err != nil {
		return nil // delivered or dropped by another attempt
	}
	deadline := it.Deadline()
	if d.nowMS() > deadline {
		return d.abandonOutbox(it, undeliveredExpired, nil)
	}
	if len(it.Envelope) == 0 {
		env, serr := d.sealEnvelopeOpts(ctx, it.ToAID, it.Type, it.IX, it.Body, sealOpts{mid: it.MID, exp: deadline})
		if serr != nil {
			if reason, ok := permanentRefusal(serr); ok {
				return d.abandonOutbox(it, reason, serr)
			}
			return d.rescheduleOutbox(it, serr)
		}
		if err := d.ix.SetOutboxEnvelope(it.ID, env, deadline); err != nil {
			return err
		}
		it.Envelope, it.Exp = env, deadline
	}
	if maybe, err := d.deliverEnvelopeTracked(ctx, it.ToAID, it.Envelope); err != nil {
		if maybe && !it.MaybeDelivered {
			// Kept on the row: the attempt that fails for good later (the
			// hub refusing what only a direct path could carry) is not
			// the whole story ([redteam:F12]).
			if merr := d.ix.MarkOutboxMaybeDelivered(it.ID); merr != nil {
				log.Printf("anet: %s: %v", it.IX, merr)
			}
			it.MaybeDelivered = true
		}
		if reason, ok := permanentRefusal(err); ok {
			return d.abandonOutbox(it, reason, err)
		}
		return d.rescheduleOutbox(it, err)
	}
	return d.ix.DeleteOutbox(it.ID)
}

// abandonOutbox drops a row that will not be delivered, records why, and
// fails the task it left waiting (undelivered.go), in the transaction that
// drops the row.
func (d *Daemon) abandonOutbox(it *interactions.OutboxItem, reason string, cause error) error {
	u, fails := d.undeliveredTask(it)
	failed := false
	if err := d.ix.Update(func(tx *interactions.Tx) error {
		if fails {
			var err error
			if failed, err = failUndeliveredTx(tx, u, it, reason); err != nil {
				return err
			}
		}
		return tx.DeleteOutbox(it.ID)
	}); err != nil {
		return err
	}
	lastErr := it.LastError
	if cause != nil {
		lastErr = cause.Error()
	}
	if d.ledger != nil {
		if _, lerr := d.ledger.Append(EvDeliveryExpired, map[string]any{
			"interaction_id": it.IX, "type": it.Type, "to_aid": it.ToAID,
			"attempts": it.Attempts, "last_error": lastErr, "reason": reason,
		}); lerr != nil {
			log.Printf("anet: delivery expiry evidence: %v", lerr)
		}
	}
	log.Printf("anet: %s: %s to %s abandoned after %d attempts (%s)", it.IX, it.Type, it.ToAID, it.Attempts, reason)
	if failed {
		d.publishResult(it.IX)
	}
	return &undeliverableError{reason: reason, cause: cause, maybe: it.MaybeDelivered}
}

// rescheduleOutbox records a failed attempt with the next backoff, or with
// the wait the hub asked for.
func (d *Daemon) rescheduleOutbox(it *interactions.OutboxItem, cause error) error {
	n := it.Attempts + 1
	wait := retryDelay(n)
	if after, ok := retryAfter(cause); ok {
		wait = after
	}
	next := int64(d.nowMS()) + wait.Milliseconds()
	if dl := int64(it.Deadline()); next > dl {
		// Not past the deadline: a row is abandoned when its deadline
		// passes, and the task it leaves waiting failed then (0017 Q5),
		// not up to a backoff later.
		next = dl + 1
	}
	if err := d.ix.RescheduleOutbox(it.ID, n, next, cause.Error()); err != nil {
		return err
	}
	if n == 1 || n%10 == 0 {
		log.Printf("anet: %s: delivering %s to %s failed (attempt %d, next in %s): %v",
			it.IX, it.Type, it.ToAID, n, wait, cause)
	}
	return cause
}

// permanentRefusal reports whether a failed attempt is one a retry cannot
// change, and why. Only the hub's answer about this envelope counts: 400,
// 404 and 413 from /relay/send, and 404 for the recipient's keys. Any other
// status — a 404 from a mistyped hub URL on another path included — and any
// transport failure is temporary.
func permanentRefusal(err error) (string, bool) {
	var he *hubError
	if !errors.As(err, &he) {
		return "", false
	}
	if he.path == "/relay/send" {
		switch he.code {
		case http.StatusBadRequest:
			return undeliveredBad, true
		case http.StatusNotFound:
			return undeliveredUnknown, true
		case http.StatusRequestEntityTooLarge:
			return undeliveredTooLarge, true
		}
		return "", false
	}
	if he.code == http.StatusNotFound && strings.HasPrefix(he.path, "/agents/") && strings.HasSuffix(he.path, "/keys") {
		return undeliveredUnknown, true
	}
	return "", false
}

// retryAfter is the wait a hub asked for with Retry-After (seconds or an
// HTTP date), bounded to [retryAfterMin, retryMaxDelay].
func retryAfter(err error) (time.Duration, bool) {
	var he *hubError
	if !errors.As(err, &he) || he.retryAfter == "" {
		return 0, false
	}
	v := strings.TrimSpace(he.retryAfter)
	var wait time.Duration
	if secs, perr := strconv.ParseInt(v, 10, 64); perr == nil {
		if secs < 0 {
			return 0, false
		}
		wait = time.Duration(secs) * time.Second
	} else if at, perr := http.ParseTime(v); perr == nil {
		wait = time.Until(at)
	} else {
		return 0, false
	}
	return min(max(wait, retryAfterMin), retryMaxDelay), true
}

// hubUnreachable reports whether a failure is the hub not answering, or
// answering "later" (5xx, 429, 408), rather than an answer about the
// message or the recipient.
func hubUnreachable(err error) bool {
	var he *hubError
	if errors.As(err, &he) {
		return he.code >= 500 || he.code == http.StatusTooManyRequests || he.code == http.StatusRequestTimeout
	}
	var ue *url.Error
	return errors.As(err, &ue) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// kickOutbox wakes the retry loop.
func (d *Daemon) kickOutbox() {
	select {
	case d.outboxKick <- struct{}{}:
	default:
	}
}

// outboxLoop delivers due rows until ctx ends. Its first pass, at daemon
// start, attempts every queued row whatever its backoff: the conditions the
// previous process backed off from (a hub down, a peer's keys missing) are
// the ones a restart is likely to follow.
func (d *Daemon) outboxLoop(ctx context.Context) {
	t := time.NewTicker(retryScanInterval)
	defer t.Stop()
	d.flushOutboxAt(ctx, math.MaxInt64)
	for {
		d.flushOutbox(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.outboxKick:
		}
	}
}

// flushOutbox attempts every due row once.
func (d *Daemon) flushOutbox(ctx context.Context) { d.flushOutboxAt(ctx, int64(d.nowMS())) }

// flushOutboxAt attempts once every row due at or before at (unix ms).
func (d *Daemon) flushOutboxAt(ctx context.Context, at int64) {
	due, err := d.ix.DueOutboxIDs(at, 100)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("anet: read outbox: %v", err)
		}
		return
	}
	for _, id := range due {
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, relayCallTimeout)
		_ = d.deliverQueued(cctx, id)
		cancel()
	}
}
