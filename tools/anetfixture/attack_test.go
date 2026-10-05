package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
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

	"github.com/ANetResearch/ANetCore/coredet"
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
// keys), verifies relay v2 on the signed routes and the session MAC on POST
// /relay/send the way a wire-3 hub does.
type fakeHub struct {
	*httptest.Server
	mu       sync.Mutex
	kels     map[string][]identity.SignedEvent
	keys     map[string][]byte
	sessions map[string][]byte // sid → session key
	sends    []fakeSend
	errs     []string
}

// fakeSend is one /relay/send the fake took: the envelope and the session's
// owner.
type fakeSend struct {
	from string
	env  []byte
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{kels: map[string][]identity.SignedEvent{}, keys: map[string][]byte{}, sessions: map[string][]byte{}}
	owners := map[string]string{}
	refuse := func(w http.ResponseWriter, code int, why string) {
		h.mu.Lock()
		h.errs = append(h.errs, why)
		h.mu.Unlock()
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": "unauthorized", "error": why})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hub/identity", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(hubapi.HubIdentity{AID: testHubAID, KEL: "AA"})
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
			KeySet: hubapi.B64(ks), KEL: hubapi.B64(kb)})
	})
	mux.HandleFunc("POST /relay/poll", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if _, err := h.verify(r, relayauth.ActionPoll, body); err != "" {
			refuse(w, http.StatusUnauthorized, err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []any{}, "more": false})
	})
	mux.HandleFunc("POST "+hubapi.RelaySessionPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		aid, err := h.verify(r, relayauth.ActionSession, body)
		if err != "" {
			refuse(w, http.StatusUnauthorized, err)
			return
		}
		var req hubapi.SessionRequest
		_ = json.Unmarshal(body, &req)
		epk, derr := hubapi.DecodeB64(req.EPK, relayauth.SessionPublicKeyLen)
		if derr != nil {
			refuse(w, http.StatusBadRequest, "epk")
			return
		}
		sid, key := randBytes(16), randBytes(32)
		enc, ct, serr := relayauth.SealSessionKey(epk, testHubAID, sid, key)
		if serr != nil {
			refuse(w, http.StatusBadRequest, serr.Error())
			return
		}
		h.mu.Lock()
		h.sessions[string(sid)], owners[string(sid)] = key, aid
		h.mu.Unlock()
		_ = json.NewEncoder(w).Encode(hubapi.SessionResponse{SID: hubapi.B64(sid),
			Exp: uint64(time.Now().Add(time.Hour).UnixMilli()), Enc: hubapi.B64(enc), CT: hubapi.B64(ct)})
	})
	mux.HandleFunc("POST "+hubapi.RelaySendPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get(hubapi.WireVersionHeader) != "3" || r.Header.Get("Content-Type") != hubapi.EnvelopeContentType ||
			r.Header.Get(relayauth.HeaderAID) != "" || r.Header.Get(relayauth.HeaderSig) != "" {
			refuse(w, http.StatusBadRequest, "headers")
			return
		}
		outer, err := seal.ParseOuter(body)
		if err != nil {
			refuse(w, http.StatusBadRequest, "envelope: "+err.Error())
			return
		}
		access, _ := coredet.Marshal(outer.Access)
		if r.Header.Get(hubapi.HeaderAccess) != hubapi.B64(access) {
			refuse(w, http.StatusBadRequest, "access")
			return
		}
		sid, err1 := relayauth.DecodeSessionID(r.Header.Get(hubapi.HeaderSession))
		mac, err2 := relayauth.DecodeMAC(r.Header.Get(hubapi.HeaderMAC))
		ts, err3 := strconv.ParseUint(r.Header.Get(relayauth.HeaderTS), 10, 64)
		h.mu.Lock()
		key, owner := h.sessions[string(sid)], owners[string(sid)]
		h.mu.Unlock()
		if err1 != nil || err2 != nil || err3 != nil || key == nil ||
			relayauth.VerifySessionMAC(key, sid, ts, r.Method, r.URL.RequestURI(), r.Header.Get(hubapi.HeaderHome), body, mac) != nil {
			refuse(w, http.StatusUnauthorized, "session")
			return
		}
		h.mu.Lock()
		h.sends = append(h.sends, fakeSend{from: owner, env: body})
		h.mu.Unlock()
		_ = json.NewEncoder(w).Encode(hubapi.RelaySendResponse{Status: hubapi.RelayQueued, RID: hubapi.B64(randBytes(16))})
	})
	h.Server = httptest.NewServer(mux)
	t.Cleanup(h.Close)
	return h
}

