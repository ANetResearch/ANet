package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/hubapi"
)

// A hub that states a wire version below this daemon's speaks another
// contract. The daemon refuses to register with it or to relay through it;
// there is no fallback (core CORE-V1).
func TestAHubBelowWire3IsRefused(t *testing.T) {
	srv := newFakeHub(t)
	fake := fakeHubAt(t, srv.URL)
	fake.mu.Lock()
	fake.wire = 2
	fake.mu.Unlock()
	d := newTestDaemon(t, srv.URL, true)
	// The join ends refused with wire_too_new: the hub is older than this
	// node, and retrying will not change that (LEC AD-2, ux UX-R6).
	err := d.RegisterWithHub(context.Background(), srv.URL, "n", nil)
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Code() != "wire_too_new" {
		t.Fatalf("registration with a wire-2 hub: %v, want wire_too_new", err)
	}
	if r := d.joinSnapshot(); r == nil || r.State != JoinRefused || r.Reason != "wire_too_new" {
		t.Fatalf("join state after a wire-2 hub: %+v", r)
	}
	if fakeHubAt(t, srv.URL).registerCalls != 0 {
		t.Error("a registration was sent to a wire-2 hub")
	}
	if _, err := d.relayPoll(context.Background(), 0); !errors.Is(err, errHubWire) {
		t.Fatalf("poll against a wire-2 hub: %v", err)
	}
	if n := relayCountFor(srv.URL); n != 0 {
		t.Fatalf("%d relay sends reached a wire-2 hub", n)
	}
}

// A hub that answers 426 refuses this daemon's version; the call fails with
// the same error, naming the refusal and the release the hub needs.
func TestA426IsRefused(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(hubapi.WireVersionHeader, "4")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = w.Write([]byte(`{"reason":"wire_too_old","error":"requires anet 0.4.0","required_wire":4,"required_anet":"0.4.0"}`))
	}))
	defer up.Close()
	d := newTestDaemon(t, "", true)
	err := d.hubGet(context.Background(), up.URL, "/agents", nil, nil)
	var we *hubWireError
	if !errors.Is(err, errHubWire) || !errors.As(err, &we) || we.hubOlder || we.version != "0.4.0" {
		t.Fatalf("426 answered with %v, want errHubWire naming 0.4.0", err)
	}
}

// Every signed call carries relayauth v2 headers that bind the hub, the
// method, the target and the body; the fake verifies them the way the hub
// does, and refuses a replay, a tampered body and a signature for another
// hub.
func TestRelayAuthV2IsCheckedByTheFake(t *testing.T) {
	srv, req, _ := registeredPair(t)
	fake := fakeHubAt(t, srv.URL)
	hubAID := fake.self.AID()
	body, _ := json.Marshal(hubapi.RelayPollRequest{Limit: 1})
	signed := func(aidForHub string, b []byte) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, srv.URL+"/relay/poll", bytes.NewReader(b))
		ts := uint64(time.Now().UnixMilli())
		sig, seq := req.self.Sign(relayauth.PreimageV2(relayauth.ActionPoll, req.AID(), aidForHub, ts,
			http.MethodPost, "/relay/poll", body))
		r.Header.Set(hubapi.WireVersionHeader, "3")
		r.Header.Set(hubapi.HeaderAID, req.AID())
		r.Header.Set(hubapi.HeaderTS, strconv.FormatUint(ts, 10))
		r.Header.Set(hubapi.HeaderSeq, strconv.FormatUint(seq, 10))
		r.Header.Set(hubapi.HeaderSig, relayauth.EncodeSig(sig))
		return r
	}
	do := func(r *http.Request) int {
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	r := signed(hubAID, body)
	if code := do(r); code != http.StatusOK {
		t.Fatalf("a correctly signed poll got %d", code)
	}
	replay2, _ := http.NewRequest(http.MethodPost, srv.URL+"/relay/poll", bytes.NewReader(body))
	replay2.Header = r.Header.Clone()
	if code := do(replay2); code != http.StatusUnauthorized {
		t.Fatalf("a replayed signature got %d, want 401", code)
	}
	if code := do(signed(hubAID, []byte(`{"limit":99}`))); code != http.StatusUnauthorized {
		t.Fatalf("a tampered body got %d, want 401", code)
	}
	if code := do(signed(req.AID(), body)); code != http.StatusUnauthorized {
		t.Fatalf("a signature for another hub got %d, want 401", code)
	}
	noWire, _ := http.NewRequest(http.MethodPost, srv.URL+"/relay/poll", bytes.NewReader(body))
	if code := do(noWire); code != http.StatusUpgradeRequired {
		t.Fatalf("a request without X-ANet-Wire got %d, want 426", code)
	}
}

