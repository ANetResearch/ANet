package daemon

// relay.go is the daemon's client of the Hub relay, wire 3 (core §2.12; wire types in
// internal/hubapi; the send itself is relay_send.go). A requester SENDS a sealed delegation envelope into the provider's Hub mailbox,
// the provider PULLS it (relayauth v2 signed poll), completes it, and SENDS a sealed result back to
// the requester's mailbox. A single background loop polls this daemon's mailbox and runs each
// envelope through the receive pipeline (receive.go). Every envelope is encrypted to its recipient and
// signed by its sender inside the encryption, so the Hub can neither read nor forge one; it sees the
// recipient AID, the authenticated sender at send time, the time and the size.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	evidence "github.com/ANetResearch/ANet/internal/evidencev1" // v030-todo(PAY-14): evidence v2
	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// relayPollInterval is how often the background loop pulls this daemon's Hub mailbox. Kept short
// so a delegated task / reply is noticed quickly (the Hub is typically local; read commands also
// trigger a best-effort fresh pull via pollFresh, so this is mainly the idle-notice latency).
const relayPollInterval = 1 * time.Second

// relayCallTimeout bounds a single relay round-trip that may carry inline ATTACHMENTS (a poll response
// or a send can be tens of MiB). It is deliberately generous: on a constrained link a 64 MiB payload can
// take minutes, and if the transfer is cut short the message is neither acked nor delivered — it would be
// redelivered forever and wedge the mailbox (a "poison" message). The background loop and the attachment
// send paths use this; interactive freshness polls use the much shorter freshPollTimeout instead.
const relayCallTimeout = 15 * time.Minute

// freshPollTimeout bounds the best-effort poll a read command (inbox/thread/results) triggers, so a large
// inbound transfer never blocks the interactive command — big messages are left to the background loop.
const freshPollTimeout = 12 * time.Second

// Delegate builds a signed TaskDoc for goal, stores the outbound interaction, and sends the delegation
// into the provider's Hub mailbox. It returns the shared interaction_id immediately (the provider may be
// offline; pull the result later with `results`).
func (d *Daemon) Delegate(ctx context.Context, providerAID, goal string, attachPaths []string) (string, error) {
	atts, err := attachmentsFromPaths(attachPaths)
	if err != nil {
		return "", err
	}
	return d.DelegateAtts(ctx, providerAID, goal, atts)
}

// DelegateAtts is Delegate with the attachments already assembled (from CLI file paths OR web uploads).
func (d *Daemon) DelegateAtts(ctx context.Context, providerAID, goal string, atts []delegation.Attachment) (string, error) {
	return d.DelegateIn(ctx, providerAID, goal, atts, "")
}

// DelegateIn is DelegateAtts under a given A2A context id. An empty
// contextID mints one: the requester daemon owns context ids for the tasks
// it creates (A2A-DESIGN §4.1).
func (d *Daemon) DelegateIn(ctx context.Context, providerAID, goal string, atts []delegation.Attachment, contextID string) (string, error) {
	id, err := newInteractionID()
	if err != nil {
		return "", err
	}
	if err := d.delegateInWithID(ctx, id, providerAID, goal, atts, contextID, nil, nil); err != nil {
		return "", err
	}
	return id, nil
}

