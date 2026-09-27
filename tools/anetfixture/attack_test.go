package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// The joint run's attack sections assert that a daemon refuses what this
// fixture sends. A fixture that sent something other than what it claims —
// an envelope a hub would reject, a forgery that fails for an unrelated
// reason, a signature over the wrong bytes — would turn those sections into
// tests of the fixture. These pin that each command produces exactly the
// object its name promises.

const testHubAID = "did:anet:test-hub"

// fakeHub serves what the fixture reads from a hub (its identity and agents'
// keys) and verifies relay v2 on POST /relay/send the way a wire-2 hub does.
type fakeHub struct {
	*httptest.Server
	mu    sync.Mutex
	kels  map[string][]identity.SignedEvent
	keys  map[string][]byte
	sends []hubapi.RelaySendRequest
	errs  []string
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{kels: map[string][]identity.SignedEvent{}, keys: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hub/identity", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(hubapi.HubIdentity{AID: testHubAID, KEL: "AA=="})
	})
	mux.HandleFunc("GET /agents/{aid}/keys", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		kel, ks := h.kels[r.PathValue("aid")], h.keys[r.PathValue("aid")]
		h.mu.Unlock()
		if ks == nil {
			http.NotFound(w, r)
			return
		}
		kb, _ := identity.MarshalKEL(kel)
		_ = json.NewEncoder(w).Encode(hubapi.KeysResponse{AID: r.PathValue("aid"),
			KeySet: base64.StdEncoding.EncodeToString(ks), KEL: base64.StdEncoding.EncodeToString(kb)})
	})
	mux.HandleFunc("POST /relay/send", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := h.verify(r, relayauth.ActionSend, body); err != "" {
			h.mu.Lock()
			h.errs = append(h.errs, err)
			h.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err})
			return
		}
		var req hubapi.RelaySendRequest
		_ = json.Unmarshal(body, &req)
		h.mu.Lock()
		h.sends = append(h.sends, req)
		n := len(h.sends)
		h.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": n, "status": "queued"})
	})
	h.Server = httptest.NewServer(mux)
	t.Cleanup(h.Close)
	return h
}

// verify is the hub's relay v2 check (ANetHub auth2.go verifyV2), minus the
// replay cache.
func (h *fakeHub) verify(r *http.Request, action string, body []byte) string {
	if r.Header.Get(hubapi.WireVersionHeader) != "2" {
		return "no wire version"
	}
	aid := r.Header.Get(relayauth.HeaderAID)
	ts, err1 := strconv.ParseUint(r.Header.Get(relayauth.HeaderTS), 10, 64)
	seq, err2 := strconv.ParseUint(r.Header.Get(relayauth.HeaderSeq), 10, 64)
	sig, err3 := relayauth.DecodeSig(r.Header.Get(relayauth.HeaderSig))
	if err1 != nil || err2 != nil || err3 != nil {
		return "malformed headers"
	}
	h.mu.Lock()
	kel := h.kels[aid]
	h.mu.Unlock()
	pre := relayauth.PreimageV2(action, aid, testHubAID, ts, r.Method, r.URL.RequestURI(), body)
	if err := identity.VerifyObject(kel, aid, seq, ts, pre, sig); err != nil {
		return "signature: " + err.Error()
	}
	return ""
}

// register publishes c's KEL and a fresh key set, and returns the key pair
// so the test can open what is sealed to c.
func (h *fakeHub) register(t *testing.T, c *identity.Controller) *seal.KeyPair {
	t.Helper()
	now := uint64(time.Now().UnixMilli())
	kp, err := seal.GenerateKeyPair(seal.SuiteX25519, now-1000, now+seal.KeyLifetimeMS)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := seal.SignEncKeySet(&seal.EncKeySet{Type: seal.EncKeySetType, AID: c.AID(), Seq: now,
		Keys: []seal.EncKey{kp.Public}, IssuedAt: now}, c.Sign)
	if err != nil {
		t.Fatal(err)
	}
	b, err := signed.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.kels[c.AID()], h.keys[c.AID()] = c.KEL(), b
	h.mu.Unlock()
	return kp
}

// sealWith runs `anetfixture seal` and opens the result as the recipient.
func sealWith(t *testing.T, recipient string, kp *seal.KeyPair, args ...string) *seal.Opened {
	t.Helper()
	out := capture(t, func() error { return cmdSeal(args) })
	env, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("seal output is not base64: %v", err)
	}
	op, err := seal.Open(env, recipient, seal.StaticKeyRing{kp})
	if err != nil {
		t.Fatalf("the recipient cannot open what seal made: %v", err)
	}
	return op
}

