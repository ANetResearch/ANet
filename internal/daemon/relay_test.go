package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/internal/transcript"
)

// newTestDaemon builds a daemon rooted at a temp dir wired to hubURL. It stops the background relay loop
// so the test can drive pollOnce deterministically.
//
// accept=true makes the daemon accept delegations from every other daemon this test creates, before or
// after it: their AIDs are written to its peers.allow (the inbound policy stays closed, as on a fresh
// install; A2A-DESIGN §5). accept=false leaves the allow list empty, so the daemon refuses every
// delegation. A test that needs to accept an identity that is not a daemon (a stranger) uses
// allowPeers.
func newTestDaemon(t *testing.T, hubURL string, accept bool) *Daemon {
	t.Helper()
	root := t.TempDir()
	cfg := map[string]any{"control_addr": "127.0.0.1:0", "hub_url": hubURL}
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
	joinTestGroup(t, d, accept)
	return d
}

// testGroups holds, per test, the daemons newTestDaemon created and which of
// them accept the others.
var testGroups sync.Map // *testing.T -> *testGroup

type testGroup struct {
	mu        sync.Mutex
	all       []*Daemon
	accepting []*Daemon
}

// joinTestGroup adds d to its test's group and keeps the allow lists of the
// accepting daemons complete.
func joinTestGroup(t *testing.T, d *Daemon, accept bool) {
	t.Helper()
	v, _ := testGroups.LoadOrStore(t, &testGroup{})
	g := v.(*testGroup)
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.accepting {
		allowPeers(t, a, d.AID())
	}
	if accept {
		for _, o := range g.all {
			allowPeers(t, d, o.AID())
		}
		g.accepting = append(g.accepting, d)
	}
	g.all = append(g.all, d)
	t.Cleanup(func() { testGroups.Delete(t) })
}

// allowPeers appends AIDs to d's peers.allow, the way scripts and operators
// do it without the CLI (the CLI's `anet peers allow` requires a TTY).
func allowPeers(t *testing.T, d *Daemon, aids ...string) {
	t.Helper()
	appendPeerFile(t, d, d.config().inbound().AllowFile, aids...)
}

// trustPeers appends AIDs to d's peers.trust.
func trustPeers(t *testing.T, d *Daemon, aids ...string) {
	t.Helper()
	appendPeerFile(t, d, d.config().inbound().TrustFile, aids...)
}

// denyPeers appends AIDs to d's peers.deny.
func denyPeers(t *testing.T, d *Daemon, aids ...string) {
	t.Helper()
	appendPeerFile(t, d, d.config().inbound().DenyFile, aids...)
}