// delegateInWithID is DelegateIn under an interaction id the caller minted,
// so a caller learns the id of a task whose row was written even when its
// delivery then failed for good. meta is the first message's metadata (A2A
// Message.metadata, a JSON object or nil): stored on the goal message here
// and carried to the provider as DelegateReq.Metadata. cm, when the client
// named a messageId, is stored with the goal message, in the same
// transaction (clientdedupe.go).
//
// The delegation goes through the retry queue in the same transaction as
// the task and its first message are recorded (an internal note item), under the message
// id the first message is recorded with (an internal note item). An unreachable hub or
// provider only delays it; the queue fails the task, with
// anet.reason=undeliverable, if it expires or the hub refuses it for good
// (undelivered.go) — on this first attempt that is returned as an error
// wrapping errUndeliverable, after the task was recorded. A recipient whose
// keys cannot be resolved or do not verify is an error before anything is
// written (§3.5 step 1).
func (d *Daemon) delegateInWithID(ctx context.Context, id, providerAID, goal string, atts []delegation.Attachment,
	contextID string, meta []byte, cm *clientMark) error {
	hub := d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	if providerAID == d.AID() {
		return fmt.Errorf("anet: cannot delegate to yourself")
	}
	if contextID == "" {
		c, err := newContextID()
		if err != nil {
			return err
		}
		contextID = c
	}
	nonce, err := newTaskNonce()
	if err != nil {
		return err
	}
	doc, env, err := d.signTaskDoc(goal, nonce)
	if err != nil {
		return err
	}
	requestCID, err := anetcid.Sum(doc)
	if err != nil {
		return err
	}
	// The KEL travels in the sealed envelope (inner key 10), not in the body.
	dr := &delegation.DelegateReq{TaskDoc: doc, Envelope: env, InteractionID: id, Attachments: atts,
		ContextID: contextID, Metadata: meta}
	payload, err := dr.Marshal()
	if err != nil {
		return err
	}
	// The goal is the first conversation message (from us, the requester),
	// kept under the delegation's envelope id: the provider records its
	// copy under the same id.
	mid, msgID := newWireMID()
	var seq int64
	qid, err := d.queueSendAs(ctx, wireSend{to: providerAID, typ: seal.TypeDelegate, ix: id, body: payload, mid: mid,
		pin: interactions.PinOutbound, strict: true}, func(tx *interactions.Tx) error {
		if err := tx.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: providerAID,
			Goal: goal, RequestCID: requestCID, RequestDoc: doc, ContextID: contextID, TaskNonce: nonce}); err != nil {
			return err
		}
		rec := interactions.MessageRecord{InteractionID: id, SenderAID: d.AID(),
			Kind: interactions.MsgText, Body: goal, MsgID: msgID, Metadata: meta}
		if err := cm.stamp(tx, id, &rec); err != nil {
			return err
		}
		var err error
		seq, _, err = tx.AddMessageRecord(rec)
		return err
	})
	if err != nil {
		return err
	}
	if err := d.storeMsgAttachments(id, seq, atts); err != nil {
		log.Printf("anet: %s: store the delegation's attachments: %v", id, err)
	}
	d.publishMessage(id, seq, interactions.MsgText)
	d.publishState(id)
	// C5: a requester's chain should show what it asked for, not only what
	// it received.
	if _, lerr := d.ledger.Append(EvDelegationSent, map[string]any{
		"interaction_id": id,
		"provider_aid":   providerAID,
		"request_cid":    requestCID,
	}); lerr != nil {
		log.Printf("anet: delegation evidence ledger: %v", lerr)
	}
	return d.firstDelivery(ctx, id, qid)
}

// firstDelivery makes the first attempt at a new task's delegation. A
// temporary failure is left to the retry queue and is not an error; a
// refusal for good (the task is now failed) is.
func (d *Daemon) firstDelivery(ctx context.Context, ixID string, qid int64) error {
	err := d.deliverQueued(ctx, qid)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errUndeliverable):
		return fmt.Errorf("anet: %s: %w", ixID, err)
	}
	log.Printf("anet: %s: delegation queued for delivery (%v)", ixID, err)
	return nil
}