// The three envelopes the joint run sends: a genuine one passes the
// receiver's steps 5-7; a forgery with the attacker's own KEL fails step 6
// (from-mismatch); a forgery with the claimed sender's KEL fails step 7
// (bad-sig). If the forgeries failed anywhere else, the joint run's
// counters would be measuring something other than SI-4.
func TestSealMakesTheEnvelopeItClaimsToMake(t *testing.T) {
	hub := newFakeHub(t)
	_, prov := withIdentity(t)
	reqDir, req := withIdentity(t)
	strDir, str := withIdentity(t)
	provKP := hub.register(t, prov)
	hub.register(t, req)
	hub.register(t, str)
	now := uint64(time.Now().UnixMilli())
	base := []string{"--hub", hub.URL, "--to", prov.AID(), "--type", "message", "--ix", "ix_1", "--text", "hello"}

	t.Run("genuine", func(t *testing.T) {
		op := sealWith(t, prov.AID(), provKP, append([]string{"--home", reqDir}, base...)...)
		in := &op.Inner
		if in.From != req.AID() || in.Type != seal.TypeMessage || in.IX != "ix_1" {
			t.Fatalf("inner = from %s type %s ix %s", in.From, in.Type, in.IX)
		}
		if err := seal.CheckTime(in, now); err != nil {
			t.Fatalf("time window: %v", err)
		}
		if err := seal.VerifyInnerSig(in, op.Preimage, op.KEL, now, time.Hour); err != nil {
			t.Fatalf("a genuine envelope does not verify: %v", err)
		}
		cm, err := delegation.UnmarshalChatMsg(in.Body)
		if err != nil || cm.Body != "hello" || cm.Kind != delegation.ChatText || cm.MsgID == "" {
			t.Fatalf("body = %+v, %v", cm, err)
		}
	})

	t.Run("claimed sender, own KEL", func(t *testing.T) {
		op := sealWith(t, prov.AID(), provKP,
			append([]string{"--home", strDir, "--as", req.AID(), "--kel", "self"}, base...)...)
		if op.Inner.From != req.AID() {
			t.Fatalf("inner from = %s, want the claimed %s", op.Inner.From, req.AID())
		}
		if got := replayAID(t, op.KEL); got != str.AID() {
			t.Fatalf("inner KEL replays to %s, want the attacker %s", got, str.AID())
		}
	})

	t.Run("claimed sender, claimed KEL", func(t *testing.T) {
		op := sealWith(t, prov.AID(), provKP,
			append([]string{"--home", strDir, "--as", req.AID(), "--kel", "claimed", "--no-msg-id"}, base...)...)
		if got := replayAID(t, op.KEL); got != req.AID() {
			t.Fatalf("inner KEL replays to %s, want the claimed %s", got, req.AID())
		}
		err := seal.VerifyInnerSig(&op.Inner, op.Preimage, op.KEL, now, time.Hour)
		if seal.ReasonOf(err) != seal.ReasonBadSig {
			t.Fatalf("forgery under the claimed KEL: %v, want %s", err, seal.ReasonBadSig)
		}
		if cm, _ := delegation.UnmarshalChatMsg(op.Inner.Body); cm == nil || cm.MsgID != "" {
			t.Fatal("--no-msg-id left a message id in the body")
		}
	})

	t.Run("delegate", func(t *testing.T) {
		op := sealWith(t, prov.AID(), provKP, "--home", reqDir, "--hub", hub.URL, "--to", prov.AID(),
			"--type", "delegate", "--text", "a goal")
		dr, err := delegation.UnmarshalDelegateReq(op.Inner.Body)
		if err != nil {
			t.Fatal(err)
		}
		if dr.InteractionID == "" || dr.InteractionID != op.Inner.IX {
			t.Fatalf("delegate names %q, envelope %q", dr.InteractionID, op.Inner.IX)
		}
		signer, _, _, err := delegation.VerifyDelegateReqWithKEL(dr, op.KEL, op.Inner.TS)
		if err != nil || signer != req.AID() {
			t.Fatalf("the TaskDoc does not verify as the requester's: %s, %v", signer, err)
		}
	})
}

// --metadata puts a JSON object into the sealed ChatMsg, which is how
// scripts/scenario.sh sends a payment message as the requester with terms
// its daemon would not sign. What the provider reads must be exactly that
// object, from a genuine sender: a payment refused because the envelope or
// the metadata was malformed would pass the scenario's negative cases for
// the wrong reason. A value that is not an object is refused here, and so
// is metadata on a delegate, which has nowhere to carry it.
func TestSealCarriesAMessagesMetadata(t *testing.T) {
	hub := newFakeHub(t)
	_, prov := withIdentity(t)
	reqDir, req := withIdentity(t)
	provKP := hub.register(t, prov)
	hub.register(t, req)
	meta := `{"x402.payment.status": "payment-submitted", "x402.payment.payload": {"x402Version": 2}}`
	op := sealWith(t, prov.AID(), provKP, "--home", reqDir, "--hub", hub.URL, "--to", prov.AID(),
		"--type", "message", "--ix", "ix_pay", "--metadata", meta)
	now := uint64(time.Now().UnixMilli())
	if err := seal.VerifyInnerSig(&op.Inner, op.Preimage, op.KEL, now, time.Hour); err != nil || op.Inner.From != req.AID() {
		t.Fatalf("not a genuine envelope from the requester: from %s, %v", op.Inner.From, err)
	}
	cm, err := delegation.UnmarshalChatMsg(op.Inner.Body)
	if err != nil || cm.Kind != delegation.ChatText || cm.Body != "" || cm.MsgID == "" {
		t.Fatalf("body = %+v, %v", cm, err)
	}
	var got map[string]any
	if err := json.Unmarshal(cm.Metadata, &got); err != nil {
		t.Fatalf("metadata %q: %v", cm.Metadata, err)
	}
	if got["x402.payment.status"] != "payment-submitted" || got["x402.payment.payload"] == nil {
		t.Fatalf("metadata = %v", got)
	}

	for _, bad := range [][]string{
		{"--type", "message", "--ix", "ix_pay", "--metadata", `["not", "an", "object"]`},
		{"--type", "message", "--ix", "ix_pay", "--metadata", `null`},
		{"--type", "delegate", "--metadata", `{"k": 1}`},
	} {
		args := append([]string{"--home", reqDir, "--hub", hub.URL, "--to", prov.AID()}, bad...)
		if err := cmdSeal(args); err == nil {
			t.Errorf("seal %v: accepted", bad)
		}
	}
}