// The relay's refusal paths reach the caller as errors with their status
// and reason code, and nothing is queued (fake completeness, A2A-DESIGN
// §17).
func TestTheRelayRefusalPathsAreSurfaced(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	fake := fakeHubAt(t, srv.URL)
	kind0 := AccessChoice{Send: true, Kind: seal.AccessKindSession}
	send := func(env []byte) error {
		_, _, err := req.relaySendV3(ctx, env, kind0)
		return err
	}
	fresh := func(to *Daemon) []byte {
		return sealFrom(t, req, to, seal.TypeMessage, "ix", chatBody(t, "x", ""))
	}
	set := func(f func()) {
		fake.mu.Lock()
		f()
		fake.mu.Unlock()
	}
	refused := func(what string, err error, code int, reason string) {
		t.Helper()
		if hubStatus(err) != code || hubReason(err) != reason {
			t.Errorf("%s: %v, want %d %s", what, err, code, reason)
		}
	}

	// Bytes that are not an envelope never leave this node.
	if err := send([]byte("not an envelope")); err == nil || hubStatus(err) != 0 {
		t.Errorf("garbage: %v, want a local refusal", err)
	}
	other := newTestDaemon(t, "", true)
	refused("unregistered recipient", send(fresh(other)), http.StatusNotFound, "recipient_unknown")
	set(func() { fake.maxEnvelope = 100 })
	refused("oversized envelope", send(fresh(prov)), http.StatusRequestEntityTooLarge, "too_large")
	set(func() { fake.maxEnvelope = 0; fake.mailboxCap = 1 })
	first := fresh(prov)
	if err := send(first); err != nil {
		t.Fatalf("first send: %v", err)
	}
	// The same envelope again is the one already queued, not a second.
	if res, _, err := req.relaySendV3(ctx, first, kind0); err != nil || res.Status != hubapi.RelayDuplicate {
		t.Fatalf("the same envelope again: %+v %v, want a duplicate", res, err)
	}
	refused("full mailbox", send(fresh(prov)), http.StatusInsufficientStorage, "mailbox_full")
	set(func() { fake.mailboxCap = 0; fake.senderLimit = fake.sendsBy[req.AID()] })
	err := send(fresh(prov))
	refused("sender over budget", err, http.StatusTooManyRequests, "rate_limited")
	if err == nil || !strings.Contains(err.Error(), "retry after") {
		t.Errorf("sender over budget: %v, want Retry-After", err)
	}
	set(func() { fake.senderLimit = 0 })
	if n := len(queuedFor(t, srv, prov.AID())); n != 1 {
		t.Fatalf("%d envelopes queued, want only the one accepted send", n)
	}
}

// The key set is published at registration and the hub answers an
// identical set at the same seq as unchanged — a restart that re-registers
// does not fail (C16).
func TestRegistrationPublishesTheKeySet(t *testing.T) {
	srv, _, prov := registeredPair(t)
	fake := fakeHubAt(t, srv.URL)
	fake.mu.Lock()
	stored := append([]byte(nil), fake.agents[prov.AID()].keyset...)
	status := fake.registerKeys[prov.AID()]
	fake.mu.Unlock()
	if !bytes.Equal(stored, prov.enc.SignedSet()) {
		t.Fatal("the hub does not hold the key set the provider registered")
	}
	if status != hubapi.KeysStatusOK && status != hubapi.KeysStatusUnchanged {
		t.Fatalf("the registration itself carried no accepted key set (keys_status %q)", status)
	}
	if got := prov.publishedKeySeq.Load(); got != prov.enc.Seq() {
		t.Fatalf("published seq = %d, want %d", got, prov.enc.Seq())
	}
	if err := prov.RegisterWithHub(context.Background(), srv.URL, "Prov", nil); err != nil {
		t.Fatalf("re-registration with the same key set: %v", err)
	}
	if err := prov.publishKeys(context.Background(), srv.URL); err != nil {
		t.Fatalf("re-publishing the same key set: %v", err)
	}
}

// Two identical signed requests in quick succession are two requests, not
// a replay. Ed25519 is deterministic, so they differ only if their
// timestamps do; the daemon keeps its signing time strictly increasing.
func TestIdenticalSignedRequestsAreNotReplays(t *testing.T) {
	srv, req, _ := registeredPair(t)
	for i := 0; i < 50; i++ {
		if err := req.hubSigned(context.Background(), srv.URL, http.MethodPost, "/relay/poll",
			relayauth.ActionPoll, hubapi.RelayPollRequest{Limit: 1}, nil); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
}