// Results pulls this daemon's mailbox once (so any pending deliverables land in the store) and then lists
// outbound delegations that carry a result and its receipt, oldest first. The state tells a completed
// task from a failed or refused one; the deliverable of a capability call carries its effect status.
func (d *Daemon) Results(ctx context.Context) ([]ResultItem, error) {
	if d.config().HubURL == "" {
		return nil, fmt.Errorf("anet: no hub configured (run `anet hub-register` first)")
	}
	d.pollFresh(ctx) // best-effort freshness; the background loop delivers anything large
	var list []*interactions.Interaction
	var since int64
	for {
		batch, err := d.ix.List(interactions.RoleOutbound, "", since, 1000)
		if err != nil {
			return nil, err
		}
		for _, ix := range batch {
			if len(ix.Receipt) > 0 {
				list = append(list, ix)
			}
		}
		if len(batch) < 1000 {
			break
		}
		since = batch[len(batch)-1].Seq
	}
	out := make([]ResultItem, 0, len(list))
	for _, ix := range list {
		rc, err := evidence.UnmarshalReceipt(ix.Receipt)
		if err != nil {
			return nil, fmt.Errorf("anet: receipt corrupt for %s: %w", ix.ID, err)
		}
		receiptCID, err := rc.CID()
		if err != nil {
			return nil, fmt.Errorf("anet: receipt CID for %s: %w", ix.ID, err)
		}
		out = append(out, ResultItem{
			InteractionID: ix.ID, Provider: ix.PeerAID, Goal: ix.Goal, State: string(ix.State),
			Result: string(ix.Result), RequestCID: ix.RequestCID, ResultCID: ix.ResultCID,
			ReceiptCID: receiptCID, Receipt: base64.StdEncoding.EncodeToString(ix.Receipt),
			Reviewed: len(ix.Review) > 0,
			// The key the receipt was checked against travels with it, so a
			// holder can re-check rather than take this node's word for it.
			ProviderKEL: d.encodedPeerKEL(ix.PeerAID),
			// Per-result, unlike ProviderKEL: that is a per-peer record, so
			// one verified interaction with a provider would otherwise make
			// every later unverified one from it look checked.
			ReceiptVerified: string(ix.ReceiptVerified),
		})
	}
	return out, nil
}

// --- relay HTTP client (wire 3, core §2.12) ---

// relayPollLimit is how many envelopes one poll asks the hub for.
const relayPollLimit = 100

// relayPoll pulls undelivered envelopes for this daemon queued after the
// mailbox id after (0: from the head). The mailbox is the authenticated
// caller's; the request carries no AID of its own.
func (d *Daemon) relayPoll(ctx context.Context, after int64) ([]hubapi.RelayMessage, error) {
	resp, err := d.relayPollPage(ctx, after)
	return resp.Messages, err
}

// relayPollPage is relayPoll with the whole answer: the page, and whether
// the mailbox holds more after it (more, FS-H5).
func (d *Daemon) relayPollPage(ctx context.Context, after int64) (hubapi.RelayPollResponse, error) {
	hub := d.config().HubURL
	if hub == "" {
		return hubapi.RelayPollResponse{}, fmt.Errorf("anet: no hub configured")
	}
	var resp hubapi.RelayPollResponse
	if err := d.hubSigned(ctx, hub, http.MethodPost, hubapi.RelayPollPath, relayauth.ActionPoll,
		hubapi.RelayPollRequest{Limit: relayPollLimit, AfterID: after}, &resp); err != nil {
		return hubapi.RelayPollResponse{}, err
	}
	// Every poll answer says how the hub confirms this node's key (KEL
	// D-50): the notice is checked before the gate sees it.
	d.noteHubNotice(resp.KeyStateNotice, resp.KeyStateNoticeError)
	return resp, nil
}