// replayAID is the AID a KEL replays to — what the receiver's step 6
// compares with inner.from.
func replayAID(t *testing.T, kel []identity.SignedEvent) string {
	t.Helper()
	states, err := identity.Replay(kel)
	if err != nil || len(states) == 0 {
		t.Fatalf("KEL does not replay: %v", err)
	}
	return states[len(states)-1].AID
}

// relay-sign's headers are what the hub checks: a signature by the loaded
// identity over PreimageV2 of exactly this request on this hub.
func TestRelaySignOutputIsAcceptedByAWire2Hub(t *testing.T) {
	hub := newFakeHub(t)
	dir, c := withIdentity(t)
	hub.register(t, c)
	body := `{"to_aid":"x","envelope":"AA=="}`
	bodyFile := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(bodyFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := capture(t, func() error {
		return cmdRelaySign([]string{"--home", dir, "--hub", hub.URL, "--action", relayauth.ActionSend,
			"--method", "POST", "--path", "/relay/send", "--body-file", bodyFile})
	})
	req, err := http.NewRequest(http.MethodPost, hub.URL+"/relay/send", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("not a header line: %q", line)
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the hub refused the fixture's signature: %d %v", resp.StatusCode, hub.errs)
	}

	// The same headers over another body are a different request.
	req2, _ := http.NewRequest(http.MethodPost, hub.URL+"/relay/send", strings.NewReader(body+" "))
	req2.Header = req.Header.Clone()
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Fatal("the signature does not cover the body")
	}
}

// The wire-1 form stays available for the hub taskboard only when asked for.
func TestRelaySignRefusesToGuessTheRequest(t *testing.T) {
	dir, _ := withIdentity(t)
	if err := cmdRelaySign([]string{"--home", dir, "--action", "task.create"}); err == nil ||
		!strings.Contains(err.Error(), "--v1") {
		t.Fatalf("no --path and no --v1: %v", err)
	}
	out := capture(t, func() error { return cmdRelaySign([]string{"--home", dir, "--v1", "--action", "task.create"}) })
	var v1 map[string]any
	if err := json.Unmarshal([]byte(out), &v1); err != nil || v1["sig"] == nil || v1["aid"] == nil {
		t.Fatalf("--v1 output = %q", out)
	}
}

// relay-send hands the same bytes to both paths: the hub gets them in a
// relay-v2-signed /relay/send, the peer wire in a v2 "send" frame.
func TestRelaySendDeliversTheSameBytesOnBothPaths(t *testing.T) {
	hub := newFakeHub(t)
	dir, c := withIdentity(t)
	hub.register(t, c)

	sock := filepath.Join(t.TempDir(), "p.wire")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan peerFrame, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var f peerFrame
		if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&f); err != nil {
			return
		}
		got <- f
		_ = json.NewEncoder(conn).Encode(peerFrame{Op: "send", V: 2, ID: f.ID})
	}()

	env := base64.StdEncoding.EncodeToString([]byte("sealed bytes"))
	out := capture(t, func() error {
		return cmdRelaySend([]string{"--home", dir, "--hub", hub.URL, "--p2p", sock,
			"--to", "did:anet:prov", "--envelope", env})
	})
	var res struct {
		Hub *hubOutcome `json:"hub"`
		P2P *p2pOutcome `json:"p2p"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res.Hub == nil || res.Hub.Code != 200 || res.Hub.Status != "queued" || res.P2P == nil || !res.P2P.OK {
		t.Fatalf("outcomes: %s", out)
	}
	f := <-got
	if f.Op != "send" || f.V != 2 || f.To != "did:anet:prov" || f.Envelope != env || f.ID == "" {
		t.Fatalf("peer frame = %+v", f)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.sends) != 1 || hub.sends[0].Envelope != env || hub.sends[0].ToAID != "did:anet:prov" {
		t.Fatalf("hub got %+v", hub.sends)
	}
}
