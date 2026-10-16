package daemon

// review.go is the daemon's side of evidence v2 (HPP §4.3.6, PAY-V6 to PAY-V9):
//
//   - the task binding a delegation carries (newTaskBinding): the nonce, the
//     commitment to the requester and the public half of a one-time review
//     key, with the seed and the salt they were made from kept on the task;
//   - the receipt a provider signs over the task it completed (signReceipt);
//   - the review a requester signs (SubmitReview), queued in review_outbox and
//     uploaded by reviewLoop at a random time within six hours, later when the
//     account is still on probation.
//
// The upload names the reviewer to the hub: it is a signed request (relayauth
// "review"), not an anonymous one. What the hub keeps of it is the hub's own
// concern (HPP SI-48); the delay only makes it harder to match a review to an
// interaction by its time (ux review.queued).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// EvReviewUploaded records a review the hub took (core §2.14): {ix, subject, rating, day}.
const EvReviewUploaded = evtypes.ReviewUploaded

const (
	// reviewWindow is how long after a task ends it can be reviewed (HPP PAY-V1 step 7).
	reviewWindow = 30 * 24 * time.Hour
	// reviewMaxDelay bounds the random wait before an upload (PAY-V9).
	reviewMaxDelay = 6 * time.Hour
	// reviewGiveUpAfter is how long an upload is retried before it is given up (PAY-V9).
	reviewGiveUpAfter = 7 * 24 * time.Hour
	// reviewBackoffMin and reviewBackoffMax bound the wait between two failed attempts.
	reviewBackoffMin = 5 * time.Minute
	reviewBackoffMax = 6 * time.Hour
	// reviewSweepEvery is how often the outbox is looked at.
	reviewSweepEvery = time.Minute
	// reviewBatch is how many reviews one sweep uploads.
	reviewBatch = 10
)

// taskBinding is the binding of one new delegation (PAY-V6): the contexts that go into the TaskDoc,
// and the one-time inputs the requester keeps on the task.
type taskBinding struct {
	nonce       string
	seed, rsalt []byte
	contexts    []tsir.Context
}

// newTaskBinding makes the binding of a delegation this node is about to send: a fresh nonce, a
// 32-byte seed for the review key and a 32-byte salt for requester_commit.
func (d *Daemon) newTaskBinding() (*taskBinding, error) { return makeTaskBinding(d.AID()) }