// relayCursor is where the next mailbox poll starts reading (A2A-DESIGN
// §3.6 step 9, §3.7; decision Q1).
//
// An envelope the receive pipeline refuses for a temporary reason (class
// T: a message that arrived before its delegation, a store error) is not
// acknowledged and stays in the mailbox. The hub answers oldest first, so
// without a cursor a page of such envelopes would be the whole answer to
// every poll until they turn permanent: anyone registered could hold back
// this node's newer mail for the ten-minute unknown-ix window by sending it
// a hundred messages for interactions it does not hold, and a few envelopes
// over the hub's byte budget would do the same with fewer.
//
// After a page that held something back, the next poll asks for what was
// queued after that page. When nothing is, the next round goes back to the
// head, so held envelopes are tried again every other round while the
// mailbox has nothing newer, and are acknowledged once they succeed or turn
// permanent (expired, or past the window).
//
// A round is one poll request, never two. Going back to the head in the
// same round would retry held envelopes every round, but it would also
// double this node's requests to the hub (each one a signature check and a
// liveness write there) for as long as anything is held, and a stranger can
// keep something held with one message every ten minutes.
//
// A mailbox that always has something new after the cursor would never
// send it back to the head: anyone registered can keep that so with one
// message per round, and what was held back — a message that came before
// its delegation, one a store error turned away — would wait as long as
// they keep going, and their own held messages would never be read again
// to turn permanent. So at least every relayHeadEvery rounds a round goes
// back over the held envelopes whatever lies after the cursor
// ([redteam:F24]; decision Q1: held rows are gone back to once their window
// ends).
//
// Going back has a place of its own (back), and the forward cursor (after)
// never goes back. With one cursor, a round sent to the head started the
// forward read over from there: a mailbox holding more than relayHeadEvery
// pages of held envelopes — a few dozen, when the hub's poll byte budget
// cuts pages of large envelopes down to one — was never read past them, and
// every new message waited until they turned permanent ([redteam:F24]
// bypass). Now the rounds between two that go back read on from after, so
// new mail is read within a round or two however much is held, and the
// rounds that go back walk the held envelopes page by page from the head,
// so each is read again, however steady the stream of new mail.
//
// In memory only: after a restart the first poll reads from the head,
// which is what every poll did before the cursor existed. The ids are the
// hub's, so the cursor is kept per hub URL.
type relayCursor struct {
	mu  sync.Mutex
	hub string
	// after: every envelope up to it has been read at least once; a round
	// that reads on starts after it. It only moves forward.
	after int64
	// back: where the next round that goes back over held envelopes starts
	// (0: the head).
	back int64
	// sinceBack counts the rounds since one last went back.
	sinceBack int
	// caughtUp: the last round that read on found nothing new, so the next
	// one goes back.
	caughtUp bool
}

// relayHeadEvery is the most rounds that pass without one going back over
// the held envelopes: about ten seconds at the poll interval.
const relayHeadEvery = 10

// next returns where this round reads from, and whether it goes back over
// held envelopes rather than reading on: when the last round found nothing
// new (so while the mailbox has nothing newer, every other round), or when
// relayHeadEvery rounds have passed without one.
func (c *relayCursor) next(hub string) (from int64, back bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hub != hub {
		c.hub, c.after, c.back, c.sinceBack, c.caughtUp = hub, 0, 0, 0, false
	}
	if c.caughtUp || c.sinceBack >= relayHeadEvery {
		return c.back, true
	}
	return c.after, false
}

// done records what a round read: the highest id on its page (0 for an
// empty page), whether it held anything back, and whether the page was
// full (more may follow it).
func (c *relayCursor) done(hub string, back bool, last int64, held, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hub != hub {
		return // the hub changed while the round ran
	}
	reached := last >= c.after
	if last > c.after {
		c.after = last
	}
	if !back {
		c.sinceBack++
		c.caughtUp = last == 0
		return
	}
	c.sinceBack, c.caughtUp = 0, false
	switch {
	case last == 0, reached:
		// The end of the mailbox, or as far as the forward cursor has
		// read (past it is its part): the next round that goes back
		// starts at the head.
		c.back = 0
	case held || full:
		// Read on past a page that held something back, and past a full
		// one, which may have more behind it.
		c.back = last
	default:
		// A short page with nothing held back: the end of what was held.
		// (A page the hub cut at its byte budget is short too, but all of
		// it was acknowledged, so starting over skips nothing.)
		c.back = 0
	}
}

// dackNone counts the mailbox rows acknowledged without a delivery token:
// the receive path decided them for good and made none (DLV-R-6a, §2.8).
const dackNone = "dack-none"

