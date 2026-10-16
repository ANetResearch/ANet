package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// reviewableTask runs a text task from a requester to a provider that has ended and whose receipt the
// requester checked.
func reviewableTask(t *testing.T) (srvURL string, req, prov *Daemon, id string) {
	t.Helper()
	return reviewableTaskFor(t, "a goal")
}

// reviewableTaskFor is reviewableTask for a task with the goal given.
func reviewableTaskFor(t *testing.T, goal string) (srvURL string, req, prov *Daemon, id string) {
	t.Helper()
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	id, err := req.Delegate(ctx, prov.AID(), goal, nil)
	if err != nil {
		t.Fatal(err)
	}
	pollEach(t, prov)
	if err := prov.SendMessage(ctx, id, "an answer", nil); err != nil {
		t.Fatal(err)
	}
	pollEach(t, req)
	if err := req.RequestEnd(ctx, id); err != nil {
		t.Fatal(err)
	}
	pollEach(t, prov)
	if _, err := req.Results(ctx); err != nil {
		t.Fatal(err)
	}
	if ix := mustIX(t, req, id); ix.ReceiptVerified != interactions.VerificationVerified || len(ix.Receipt) == 0 {
		t.Fatalf("receipt %q (%d bytes), want verified", ix.ReceiptVerified, len(ix.Receipt))
	}
	return srv.URL, req, prov, id
}

// A delegation that does not carry the task binding is a bad request, for a text task and for a
// capability call alike (CORE-K7, HPP PAY-V7).
func TestADelegationWithoutTheTaskBindingIsABadRequest(t *testing.T) {
	_, req, prov := registeredPair(t)
	for name, task := range map[string]tsir.Task{
		"text": {Intent: tsir.Intent{Summary: "g", Body: "g"}},
		"capability": {Intent: tsir.Intent{Summary: "g", Body: "g"},
			Requires: []tsir.Require{{ID: "text.free", Type: RequireTypeCapability, Necessity: "must"}},
			Contexts: []tsir.Context{{Key: "args", Value: "{}", Format: "json"}}},
	} {
		td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{task}}
		if err := td.Sign(req.self); err != nil {
			t.Fatal(err)
		}
		doc, err := coredet.Marshal(td)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := newInteractionID()
		body := mustMarshal(t, &delegation.DelegateReq{TaskDoc: doc, Envelope: td.Envelope, InteractionID: id})
		r := receive(t, prov, sealFrom(t, req, prov, seal.TypeDelegate, id, body))
		if r.class == rxAccepted || r.reason != dropRefusedPrefix+reasonBadRequest {
			t.Errorf("%s without the binding: %+v, want %s", name, r, reasonBadRequest)
		}
		if _, err := prov.ix.Get(id); err == nil {
			t.Errorf("%s without the binding was stored", name)
		}
	}
}