// makeTaskBinding is newTaskBinding for the requester aid.
func makeTaskBinding(aid string) (*taskBinding, error) {
	nonce, err := newTaskNonce()
	if err != nil {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	rsalt := make([]byte, evidence.RSaltSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	if _, err := rand.Read(rsalt); err != nil {
		return nil, err
	}
	commit, err := evidence.RequesterCommit(aid, rsalt)
	if err != nil {
		return nil, err
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	ctxs, err := (&delegation.TaskBinding{Nonce: nonce, RequesterCommit: commit, ReviewKey: pub}).Contexts()
	if err != nil {
		return nil, err
	}
	return &taskBinding{nonce: nonce, seed: seed, rsalt: rsalt, contexts: ctxs}, nil
}

// taskBindingOf reads the binding out of the TaskDoc bytes a task was delegated with.
func taskBindingOf(doc []byte) (*delegation.TaskBinding, error) {
	var td tsir.TaskDoc
	if err := coredet.Unmarshal(doc, &td); err != nil {
		return nil, fmt.Errorf("anet: the stored TaskDoc does not decode: %w", err)
	}
	return delegation.ReadTaskBinding(&td)
}

// dayOf is the UTC day number of a unix-millisecond time (CORE-E6).
func dayOf(ms uint64) uint64 { return ms / evidence.DayMS }

// signReceipt is this node's receipt, as a provider, for the inbound task ix whose result is
// resultCID (PAY-V7): the two commitments and the review key are copied from the TaskDoc, ix_commit
// is computed from the interaction id and the TaskDoc's nonce. It returns the receipt as it travels.
func (d *Daemon) signReceipt(ix *interactions.Interaction, resultCID string) ([]byte, error) {
	b, err := taskBindingOf(ix.RequestDoc)
	if err != nil {
		return nil, err
	}
	ixc, err := b.IXCommit(ix.ID)
	if err != nil {
		return nil, err
	}
	requestCID := ix.RequestCID
	if requestCID == "" {
		// A row written without it: the CID is that of the TaskDoc it holds.
		if requestCID, err = anetcid.Sum(ix.RequestDoc); err != nil {
			return nil, err
		}
	}
	body := evidence.ReceiptBody{
		IXCommit: ixc, RequesterCommit: b.RequesterCommit, ReviewKey: b.ReviewKey,
		Provider: d.AID(), RequestCID: requestCID, ResultCID: resultCID,
		CompletedDay: dayOf(d.nowMS()),
	}
	w, err := body.Sign(d.self)
	if err != nil {
		return nil, err
	}
	return w.Marshal()
}

// ixCreatedMS is the time (unix ms) an interaction row was created, or 0.
func ixCreatedMS(ix *interactions.Interaction) uint64 {
	t, err := time.Parse(time.RFC3339Nano, ix.CreatedAt)
	if err != nil || t.UnixMilli() < 0 {
		return 0
	}
	return uint64(t.UnixMilli())
}

// ReviewResult is the outcome of signing a review.
type ReviewResult struct {
	InteractionID string
	Subject       string
	Rating        int
	// DueAt is the earliest time (unix ms) the review is uploaded.
	DueAt int64
	// ProbationUntil (unix ms) is set when the account is on probation: nothing is uploaded before it.
	ProbationUntil int64
}

// SubmitReview signs a rating of the provider for a finished outbound task (PAY-V9) and queues it
// for upload: the task has ended, its receipt was checked, and it ended no more than 30 days ago.
// Nothing is sent here.
func (d *Daemon) SubmitReview(interactionID string, rating int, comment string) (ReviewResult, error) {
	var zero ReviewResult
	ix, err := d.ix.Get(interactionID)
	if err != nil {
		return zero, err
	}
	if ix.Role != interactions.RoleOutbound {
		return zero, fmt.Errorf("anet: %s is not an outbound delegation", interactionID)
	}
	if len(ix.Receipt) == 0 {
		return zero, fmt.Errorf("anet: no receipt for %s yet (run `results` first)", interactionID)
	}
	if ix.ReceiptVerified == interactions.VerificationMismatch {
		// The receipt covers a conversation that changes what this node said: a review would vouch
		// for it (CUX-R6 item 4).
		return zero, a2ashape.Refuse(reasonReceiptMismatch, nil)
	}
	if len(ix.Review) > 0 {
		return zero, a2ashape.Refuse("review_exists", nil)
	}
	if !ix.IsTerminal() {
		return zero, fmt.Errorf("anet: %s has not ended yet; review it once it has", interactionID)
	}
	now := int64(d.nowMS())
	if now-ix.StateAt > reviewWindow.Milliseconds() {
		return zero, a2ashape.Refuse("review_window_closed", nil)
	}
	if rating < evidence.RatingMin || rating > evidence.RatingMax {
		return zero, fmt.Errorf("anet: rating must be %d..%d", evidence.RatingMin, evidence.RatingMax)
	}
	if utf8.RuneCountInString(comment) > evidence.MaxCommentChars {
		return zero, fmt.Errorf("anet: the comment is longer than %d characters", evidence.MaxCommentChars)
	}
	rw, err := evidence.UnmarshalReceiptWire(ix.Receipt)
	if err != nil {
		return zero, a2ashape.Refuse("receipt_not_reviewable", nil)
	}
	rc, err := rw.Open()
	if err != nil {
		return zero, a2ashape.Refuse("receipt_not_reviewable", nil)
	}
	if ix.ReceiptVerified != interactions.VerificationVerified || len(ix.ReviewSeed) != ed25519.SeedSize ||
		len(ix.ReviewRSalt) != evidence.RSaltSize {
		// Not checked against the provider's keys, or the one-time inputs are spent: the hub would
		// refuse it, and retrying cannot help.
		return zero, a2ashape.Refuse("receipt_not_reviewable", nil)
	}
	key := ed25519.NewKeyFromSeed(ix.ReviewSeed)
	commit, err := evidence.RequesterCommit(d.AID(), ix.ReviewRSalt)
	if err != nil {
		return zero, err
	}
	if err := receiptBindsTask(rc, ix, key.Public().(ed25519.PublicKey), commit); err != nil {
		return zero, err
	}
	receiptCID, err := rw.CID()
	if err != nil {
		return zero, err
	}
	rv := evidence.ReviewBody{ReceiptCID: receiptCID, Subject: ix.PeerAID, Rating: uint64(rating),
		Comment: comment, CreatedDay: dayOf(uint64(now))}
	rvw, err := rv.Sign(key)
	if err != nil {
		return zero, err
	}
	rvBytes, err := rvw.Marshal()
	if err != nil {
		return zero, err
	}
	due, until := d.reviewDue(now)
	if err := d.ix.QueueReview(interactionID, ix.Receipt, rvBytes, due); err != nil {
		if errors.Is(err, interactions.ErrReviewQueued) {
			return zero, a2ashape.Refuse("review_exists", nil)
		}
		return zero, err
	}
	return ReviewResult{InteractionID: interactionID, Subject: ix.PeerAID, Rating: rating, DueAt: due, ProbationUntil: until}, nil
}

// reviewDue is when a review signed at now (unix ms) is first uploaded: a random time within six
// hours, and not before the account's probation ends (PAY-V9). until is the end of the probation,
// or 0. The account is asked for once and a hub that does not answer counts as no probation: the
// hub's own refusal (probation) puts the upload off again.
func (d *Daemon) reviewDue(now int64) (due, until int64) {
	due = now + randBelow(reviewMaxDelay.Milliseconds()+1)
	if d.config().HubURL == "" {
		return due, 0
	}
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	acct, err := d.HubAccount(ctx)
	if err != nil || acct.ProbationUntil == nil {
		return due, 0
	}
	p := *acct.ProbationUntil
	if p > uint64(now) && p < 1<<62 {
		until = int64(p)
		if until > due {
			due = until
		}
	}
	return due, until
}

// randBelow is a uniform random number in [0, n).
func randBelow(n int64) int64 {
	if n <= 1 {
		return 0
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return int64(binary.BigEndian.Uint64(b[:]) % uint64(n))
}

// receiptBindsTask checks, before this node signs a review anchored to a receipt, that the receipt
// is about the task it is stored on: the provider the task went to, the review key and the
// requester commitment this node chose, and the request and the result it holds. The result check
// already made the same comparisons when the receipt arrived; a row stored by another path is
// checked here too, because the review is what vouches for the receipt to a third party.
func receiptBindsTask(rc *evidence.ReceiptBody, ix *interactions.Interaction, reviewKey ed25519.PublicKey, commit []byte) error {
	notReviewable := a2ashape.Refuse("receipt_not_reviewable", nil)
	switch {
	case rc.Provider != ix.PeerAID:
		return notReviewable
	case string(rc.ReviewKey) != string(reviewKey):
		return notReviewable
	case string(rc.RequesterCommit) != string(commit):
		return notReviewable
	case ix.RequestCID != "" && rc.RequestCID != ix.RequestCID:
		return notReviewable
	case ix.ResultCID != "" && rc.ResultCID != ix.ResultCID:
		return notReviewable
	}
	return nil
}

// reviewLoop uploads the queued reviews that are due, once a minute, until ctx ends.
func (d *Daemon) reviewLoop(ctx context.Context) {
	t := time.NewTicker(reviewSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.uploadDueReviews(ctx)
		}
	}
}

// uploadDueReviews uploads the reviews due now, and spends the one-time inputs of the tasks that
// ended more than 30 days ago.
func (d *Daemon) uploadDueReviews(ctx context.Context) int {
	if d.ix == nil {
		return 0
	}
	now := int64(d.nowMS())
	if _, err := d.ix.PurgeReviewSecrets(now - reviewWindow.Milliseconds()); err != nil {
		log.Printf("anet: spend the review inputs: %v", err)
	}
	hub := d.config().HubURL
	if hub == "" {
		return 0
	}
	rows, err := d.ix.DueReviews(now, reviewBatch)
	if err != nil {
		log.Printf("anet: review outbox: %v", err)
		return 0
	}
	done := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		if d.uploadReviewRow(ctx, hub, row, now) {
			done++
		}
	}
	return done
}

// uploadReviewRow makes one upload attempt of a queued review and records the outcome (PAY-V9):
// stored, or already held by the hub: done; a refusal that retrying cannot change: given up;
// anything else: tried again later, with a growing wait, until seven days have passed.
func (d *Daemon) uploadReviewRow(ctx context.Context, hub string, row interactions.ReviewOutboxRow, now int64) bool {
	ix, err := d.ix.Get(row.IX)
	if err != nil || len(ix.ReviewRSalt) != evidence.RSaltSize {
		// The task's content was deleted, or its inputs are spent: there is nothing to prove the
		// requester with.
		d.giveUpReview(row.IX, "the task's review inputs are gone")
		return false
	}
	body := hubapi.UploadReviewRequest{
		Receipt: hubapi.B64(row.Receipt), Review: hubapi.B64(row.Review), RSalt: hubapi.B64(ix.ReviewRSalt),
	}
	var resp hubapi.UploadReviewResponse
	cctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	err = d.hubSigned(cctx, hub, "POST", "/reviews", relayauth.ActionReview, body, &resp)
	switch {
	case err == nil:
		d.reviewStored(ctx, hub, row, ix, resp.Attestation)
		return true
	case hubReason(err) == "review_exists":
		d.reviewStored(ctx, hub, row, ix, "")
		return true
	case reviewFutile(hubReason(err)):
		d.giveUpReview(row.IX, "the hub refused it: "+hubReason(err))
		return false
	}
	if now-row.FirstAt > reviewGiveUpAfter.Milliseconds() {
		d.giveUpReview(row.IX, "not uploaded within seven days")
		return false
	}
	wait := reviewBackoffMin << min(row.Attempts, 10)
	if wait > reviewBackoffMax || wait <= 0 {
		wait = reviewBackoffMax
	}
	if hubReason(err) == "probation" {
		wait = time.Hour
	}
	log.Printf("anet: %s: review not uploaded yet (%v); trying again in %s", row.IX, err, wait.Round(time.Minute))
	if rerr := d.ix.RetryReview(row.IX, now+wait.Milliseconds()); rerr != nil {
		log.Printf("anet: review outbox: %v", rerr)
	}
	return false
}

// reviewFutile reports whether the hub's refusal code is one a retry cannot change (core §2.15.1).
func reviewFutile(reason string) bool {
	switch reason {
	case "requester_commit_mismatch", "receipt_not_reviewable", "review_window_closed", "bad_request":
		return true
	}
	return false
}

// giveUpReview removes a review from the outbox for good; doctor reports it (payments.reviews).
func (d *Daemon) giveUpReview(id, why string) {
	log.Printf("anet: %s: review not uploaded, given up: %s", id, why)
	if err := d.ix.DropReview(id); err != nil {
		log.Printf("anet: review outbox: %v", err)
	}
	d.reviewsGivenUp.Add(1)
}

// reviewStored finishes a review the hub took: the outbox row and the one-time inputs go, and the
// evidence chain records it. attestation is the hub's (base64url), or "" when the hub already held
// the review. A stored review whose attestation does not verify is still stored; it is said in the
// log, because the hub's word is then all there is.
func (d *Daemon) reviewStored(ctx context.Context, hub string, row interactions.ReviewOutboxRow,
	ix *interactions.Interaction, attestation string) {
	day := dayOf(d.nowMS())
	if attestation != "" {
		if att, err := d.checkAttestation(ctx, hub, row, attestation); err != nil {
			log.Printf("anet: %s: the hub's attestation of the review does not verify: %v", row.IX, err)
		} else {
			day = att.Day
		}
	}
	if err := d.ix.FinishReview(row.IX); err != nil {
		log.Printf("anet: review outbox: %v", err)
		return
	}
	rating := uint64(0)
	if w, err := evidence.UnmarshalReviewWire(row.Review); err == nil {
		if b, err := w.Open(); err == nil {
			rating = b.Rating
		}
	}
	if d.ledger == nil {
		return
	}
	if _, err := d.ledger.Append(EvReviewUploaded, map[string]any{
		"ix": row.IX, "subject": ix.PeerAID, "rating": rating, "day": day,
	}); err != nil {
		log.Printf("anet: review evidence: %v", err)
	}
}

// checkAttestation verifies the hub's attestation of a review against the hub's key history at the
// start of the day it names (core CORE-L8), and that it is about this review and this receipt.
func (d *Daemon) checkAttestation(ctx context.Context, hub string, row interactions.ReviewOutboxRow, b64 string) (*evidence.ReviewAttBody, error) {
	raw, err := hubapi.DecodeB64(b64, -1)
	if err != nil {
		return nil, err
	}
	w, err := evidence.UnmarshalReviewAttWire(raw)
	if err != nil {
		return nil, err
	}
	body, err := w.Open()
	if err != nil {
		return nil, err
	}
	at, ok := evidence.DayStart(body.Day)
	if !ok {
		return nil, fmt.Errorf("the attestation's day %d is not a day", body.Day)
	}
	hubAID, kel, err := d.hubIdentity(ctx, hub)
	if err != nil {
		return nil, err
	}
	if body.Hub != hubAID {
		return nil, fmt.Errorf("it is signed for %s, not for this node's hub", body.Hub)
	}
	if _, err := w.Verify(kel, at); err != nil {
		return nil, err
	}
	rv, err := evidence.UnmarshalReviewWire(row.Review)
	if err != nil {
		return nil, err
	}
	reviewCID, err := rv.CID()
	if err != nil {
		return nil, err
	}
	rw, err := evidence.UnmarshalReceiptWire(row.Receipt)
	if err != nil {
		return nil, err
	}
	receiptCID, err := rw.CID()
	if err != nil {
		return nil, err
	}
	if body.ReviewCID != reviewCID || body.ReceiptCID != receiptCID {
		return nil, fmt.Errorf("it names another review or receipt")
	}
	return body, nil
}

// reviewsStatus is the part of /status?doctor that payments.reviews reads (HPP §5.6): how many
// reviews wait, how long the oldest has, and whether any was given up since this daemon started.
func (d *Daemon) reviewsStatus() map[string]any {
	if d.ix == nil {
		return nil
	}
	rows, err := d.ix.PendingReviews()
	if err != nil {
		return nil
	}
	given := d.reviewsGivenUp.Load()
	if len(rows) == 0 && given == 0 {
		return nil
	}
	out := map[string]any{"pending": len(rows), "given_up": given}
	if len(rows) > 0 {
		out["oldest"] = rows[0].FirstAt
	}
	return out
}