// ackItem is the /relay/ack item for mailbox row id, which the receive path
// decided as r (DLV-R-6a): accepted (A) or refused for good (P) gets an
// item, with its delivery token when the pipeline made one and the id alone
// otherwise, counted dack-none; a temporary refusal (T) gets none — the row
// stays in the mailbox and is read again.
func (d *Daemon) ackItem(id int64, r rxResult) (hubapi.RelayAckItem, bool) {
	if !r.ack() {
		return hubapi.RelayAckItem{}, false
	}
	if r.dack == nil {
		d.count(dackNone)
		return hubapi.RelayAckItem{ID: id}, true
	}
	return hubapi.NewRelayAckItem(id, r.dack.handle, r.dack.dack), true
}

// relayAck tells the hub these mailbox rows are decided, with the delivery
// tokens some of them carry; the hub deletes the rows and keeps the tokens
// for their senders (DLV §2.4.2). The items go in their order, at most
// hubapi.RelayAckMaxItems a request. A request that fails leaves only its
// own rows in the mailbox, and the others are still sent: the relay
// cursor's rounds that go back read those rows again, and the receive path
// answers each copy with the outcome it gave the first (engine ENG-V13).
func (d *Daemon) relayAck(ctx context.Context, items []hubapi.RelayAckItem) error {
	if len(items) == 0 {
		return nil
	}
	hub := d.config().HubURL
	if hub == "" {
		return fmt.Errorf("anet: no hub configured")
	}
	var errs []error
	for len(items) > 0 {
		batch := items[:min(len(items), hubapi.RelayAckMaxItems)]
		items = items[len(batch):]
		if err := d.hubSigned(ctx, hub, http.MethodPost, hubapi.RelayAckPath, relayauth.ActionAck,
			hubapi.RelayAckRequest{Items: batch}, nil); err != nil {
			errs = append(errs, fmt.Errorf("ack mailbox rows %d to %d: %w", batch[0].ID, batch[len(batch)-1].ID, err))
		}
	}
	return errors.Join(errs...)
}

// --- background poll loop ---

// stopRelayLoop cancels the running loop, if any, and starts none.
//
// Separate from startRelayLoop because "poll a different hub" and "poll
// nothing" are different intentions, and the second one had no way to be
// expressed: a node that left its hub kept polling it.
func (d *Daemon) stopRelayLoop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.relayStop != nil {
		d.relayStop()
		d.relayStop = nil
	}
}

// startRelayLoop cancels any running loop and starts a fresh one against hubURL, under the daemon ctx.
func (d *Daemon) startRelayLoop(hubURL string) {
	d.mu.Lock()
	if d.relayStop != nil {
		d.relayStop()
	}
	loopCtx, cancel := context.WithCancel(d.ctx)
	d.relayStop = cancel
	d.mu.Unlock()
	d.goBackground(func() { d.relayLoop(loopCtx) })
}

func (d *Daemon) relayLoop(ctx context.Context) {
	t := time.NewTicker(relayPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !d.joinRegistered() {
				// Until the hub registered this node it holds no mailbox for
				// it, and a poll would only be refused (LEC AD-2).
				continue
			}
			// Block-acquire pollMu so the loop always eventually polls (a brief freshness poll may hold
			// it). Use the generous relayCallTimeout so a large inbound attachment fully transfers rather
			// than being cut short and redelivered forever.
			d.pollMu.Lock()
			pc, cancel := context.WithTimeout(ctx, relayCallTimeout)
			if err := d.pollOnce(pc); err != nil && ctx.Err() == nil && !errors.Is(err, errHubWire) &&
				!errors.Is(err, errGateClosed) {
				// A hub of another wire is reported once by noteHubWire, and
				// a closed gate by the identity's state (KEL D-1: no poll
				// but in notice_stale); repeating either every second would
				// fill the log.
				log.Printf("anet: relay poll: %v", err)
			}
			cancel()
			d.pollMu.Unlock()
		}
	}
}