// The receipt a provider signs carries the commitments of the TaskDoc it answers and names neither
// the requester nor the interaction (core §2.9).
func TestTheReceiptCarriesTheTaskBindingAndNoOneElse(t *testing.T) {
	_, req, prov, id := reviewableTask(t)
	rix := mustIX(t, req, id)
	w, err := evidence.UnmarshalReceiptWire(rix.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := w.Open()
	if err != nil {
		t.Fatal(err)
	}
	b, err := taskBindingOf(rix.RequestDoc)
	if err != nil {
		t.Fatal(err)
	}
	ixc, _ := b.IXCommit(id)
	if rc.Provider != prov.AID() || string(rc.IXCommit) != string(ixc) ||
		string(rc.RequesterCommit) != string(b.RequesterCommit) || string(rc.ReviewKey) != string(b.ReviewKey) ||
		rc.RequestCID != rix.RequestCID || rc.ResultCID != rix.ResultCID {
		t.Fatalf("receipt %+v does not carry the binding of %s", rc, id)
	}
	for _, s := range []string{req.AID(), id} {
		if bytes.Contains(rix.Receipt, []byte(s)) {
			t.Fatalf("the receipt names %q", s)
		}
	}
	if len(rix.ReviewSeed) != 32 || len(rix.ReviewRSalt) != 32 {
		t.Fatal("the requester did not keep the inputs of the review")
	}
}

// A review is signed once, only for a task that has ended with a checked receipt, within 30 days.
func TestSubmitReviewRefusals(t *testing.T) {
	_, req, _, id := reviewableTask(t)
	code := func(err error) string {
		var ref *a2ashape.Refusal
		if errors.As(err, &ref) {
			return ref.Code()
		}
		return ""
	}
	if _, err := req.SubmitReview(id, 6, ""); err == nil {
		t.Error("a rating of 6 was accepted")
	}
	if _, err := req.SubmitReview(id, 3, string(make([]rune, 281))); err == nil {
		t.Error("a comment of 281 characters was accepted")
	}
	if _, err := req.SubmitReview(id, 5, "good"); err != nil {
		t.Fatal(err)
	}
	if _, err := req.SubmitReview(id, 5, "again"); code(err) != "review_exists" {
		t.Errorf("a second review: %v, want review_exists", err)
	}
	// 31 days after the task ended it can no longer be started.
	_, req2, _, id2 := reviewableTask(t)
	req2.setClock(func() uint64 { return uint64(time.Now().Add(31 * 24 * time.Hour).UnixMilli()) })
	if _, err := req2.SubmitReview(id2, 5, "late"); code(err) != "review_window_closed" {
		t.Errorf("a review 31 days later: %v, want review_window_closed", err)
	}
}

// The upload is a queue: a review waits for its time, is taken once, and is given up when the hub
// refuses it in a way no retry changes or when seven days have passed (PAY-V9).
func TestTheReviewOutbox(t *testing.T) {
	url, req, _, id := reviewableTask(t)
	ctx := context.Background()
	fh := fakeHubAt(t, url)
	if _, err := req.SubmitReview(id, 4, "fine"); err != nil {
		t.Fatal(err)
	}
	rows, _ := req.ix.PendingReviews()
	if len(rows) != 1 {
		t.Fatalf("outbox %+v", rows)
	}
	// A hub that fails: the review stays, one attempt later.
	fh.mu.Lock()
	fh.reviewRefuse = "internal"
	fh.mu.Unlock()
	if err := req.ix.RetryReview(id, 0); err != nil {
		t.Fatal(err)
	}
	if n := req.uploadDueReviews(ctx); n != 0 {
		t.Fatalf("%d uploaded against a failing hub", n)
	}
	rows, _ = req.ix.PendingReviews()
	if len(rows) != 1 || rows[0].Attempts != 2 || rows[0].DueAt <= time.Now().UnixMilli() {
		t.Fatalf("after a failure: %+v, want one row, 2 attempts, due later", rows)
	}
	// A refusal that retrying cannot change: given up, and said.
	fh.mu.Lock()
	fh.reviewRefuse = "review_window_closed"
	fh.mu.Unlock()
	if err := req.ix.RetryReview(id, 0); err != nil {
		t.Fatal(err)
	}
	req.uploadDueReviews(ctx)
	if rows, _ := req.ix.PendingReviews(); len(rows) != 0 {
		t.Fatalf("a review the hub refused for good is still queued: %+v", rows)
	}
	if st := req.reviewsStatus(); st == nil || st["given_up"].(int64) != 1 {
		t.Fatalf("status %v, want one given up", st)
	}
}

func TestAReviewNotUploadedInSevenDaysIsGivenUp(t *testing.T) {
	url, req, _, id := reviewableTask(t)
	fh := fakeHubAt(t, url)
	if _, err := req.SubmitReview(id, 4, "fine"); err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	fh.reviewRefuse = "internal"
	fh.mu.Unlock()
	req.setClock(func() uint64 { return uint64(time.Now().Add(reviewGiveUpAfter + time.Hour).UnixMilli()) })
	if err := req.ix.RetryReview(id, 0); err != nil {
		t.Fatal(err)
	}
	req.uploadDueReviews(context.Background())
	if rows, _ := req.ix.PendingReviews(); len(rows) != 0 {
		t.Fatalf("still queued after seven days: %+v", rows)
	}
}

// A hub that already holds the review counts as done; the inputs are spent either way.
func TestAReviewTheHubAlreadyHoldsIsDone(t *testing.T) {
	url, req, _, id := reviewableTask(t)
	fh := fakeHubAt(t, url)
	if _, err := req.SubmitReview(id, 4, "fine"); err != nil {
		t.Fatal(err)
	}
	fh.mu.Lock()
	fh.reviewRefuse = "review_exists"
	fh.mu.Unlock()
	if err := req.ix.RetryReview(id, 0); err != nil {
		t.Fatal(err)
	}
	if n := req.uploadDueReviews(context.Background()); n != 1 {
		t.Fatalf("uploaded %d, want the review counted done", n)
	}
	if ix := mustIX(t, req, id); len(ix.ReviewSeed) != 0 || len(ix.ReviewRSalt) != 0 {
		t.Fatal("the review inputs outlived a review the hub holds")
	}
}

// An account on probation is not asked to upload before it ends, and is told so.
func TestAReviewWaitsForProbation(t *testing.T) {
	url, req, _, id := reviewableTask(t)
	fh := fakeHubAt(t, url)
	until := uint64(time.Now().Add(48 * time.Hour).UnixMilli())
	fh.mu.Lock()
	fh.acctExtra = map[string]map[string]any{req.AID(): {"probation_until": until}}
	fh.mu.Unlock()
	res, err := req.SubmitReview(id, 5, "good")
	if err != nil {
		t.Fatal(err)
	}
	if res.DueAt < int64(until) || res.ProbationUntil != int64(until) {
		t.Fatalf("due %d, probation %d, want both at or after %d", res.DueAt, res.ProbationUntil, until)
	}
}

// The one-time inputs of a task that ended more than 30 days ago are spent.
func TestReviewInputsAreSpentAfterThirtyDays(t *testing.T) {
	_, req, _, id := reviewableTask(t)
	if n, err := req.ix.PurgeReviewSecrets(time.Now().Add(-time.Hour).UnixMilli()); err != nil || n != 0 {
		t.Fatalf("spent %d inputs of a task that ended just now (%v)", n, err)
	}
	if n, err := req.ix.PurgeReviewSecrets(time.Now().Add(time.Hour).UnixMilli()); err != nil || n != 1 {
		t.Fatalf("spent %d inputs, want 1 (%v)", n, err)
	}
	if ix := mustIX(t, req, id); len(ix.ReviewSeed) != 0 || len(ix.ReviewRSalt) != 0 {
		t.Fatal("the inputs are still there")
	}
}

// ledgerHasIX reports whether d's evidence chain holds an event of kind whose payload names task id
// under key.
func ledgerHasIX(t *testing.T, d *Daemon, kind, key, id string) bool {
	t.Helper()
	b, err := os.ReadFile(d.layout.EvidenceLedgerPath())
	if err != nil {
		return false
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		rec, err := decodeRecord(line)
		if err != nil {
			t.Fatal(err)
		}
		if rec.EventType == kind && plainMap(rec.Payload)[key] == id {
			return true
		}
	}
	return false
}
