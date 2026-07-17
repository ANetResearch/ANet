package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/protocol/evidence"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// newTestDaemon builds a daemon rooted at a temp dir wired to hubURL. It stops the background relay loop
// so the test can drive pollOnce deterministically.
func newTestDaemon(t *testing.T, hubURL string, accept bool) *Daemon {
	t.Helper()
	root := t.TempDir()
	cfg := map[string]any{"control_addr": "127.0.0.1:0", "hub_url": hubURL, "accept_delegations": accept}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := New(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	// Stop the background poll loop; the test drives pollOnce explicitly.
	d.mu.Lock()
	if d.relayStop != nil {
		d.relayStop()
		d.relayStop = nil
	}
	d.mu.Unlock()
	t.Cleanup(func() { d.Close() })
	return d
}

// TestRelayDelegationRoundTrip exercises the whole v0.1 multi-turn loop through the Hub relay: register →
// delegate → provider poll → chat both ways → end negotiation (propose + accept) → provider issues the
// receipt over the transcript → requester poll → review → Hub verify + display the verified transcript.
func TestRelayDelegationRoundTrip(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)

	// Both must be registered so the relay knows their mailboxes + KELs (for poll auth + review verify).
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatalf("register requester: %v", err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Bakery Bot", []string{"haiku"}, GuestDefaultMessages); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	// 1. requester delegates to the provider's AID.
	id, err := req.Delegate(ctx, prov.AID(), "write a haiku about agents", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}

	// 2. provider pulls its mailbox and stores the task.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatalf("provider poll: %v", err)
	}
	inbox, err := prov.Inbox(true)
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].InteractionID != id {
		t.Fatalf("provider inbox = %+v, want the delegated task %s", inbox, id)
	}
	if inbox[0].Requester != req.AID() {
		t.Fatalf("inbox requester = %s, want %s", inbox[0].Requester, req.AID())
	}

	// 3. provider's external agent replies with a message; requester pulls it and replies back.
	const deliverable = "agents in the dark / whisper across the network / a haiku returns"
	if err := prov.SendMessage(ctx, id, deliverable, nil); err != nil {
		t.Fatalf("provider message: %v", err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatalf("requester poll message: %v", err)
	}
	if err := req.SendMessage(ctx, id, "perfect, thank you", nil); err != nil {
		t.Fatalf("requester message: %v", err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatalf("provider poll reply: %v", err)
	}

	// 4. end negotiation: the requester proposes ending, the provider accepts → the provider issues the
	// signed receipt over the transcript and relays it back.
	if err := req.RequestEnd(ctx, id); err != nil {
		t.Fatalf("request end: %v", err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatalf("provider poll end-request: %v", err)
	}
	if err := prov.AcceptEnd(ctx, id); err != nil {
		t.Fatalf("accept end: %v", err)
	}

	// 5. requester pulls the receipt (interaction becomes done).
	results, err := req.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 1 || results[0].InteractionID != id {
		t.Fatalf("results = %+v, want the ended task %s", results, id)
	}
	if !strings.Contains(results[0].Result, deliverable) {
		t.Fatalf("transcript = %q, want it to contain the provider's message", results[0].Result)
	}
	if results[0].RequestCID == "" || results[0].ResultCID == "" ||
		results[0].ReceiptCID == "" || results[0].Receipt == "" {
		t.Fatalf("result omitted verifiable evidence: %+v", results[0])
	}

	// 6. requester reviews + uploads to the Hub.
	if _, err := req.SubmitReview(id, 5, "fast and delightful"); err != nil {
		t.Fatalf("review: %v", err)
	}
	if err := req.UploadReview(ctx, srv.URL, id); err != nil {
		t.Fatalf("upload review: %v", err)
	}

	// 7. the Hub shows the verified rating + transcript on the provider.
	var got struct {
		Agent   hubapi.AgentView    `json:"agent"`
		Reviews []hubapi.ReviewView `json:"reviews"`
	}
	resp, err := srv.Client().Get(srv.URL + "/agents/" + prov.AID())
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.Agent.ReviewCount != 1 || got.Agent.AvgRating != 5 {
		t.Fatalf("aggregate = %d/%v, want 1/5", got.Agent.ReviewCount, got.Agent.AvgRating)
	}
	if len(got.Reviews) != 1 || !strings.Contains(got.Reviews[0].Deliverable, deliverable) {
		t.Fatalf("stored review = %+v, want transcript containing %q", got.Reviews, deliverable)
	}
}

func TestResultsPaginatesBeyondStoreDefaultLimit(t *testing.T) {
	srv := newFakeHub(t)
	req := newTestDaemon(t, srv.URL, false)
	const count = 1005
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("ix_page_%04d", index)
		requestCID := fmt.Sprintf("request_%04d", index)
		resultCID := fmt.Sprintf("result_%04d", index)
		if err := req.ix.Put(
			id, interactions.RoleOutbound, req.AID(), "goal", requestCID, []byte("request"),
		); err != nil {
			t.Fatal(err)
		}
		receipt := &evidence.Receipt{
			InteractionID: id, RequesterAID: req.AID(), ProviderAID: req.AID(),
			RequestCID: requestCID, ResultCID: resultCID, CompletedAt: uint64(index + 1),
		}
		if err := receipt.Sign(req.self); err != nil {
			t.Fatal(err)
		}
		receiptBytes, err := receipt.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := req.ix.SetResult(id, []byte("result"), resultCID, receiptBytes); err != nil {
			t.Fatal(err)
		}
	}
	results, err := req.Results(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != count {
		t.Fatalf("results = %d, want %d", len(results), count)
	}
}

// TestRelayDelegationRefusedWhenNotAccepting verifies a provider that has not opted in drops delegated
// tasks (they never enter its inbox).
func TestRelayDelegationRefusedWhenNotAccepting(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, false) // NOT accepting

	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Closed Bot", nil, GuestDefaultMessages); err != nil {
		t.Fatal(err)
	}
	if _, err := req.Delegate(ctx, prov.AID(), "do something", nil); err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	inbox, err := prov.Inbox(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 0 {
		t.Fatalf("non-accepting provider stored %d tasks, want 0", len(inbox))
	}
}

// TestRelayAttachmentRoundTrip exercises binary attachments through the whole relay loop: the requester
// delegates WITH a file, the provider receives + stores it (CID-verified), replies with its OWN file, the
// requester pulls it to disk (bytes intact), and the receipt-bound transcript records both attachments'
// content CIDs (not their bytes).
func TestRelayAttachmentRoundTrip(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages); err != nil {
		t.Fatalf("register requester: %v", err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Coder", []string{"coding"}, GuestDefaultMessages); err != nil {
		t.Fatalf("register provider: %v", err)
	}

	// A tiny PNG-ish blob (bytes are arbitrary; we only assert byte-for-byte fidelity + CID binding).
	reqFile := filepath.Join(t.TempDir(), "spec.png")
	reqBytes := []byte("\x89PNG\r\n\x1a\n-attachment-payload-from-requester")
	if err := os.WriteFile(reqFile, reqBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := req.Delegate(ctx, prov.AID(), "here is the spec, build it", []string{reqFile})
	if err != nil {
		t.Fatalf("delegate with attachment: %v", err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatalf("provider poll: %v", err)
	}

	// Provider sees the requester's attachment, byte-identical.
	provAtts, err := prov.ix.Attachments(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(provAtts) != 1 || provAtts[0].Name != "spec.png" || provAtts[0].Size != int64(len(reqBytes)) {
		t.Fatalf("provider attachments = %+v, want one spec.png of %d bytes", provAtts, len(reqBytes))
	}
	if provAtts[0].Mime != "image/png" {
		t.Fatalf("mime = %q, want image/png", provAtts[0].Mime)
	}
	if got, _, data, err := prov.AttachmentBytes(id, provAtts[0].CID); err != nil || string(data) != string(reqBytes) {
		t.Fatalf("provider stored bytes mismatch (name=%q err=%v)", got, err)
	}

	// Provider delivers a result file back.
	provFile := filepath.Join(t.TempDir(), "build.zip")
	provBytes := []byte("PK\x03\x04-fake-zip-of-the-delivered-project")
	if err := os.WriteFile(provFile, provBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prov.SendMessage(ctx, id, "done — see the zip", []string{provFile}); err != nil {
		t.Fatalf("provider message with attachment: %v", err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatalf("requester poll: %v", err)
	}

	// Requester pulls received attachments to disk; bytes must be intact.
	outDir := t.TempDir()
	files, err := req.Pull(id, outDir)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("pulled %d files, want 2", len(files))
	}
	zipBytes, err := os.ReadFile(filepath.Join(outDir, "build.zip"))
	if err != nil || string(zipBytes) != string(provBytes) {
		t.Fatalf("pulled build.zip mismatch: err=%v", err)
	}

	// End the task; the receipt-bound transcript must reference both attachment CIDs (not their bytes).
	if err := prov.RequestEnd(ctx, id); err != nil {
		t.Fatalf("provider end: %v", err)
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatalf("requester poll end: %v", err)
	}
	if err := req.AcceptEnd(ctx, id); err != nil {
		t.Fatalf("requester accept end: %v", err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatalf("provider poll accept: %v", err)
	}
	results, err := req.Results(ctx)
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	for _, want := range []string{provAtts[0].CID, "build.zip", "spec.png"} {
		if !strings.Contains(results[0].Result, want) {
			t.Fatalf("transcript %q missing %q", results[0].Result, want)
		}
	}
}