// pollFresh is a best-effort, non-blocking-if-busy mailbox poll for interactive read commands. If the
// background loop (or another handler) is already polling, it returns immediately and the caller serves
// whatever is already local — so an in-flight large transfer never blocks the command. Errors are
// intentionally swallowed (freshness is best-effort; the background loop retries).
func (d *Daemon) pollFresh(ctx context.Context) {
	if d.config().HubURL == "" || !d.joinRegistered() {
		return
	}
	if !d.pollMu.TryLock() {
		return
	}
	defer d.pollMu.Unlock()
	pc, cancel := context.WithTimeout(ctx, freshPollTimeout)
	defer cancel()
	_ = d.pollOnce(pc)
}

// pollOnce pulls the mailbox, runs each envelope through the receive
// pipeline (receive.go) and acks the ones the pipeline says to ack:
// accepted, or refused for a reason that will not change on retry, each
// with the delivery token the pipeline made for it (ackItem). An envelope
// refused for a temporary reason (a store error, a message that arrived
// before its task) stays in the mailbox and is delivered again on a later
// poll.
//
// A poll starts at the relay cursor (relayCursor), so envelopes held back
// in an earlier round do not stand in front of newer ones; when nothing is
// queued after the cursor, and in any case every relayHeadEvery rounds, a
// round goes back over the held envelopes instead.
//
// Each round also tells the epoch key ring what the mailbox holds (FS-K9 to
// FS-K11; noteRound): the kid of every envelope it held back, and, when the
// hub said nothing more is queued and every row it read was acknowledged,
// that the mailbox was drained as of the moment the round asked.
func (d *Daemon) pollOnce(ctx context.Context) error {
	hub := d.config().HubURL
	from, back := d.relayCur.next(hub)
	askedAt := d.nowMS()
	resp, err := d.relayPollPage(ctx, from)
	if err != nil {
		return err
	}
	msgs := resp.Messages
	var items []hubapi.RelayAckItem
	var last int64
	held := false
	round := pollRound{hub: hub, from: from, more: resp.More, askedAt: askedAt, kids: map[int64][]byte{}}
	for _, m := range msgs {
		last = max(last, m.ID)
		round.read = append(round.read, m.ID)
		var r rxResult
		var kid []byte
		if env, derr := hubapi.DecodeB64(m.Envelope, -1); derr != nil {
			r = d.drop(dropBadTransport, derr) // not strict base64url (core G2) — drop
		} else {
			r = d.receiveEnvelope(ctx, env)
			kid = outerKID(env)
		}
		if it, ok := d.ackItem(m.ID, r); ok {
			items = append(items, it)
			round.acked = append(round.acked, m.ID)
		} else {
			held = true
			round.kids[m.ID] = kid
		}
	}
	d.relayCur.done(hub, back, last, held, len(msgs) >= relayPollLimit)
	ackErr := d.relayAck(ctx, items)
	round.ackOK = ackErr == nil
	d.drain.noteRound(round)
	return ackErr
}

// outerKID is the kid of the epoch key an envelope is sealed to (outer key
// 4), or nil when its outer does not parse.
func outerKID(env []byte) []byte {
	o, err := seal.ParseOuter(env)
	if err != nil {
		return nil
	}
	return o.KID
}

// drainState is what the polls of the hub mailboxes tell the epoch key ring
// (FS-K9, FS-K10). In memory only: after a restart no hub has a drain mark,
// so no key goes early right after a start, and nothing is held until a
// round finds it again; the two start empty together.
type drainState struct {
	mu sync.Mutex
	// marks is each polled hub's drainMark: the local time at which a round
	// asked whose answer said nothing more was queued (more false) and whose
	// rows were all acknowledged, or which had none to acknowledge. Every
	// row the hub accepted before that moment was read at least once and is
	// either gone from the mailbox or held back.
	marks map[string]uint64
	// held is the outer kid of each mailbox row a round held back (a
	// temporary refusal, not acknowledged), by hub and row id, until a
	// round acknowledges it or finds it gone.
	held map[heldRow][]byte
}