func appendPeerFile(t *testing.T, d *Daemon, name string, aids ...string) {
	t.Helper()
	f, err := os.OpenFile(d.peerFile(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, a := range aids {
		if _, err := f.WriteString(a + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRelayDelegationRoundTrip exercises the whole multi-turn loop through the Hub relay: register →
// delegate → provider poll → chat both ways → the requester asks to end → the provider daemon completes
// and signs the receipt over the v2 transcript → requester poll → review → the Hub stores the review
// without any task content.
func TestRelayDelegationRoundTrip(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)

	// Both must be registered so the relay knows their mailboxes + KELs (for poll auth + review verify).
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, ""); err != nil {
		t.Fatalf("register requester: %v", err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Bakery Bot", []string{"haiku"}, ""); err != nil {
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

	// 4. the requester asks to end; the provider daemon completes on its own (A2A-DESIGN §4.2): it
	// signs the receipt over the transcript and relays it back, without the provider's agent.
	if err := req.RequestEnd(ctx, id); err != nil {
		t.Fatalf("request end: %v", err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatalf("provider poll end-request: %v", err)
	}
	if pix, _ := prov.ix.Get(id); pix.State != interactions.StateCompleted || len(pix.Receipt) == 0 {
		t.Fatalf("provider did not complete on the end request: state %s receipt %d bytes", pix.State, len(pix.Receipt))
	}

	// 5. requester pulls the receipt (interaction becomes completed).
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
	if results[0].State != string(interactions.StateCompleted) || results[0].ReceiptVerified != string(interactions.VerificationVerified) {
		t.Fatalf("result state %s, receipt %q", results[0].State, results[0].ReceiptVerified)
	}
	// The deliverable is a v2 transcript carrying the task nonce (A2A-DESIGN §2 X4).
	tr, err := transcript.Parse([]byte(results[0].Result))
	if err != nil || tr.Version != 2 {
		t.Fatalf("deliverable is not a v2 transcript: %v %q", err, results[0].Result)
	}
	rix, _ := req.ix.Get(id)
	if tr.Nonce == "" || tr.Nonce != rix.TaskNonce {
		t.Fatalf("transcript nonce %q, task nonce %q", tr.Nonce, rix.TaskNonce)
	}

	// 6. requester reviews + uploads to the Hub.
	if _, err := req.SubmitReview(id, 5, "fast and delightful"); err != nil {
		t.Fatalf("review: %v", err)
	}
	if err := req.UploadReview(ctx, srv.URL, id); err != nil {
		t.Fatalf("upload review: %v", err)
	}

	// 7. the Hub shows the rating; it received no task content and says the content binding is
	// unverified (A2A-DESIGN §9).
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
	if len(got.Reviews) != 1 || got.Reviews[0].ContentBinding != hubapi.ContentBindingUnverified {
		t.Fatalf("stored review = %+v, want one with content_binding UNVERIFIED", got.Reviews)
	}
}

// The review upload body carries the receipt and the review and nothing else: no request TaskDoc, no
// deliverable (A2A-DESIGN §9, row 评价).
func TestTheReviewUploadCarriesNoContent(t *testing.T) {
	var body map[string]any
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	defer hub.Close()
	d := newTestDaemon(t, "", false)
	const id = "ix_review_body"
	if err := d.ix.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: "did:anet:bprovider",
		Goal: "a secret goal", RequestDoc: []byte("SECRET-REQUEST")}); err != nil {
		t.Fatal(err)
	}
	rc := &evidence.Receipt{InteractionID: id, RequesterAID: d.AID(), ProviderAID: "did:anet:bprovider",
		RequestCID: "req", ResultCID: "res", CompletedAt: 1}
	if err := rc.Sign(d.self); err != nil {
		t.Fatal(err)
	}
	rcb, _ := rc.Marshal()
	if err := d.ix.SetResult(id, []byte("SECRET-DELIVERABLE"), "res", rcb, interactions.VerificationVerified); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SubmitReview(id, 4, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := d.UploadReview(context.Background(), hub.URL, id); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "receipt,review" {
		t.Fatalf("review upload fields = %v, want receipt and review only", keys)
	}
	raw, _ := json.Marshal(body)
	for _, secret := range []string{"SECRET", base64.StdEncoding.EncodeToString([]byte("SECRET-REQUEST"))} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("review upload carries task content %q", secret)
		}
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
		if err := req.ix.SetResult(id, []byte("result"), resultCID, receiptBytes, interactions.VerificationVerified); err != nil {
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

// TestRelayDelegationRefusedWhenNotAccepting verifies that a provider with an empty allow list under the
// default closed policy refuses a delegation: it never enters its inbox, and the requester is told
// status{rejected, anet.reason=not_accepting} (A2A-DESIGN §5.2 row 6).
func TestRelayDelegationRefusedWhenNotAccepting(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()

	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, false) // NOT accepting

	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Closed Bot", nil, ""); err != nil {
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
	waitUntil(t, "the refusal notice reaches the requester", func() bool {
		_ = req.pollOnce(ctx)
		list, _ := req.ix.List(interactions.RoleOutbound, interactions.StateRejected, 0, 0)
		return len(list) == 1
	})
	list, _ := req.ix.List(interactions.RoleOutbound, interactions.StateRejected, 0, 0)
	msgs, _ := req.ix.Messages(list[0].ID)
	last := msgs[len(msgs)-1]
	if last.Kind != interactions.MsgStatus || !strings.Contains(last.Metadata, `"anet.reason":"not_accepting"`) {
		t.Fatalf("refusal notice = %+v", last)
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
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, ""); err != nil {
		t.Fatalf("register requester: %v", err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Coder", []string{"coding"}, ""); err != nil {
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
	zipPath := ""
	for _, f := range files {
		if f.Name == "build.zip" {
			zipPath = f.Path
		}
	}
	zipBytes, err := os.ReadFile(zipPath)
	if err != nil || string(zipBytes) != string(provBytes) {
		t.Fatalf("pulled build.zip mismatch: err=%v", err)
	}

	// The provider completes; the receipt-bound transcript must reference both attachment CIDs (not
	// their bytes).
	if err := prov.RequestEnd(ctx, id); err != nil {
		t.Fatalf("provider end: %v", err)
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

// Envelopes held back in the mailbox do not stand in front of newer mail
// (A2A-DESIGN §3.6 step 9, §3.7; decision Q1). A stranger fills the
// provider's mailbox with more than a page of signed messages for
// interactions it does not hold — each one class T for the unknown-ix
// window, so none is acknowledged — and a real delegation queued behind
// them is taken on the next round. Without the relay cursor every poll is
// handed the same first page and the delegation waits out the window.
func TestHeldBackEnvelopesDoNotBlockNewerMail(t *testing.T) {
	srv, req, prov := registeredPair(t)
	// The test drives the rounds: no background poll may move the cursor.
	// Taking pollMu once waits out a round already under way.
	prov.stopRelayLoop()
	prov.pollMu.Lock()
	prov.pollMu.Unlock()
	ctx := context.Background()
	var clock atomic.Uint64
	clock.Store(uint64(time.Now().UnixMilli()))
	prov.clock = clock.Load

	const flood = relayPollLimit + 50
	stranger := newStranger(t)
	for i := 0; i < flood; i++ {
		injectEnvelope(t, srv, prov.AID(), craft(t, stranger, prov, seal.TypeMessage,
			fmt.Sprintf("ix_not_held_%03d", i), chatBody(t, "noise", ""), nil))
	}
	id, err := req.Delegate(ctx, prov.AID(), "queued behind the flood", nil)
	if err != nil {
		t.Fatal(err)
	}
	queued := func() int { return len(queuedFor(t, srv, prov.AID())) }
	taken := func() bool {
		_, err := prov.ix.Get(id)
		return err == nil
	}

	// Round 1: the first page, every envelope held back.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := counter(prov, transientUnknownIX); n != relayPollLimit {
		t.Fatalf("round 1 held back %d envelopes, want the first page of %d", n, relayPollLimit)
	}
	if taken() {
		t.Fatal("setup: the delegation was on the first page")
	}
	if n := queued(); n != flood+1 {
		t.Fatalf("round 1 acknowledged %d envelopes; a class T envelope must stay", flood+1-n)
	}

	// Round 2: read on after that page.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !taken() {
		t.Fatal("the delegation queued behind a page of held-back envelopes was not taken on the next round")
	}
	if n := queued(); n != flood {
		t.Fatalf("%d envelopes left after round 2, want the %d held back", n, flood)
	}

	// Past the window the held envelopes turn permanent (TaskNotFound) and
	// are acknowledged on the way back through the mailbox.
	clock.Add(uint64((unknownIXWait + time.Minute).Milliseconds()))
	for round := 0; round < 4 && queued() > 0; round++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := queued(); n != 0 {
		t.Fatalf("%d envelopes still queued after the window; they were not retried from the head", n)
	}
	if n := counter(prov, dropUnknownIX); n != flood {
		t.Fatalf("%d envelopes dropped as unknown-ix, want %d", n, flood)
	}
}

// An envelope held back at the tail of the mailbox is still retried, and
// holding it costs the hub no extra requests: every round is one poll
// (relayCursor). A round that finds nothing after the cursor sends the next
// round back to the head rather than polling the head itself, so a
// stranger who keeps one message held cannot double how often this node
// calls its hub.
func TestAHeldEnvelopeIsRetriedAtOnePollPerRound(t *testing.T) {
	srv, _, prov := registeredPair(t)
	prov.stopRelayLoop()
	prov.pollMu.Lock()
	prov.pollMu.Unlock()
	ctx := context.Background()
	var clock atomic.Uint64
	clock.Store(uint64(time.Now().UnixMilli()))
	prov.clock = clock.Load

	injectEnvelope(t, srv, prov.AID(), craft(t, newStranger(t), prov, seal.TypeMessage,
		"ix_not_held_tail", chatBody(t, "noise", ""), nil))
	polls0 := relayPollsFor(srv.URL, prov.AID())
	tries0 := counter(prov, transientUnknownIX)
	const rounds = 6
	for i := 0; i < rounds; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := relayPollsFor(srv.URL, prov.AID()) - polls0; n != rounds {
		t.Fatalf("%d rounds made %d poll requests, want one each", rounds, n)
	}
	if n := counter(prov, transientUnknownIX) - tries0; n < rounds/2 {
		t.Fatalf("the held envelope was tried %d times in %d rounds, want at least every other round", n, rounds)
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("%d envelopes queued, want the held one", n)
	}

	// Past the window it turns permanent and is acknowledged.
	clock.Add(uint64((unknownIXWait + time.Minute).Milliseconds()))
	for i := 0; i < 2; i++ {
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(queuedFor(t, srv, prov.AID())); n != 0 {
		t.Fatalf("%d envelopes still queued two rounds after the window", n)
	}
}