// randBytes is n random bytes.
func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// verify is the hub's relay v2 check (ANetHub auth2.go verifyV2), minus the
// replay cache. It returns the signer.
func (h *fakeHub) verify(r *http.Request, action string, body []byte) (string, string) {
	if r.Header.Get(hubapi.WireVersionHeader) != "3" {
		return "", "no wire version"
	}
	aid := r.Header.Get(relayauth.HeaderAID)
	ts, err1 := strconv.ParseUint(r.Header.Get(relayauth.HeaderTS), 10, 64)
	seq, err2 := strconv.ParseUint(r.Header.Get(relayauth.HeaderSeq), 10, 64)
	sig, err3 := relayauth.DecodeSig(r.Header.Get(relayauth.HeaderSig))
	if err1 != nil || err2 != nil || err3 != nil {
		return "", "malformed headers"
	}
	h.mu.Lock()
	kel := h.kels[aid]
	h.mu.Unlock()
	pre := relayauth.PreimageV2(action, aid, testHubAID, ts, r.Method, r.URL.RequestURI(), body)
	if err := identity.VerifyObject(kel, aid, seq, ts, pre, sig); err != nil {
		return "", "signature: " + err.Error()
	}
	return aid, ""
}

// mboxPub is a fresh Ed25519 public key for a test key set's mailbox
// signing key.
func mboxPub(t *testing.T) []byte {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

// register publishes c's KEL and a fresh key set, and returns the key pair
// so the test can open what is sealed to c.
func (h *fakeHub) register(t *testing.T, c *identity.Controller) *seal.KeyPair {
	t.Helper()
	now := uint64(time.Now().UnixMilli())
	kp, err := seal.GenerateKeyPair(seal.SuiteX25519, now-1000, now-1000+seal.MaxKeyValidityMS)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := seal.SignEncKeySet(&seal.EncKeySet{Type: seal.EncKeySetType, AID: c.AID(), Seq: now,
		Keys: []seal.EncKey{kp.Public}, IssuedAt: now, Mbox: seal.Mbox{Epoch: 1, Pub: mboxPub(t)}}, c.Sign)
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
	op, err := seal.Open(env, recipient, seal.StaticKeyRing{kp}, nil)
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
		if err := seal.VerifyInnerSig(in, op.Preimage, op.KEL, now); err != nil {
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
		err := seal.VerifyInnerSig(&op.Inner, op.Preimage, op.KEL, now)
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
	if err := seal.VerifyInnerSig(&op.Inner, op.Preimage, op.KEL, now); err != nil || op.Inner.From != req.AID() {
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
func TestRelaySignOutputIsAcceptedByTheHub(t *testing.T) {
	hub := newFakeHub(t)
	dir, c := withIdentity(t)
	hub.register(t, c)
	body := `{"limit":1}`
	bodyFile := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(bodyFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := capture(t, func() error {
		return cmdRelaySign([]string{"--home", dir, "--hub", hub.URL, "--action", relayauth.ActionPoll,
			"--method", "POST", "--path", "/relay/poll", "--body-file", bodyFile})
	})
	req, err := http.NewRequest(http.MethodPost, hub.URL+"/relay/poll", strings.NewReader(body))
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
	req2, _ := http.NewRequest(http.MethodPost, hub.URL+"/relay/poll", strings.NewReader(body+" "))
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

// relay-sign signs a request it was told in full, never one it guessed.
func TestRelaySignRefusesToGuessTheRequest(t *testing.T) {
	dir, _ := withIdentity(t)
	if err := cmdRelaySign([]string{"--home", dir, "--action", relayauth.ActionPoll}); err == nil ||
		!strings.Contains(err.Error(), "--path") {
		t.Fatalf("no --path: %v", err)
	}
}

// relay-send hands the same bytes to both paths: the hub gets them as a
// first contact (a session opened with the sender's signature, the send
// under its MAC, no identity header), the peer wire in a v3 "send" frame.
func TestRelaySendDeliversTheSameBytesOnBothPaths(t *testing.T) {
	hub := newFakeHub(t)
	_, prov := withIdentity(t)
	dir, c := withIdentity(t)
	hub.register(t, prov)
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
		_ = json.NewEncoder(conn).Encode(peerFrame{Op: "send", V: 3, ID: f.ID})
	}()

	envB64 := capture(t, func() error {
		return cmdSeal([]string{"--home", dir, "--hub", hub.URL, "--to", prov.AID(), "--type", "message", "--ix", "ix_1", "--text", "hi"})
	})
	env, err := base64.StdEncoding.DecodeString(envB64)
	if err != nil {
		t.Fatal(err)
	}
	out := capture(t, func() error {
		return cmdRelaySend([]string{"--home", dir, "--hub", hub.URL, "--p2p", sock,
			"--to", prov.AID(), "--envelope", envB64})
	})
	var res struct {
		Hub *hubOutcome `json:"hub"`
		P2P *p2pOutcome `json:"p2p"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res.Hub == nil || res.Hub.Code != 200 || res.Hub.Status != "queued" || res.Hub.RID == "" || res.P2P == nil || !res.P2P.OK {
		t.Fatalf("outcomes: %s (hub refusals %v)", out, hub.errs)
	}
	f := <-got
	if f.Op != "send" || f.V != 3 || f.To != prov.AID() || f.ID == "" || f.Envelope != hubapi.B64(env) {
		t.Fatalf("peer frame = %+v", f)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.sends) != 1 || !bytes.Equal(hub.sends[0].env, env) || hub.sends[0].from != c.AID() {
		t.Fatalf("hub got %d sends (refusals %v)", len(hub.sends), hub.errs)
	}
}