// heldRow is a mailbox row of one hub.
type heldRow struct {
	hub string
	id  int64
}

// pollRound is what one poll round read and did.
type pollRound struct {
	hub     string
	from    int64   // after_id of the request
	more    bool    // the hub said more is queued after the page
	askedAt uint64  // local time when the request was made
	read    []int64 // the ids on the page
	acked   []int64 // the ids acknowledged (in the ack request)
	kids    map[int64][]byte
	ackOK   bool // the ack request succeeded, or there was none
}

// noteRound records a poll round (FS-K9, FS-K10). A held row is forgotten
// once a round acknowledges it, or once a round reads past its id without
// finding it: the hub holds it no more (it expired, or was acknowledged).
func (s *drainState) noteRound(r pollRound) {
	hub := strings.TrimRight(r.hub, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held == nil {
		s.held, s.marks = map[heldRow][]byte{}, map[string]uint64{}
	}
	onPage := map[int64]bool{}
	var last int64
	for _, id := range r.read {
		onPage[id] = true
		last = max(last, id)
	}
	for row := range s.held {
		if row.hub == hub && row.id > r.from && (row.id <= last || !r.more) && !onPage[row.id] {
			delete(s.held, row)
		}
	}
	for id, kid := range r.kids {
		s.held[heldRow{hub, id}] = kid
	}
	if r.ackOK {
		for _, id := range r.acked {
			delete(s.held, heldRow{hub, id})
		}
	}
	if !r.more && r.ackOK && r.askedAt > s.marks[hub] {
		s.marks[hub] = r.askedAt
	}
}

// drainInputs is the drainMark and heldKids the deletion rule takes
// (epochKeyDeletable): for a node without a hub, the largest uint64 and none
// (FS-K8); otherwise the configured hub's mark (0 before its first drained
// round) and the kids it holds back. The marks and rows are kept per hub,
// so that the old hub drained after a hub change (FS-K6a, KEL D-32) can
// join: the mark is then the least of the polled hubs' and the kids are
// every polled hub's.
func (d *Daemon) drainInputs() (uint64, kidSet) {
	hub := d.config().HubURL
	if hub == "" {
		return math.MaxUint64, nil
	}
	key := strings.TrimRight(hub, "/")
	s := &d.drain
	s.mu.Lock()
	defer s.mu.Unlock()
	held := kidSet{}
	for row, kid := range s.held {
		if row.hub == key && kid != nil {
			held[string(kid)] = struct{}{}
		}
	}
	return s.marks[key], held
}

// kickAutoReply wakes the auto-reply loop immediately (non-blocking; coalesces bursts). No-op when the
// channel is full (a wake is already pending) or auto-reply is off (the loop just isn't selecting on it).
func (d *Daemon) kickAutoReply() {
	select {
	case d.autoReplyKick <- struct{}{}:
	default:
	}
	d.inFeed.wake() // and the modules' A2A backends (inbound_tasks.go)
}

// withServedCapabilities folds this node's actual capability ids into what
// it advertises.
//
// The advertised list and the served list were two separate things kept in
// step by hand, and nothing checked. A node could register "digest" while
// its provider answered "text.digest" — harmless while discovery searched
// prose, because "digest" is a substring of the goal text and the caller
// found it anyway. The moment discovery became exact, that node stopped
// being findable for the thing it actually does, and the directory was
// confidently wrong rather than vague.
//
// Operator labels are kept alongside rather than replaced. "coding" is not
// a capability id and is still how a person describes what they offer;
// what changes is that the ids a provider will actually answer are in
// there too, put there by the daemon that knows them.
func withServedCapabilities(declared []string, reg *provider.Registry) []string {
	if reg == nil {
		return declared
	}
	served := reg.Capabilities()
	if len(served) == 0 {
		return declared
	}
	seen := make(map[string]bool, len(declared)+len(served))
	out := make([]string, 0, len(declared)+len(served))
	for _, c := range append(append([]string{}, declared...), served...) {
		if c = strings.TrimSpace(c); c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}
