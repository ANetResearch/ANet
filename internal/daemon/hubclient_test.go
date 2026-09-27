package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// A hub that states a wire version below 2 stores and forwards plaintext.
// The daemon refuses to register with it or to relay through it; there is
// no fallback (A2A-DESIGN §3.7, §18).
func TestAHubBelowWire2IsRefused(t *testing.T) {
	srv := newFakeHub(t)
	fake := fakeHubAt(t, srv.URL)
	fake.mu.Lock()
	fake.wire = 1
	fake.mu.Unlock()
	d := newTestDaemon(t, srv.URL, true)
	err := d.RegisterWithHub(context.Background(), srv.URL, "n", nil, "")
	if !errors.Is(err, errHubWire) {
		t.Fatalf("registration with a wire-1 hub: %v, want errHubWire", err)
	}
	if !strings.Contains(err.Error(), "wire 1") {
		t.Errorf("the error does not say what the hub speaks: %v", err)
	}
	if _, err := d.relayPoll(context.Background(), 0); !errors.Is(err, errHubWire) {
		t.Fatalf("poll against a wire-1 hub: %v", err)
	}
	if n := relayCountFor(srv.URL); n != 0 {
		t.Fatalf("%d relay sends reached a wire-1 hub", n)
	}
}

// A hub that answers 426 refuses this daemon's version; the call fails with
// the same error, naming the refusal.
func TestA426IsRefused(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = w.Write([]byte(`{"error":"requires anet >= 0.3.0"}`))
	}))
	defer up.Close()
	d := newTestDaemon(t, "", true)
	if err := d.hubGet(context.Background(), up.URL, "/agents", nil, nil); !errors.Is(err, errHubWire) {
		t.Fatalf("426 answered with %v, want errHubWire", err)
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
		r.Header.Set(hubapi.WireVersionHeader, "2")
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

// The relay's refusal paths reach the caller as errors with their status,
// and nothing is queued (fake completeness, A2A-DESIGN §17).
func TestTheRelayRefusalPathsAreSurfaced(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	fake := fakeHubAt(t, srv.URL)
	env := sealFrom(t, req, prov, seal.TypeMessage, "ix", chatBody(t, "x", ""))
	send := func(to string, envelope []byte) error {
		return req.hubSigned(ctx, srv.URL, http.MethodPost, "/relay/send", relayauth.ActionSend,
			hubapi.RelaySendRequest{ToAID: to, Envelope: base64.StdEncoding.EncodeToString(envelope)}, nil)
	}
	set := func(f func()) {
		fake.mu.Lock()
		f()
		fake.mu.Unlock()
	}

	if err := send(prov.AID(), []byte("not an envelope")); hubStatus(err) != http.StatusBadRequest {
		t.Errorf("garbage envelope: %v, want 400", err)
	}
	other := newTestDaemon(t, "", true)
	if err := send(prov.AID(), sealFrom(t, req, other, seal.TypeMessage, "ix", chatBody(t, "x", ""))); hubStatus(err) != http.StatusBadRequest {
		t.Errorf("envelope to another AID than to_aid: %v, want 400", err)
	}
	if err := send(other.AID(), sealFrom(t, req, other, seal.TypeMessage, "ix", chatBody(t, "x", ""))); hubStatus(err) != http.StatusNotFound {
		t.Errorf("unregistered recipient: %v, want 404", err)
	}
	set(func() { fake.maxEnvelope = 100 })
	if err := send(prov.AID(), env); hubStatus(err) != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized envelope: %v, want 413", err)
	}
	set(func() { fake.maxEnvelope = 0; fake.mailboxCap = 1 })
	if err := send(prov.AID(), env); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if err := send(prov.AID(), env); hubStatus(err) != http.StatusInsufficientStorage {
		t.Errorf("full mailbox: %v, want 507", err)
	}
	set(func() { fake.mailboxCap = 0; fake.senderLimit = fake.sendsBy[req.AID()] })
	err := send(prov.AID(), env)
	if hubStatus(err) != http.StatusTooManyRequests || !strings.Contains(err.Error(), "retry after") {
		t.Errorf("sender over budget: %v, want 429 with Retry-After", err)
	}
	set(func() { fake.senderLimit = 0 })
	// A daemon whose hub identity is wrong signs for another hub: 401.
	req.hubIDMu.Lock()
	req.hubIDs[strings.TrimRight(srv.URL, "/")] = hubIdent{aid: prov.AID()}
	req.hubIDMu.Unlock()
	if err := send(prov.AID(), env); hubStatus(err) != http.StatusUnauthorized {
		t.Errorf("signature for another hub: %v, want 401", err)
	}
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
	if err := prov.RegisterWithHub(context.Background(), srv.URL, "Prov", nil, ""); err != nil {
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
