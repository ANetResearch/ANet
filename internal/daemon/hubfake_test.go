package daemon

// hubfake_test.go is an in-memory stand-in for the Hub service (a separate repository; only its wire
// types live in internal/hubapi). The daemon tests run against this fake instead of a real Hub. It
// implements the endpoints the daemon's HTTP client calls, with wire shapes identical to the real Hub
// at wire 2 (A2A-DESIGN §3.7), including every refusal path of the relay:
//
//   - relayauth v2 on every signed endpoint: the X-ANet-* headers are verified against the caller's
//     registered KEL (the body KEL for /register), with the hub AID, method, request target and raw
//     body bound, a ±5 minute window and a replay cache — 401 on any failure;
//   - /relay/*: 426 to a request without X-ANet-Wire >= 2, 400 for a body or envelope that does not
//     parse or whose outer to differs from to_aid, 404 for an unknown recipient, 413 over the envelope
//     cap, 429 (with Retry-After) over the per-sender budget, 507 over the per-recipient mailbox cap;
//   - GET/POST /agents/{aid}/keys with the publisher high-water rule (200 / 409), and a federated
//     lookup through peer fakes for AIDs registered elsewhere;
//   - GET /hub/identity.
//
// Relay messages are OPAQUE: the fake stores the envelope bytes keyed by to_aid and nothing else. It
// applies seal.ParseOuter, the structural check the real hub applies, and never opens a payload — a
// fake that read payloads could make a test pass that the real hub would fail.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/adp"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/relayauth"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// fakeHubAgent is one registered agent's row in the fake registry.
type fakeHubAgent struct {
	view hubapi.AgentView
	kel  []byte
	// keyset is the agent's SignedEncKeySet encoding, as last accepted.
	keyset []byte
}

// fakeHubMsg is one queued relay message: the real hub's relay_message row
// at wire 2 (id, to_aid, payload, size, created_at). No sender, no kind, no
// interaction id.
type fakeHubMsg struct {
	id        int64
	toAID     string
	payload   []byte
	createdAt string
}

// fakeHub is the in-memory Hub: registry + relay mailboxes + verified-review store
// + the credit ledger it is custodian of.
type fakeHub struct {
	mu      sync.Mutex
	nextID  int64
	agents  map[string]*fakeHubAgent
	mailbox []*fakeHubMsg
	reviews map[string]hubapi.ReviewView // interaction_id → stored review (one per interaction)
	// The credit side. Balances are the hub's, which is the whole point of
	// the custody note in pay.go — the fake holds them the same way.
	self    *identity.Controller
	balance map[string]uint64
	entries map[string][]map[string]any
	settled map[string]string // authorization id → transaction, so replay is idempotent
	// cardHighWater is the per-subject high water the card gate compares
	// against — the same rule the real hub keeps in agent_card.seq.
	cardHighWater map[string]uint64
	// departedKEL keeps a deregistered agent's key history, so a receipt it
	// signed before leaving can still be verified.
	departedKEL map[string][]byte
	// relaySends counts deliveries the hub actually carried, so a test can
	// tell "the hub delivered it" from "something else did".
	relaySends int
	// lastSeen is when each agent last collected its mail — the real hub's
	// agent.last_seen_at, updated by register and by every poll. It is what
	// /relay/send answers recipient_quiet from, so a fake without it makes
	// the whole liveness contract invisible on this side.
	lastSeen map[string]time.Time

	// wire is the X-ANet-Wire version this hub states (default 2). Setting
	// it to 1 makes the fake answer like a pre-wire-2 hub.
	wire int
	// Relay limits. Zero means the default (envelope cap) or no limit.
	maxEnvelope int // 413 above this many envelope bytes
	senderLimit int // 429 once a sender has sent this many envelopes
	mailboxCap  int // 507 once a recipient holds this many undelivered envelopes
	sendsBy     map[string]int
	// sigSeen is the relayauth replay cache: (aid, signature) pairs seen
	// inside the skew window.
	sigSeen map[string]bool
	// keysOverride makes GET /agents/{aid}/keys answer with someone else's
	// material: a hostile hub substituting keys.
	keysOverride map[string]hubapi.KeysResponse
	// peers are other fake hubs asked for keys of AIDs not registered
	// here, standing in for /fed/v2/keys/{aid}.
	peers []*fakeHub
	// authFailures counts 401 answers, so a test can tell "refused by the
	// hub" from "never sent".
	authFailures int
	// registerKeys is the keys_status of each agent's last registration.
	registerKeys map[string]string
	// a2aCards holds each agent's admitted A2A network card (raw bytes) and
	// a2aMarks its params.seq high water, the admission hub brief H1
	// describes: a2acard.Verify, then CheckHighWater. registerCards is the
	// card_status of each agent's last registration, and a2aCardSends
	// counts registrations that carried a card.
	a2aCards      map[string][]byte
	a2aMarks      map[string]a2acard.Mark
	registerCards map[string]string
	a2aCardSends  int
	// agentsQueries records the query string of every GET /agents, so a
	// test can tell what a search sent to the hub.
	agentsQueries []string
}

// fakeHubDefaultMaxEnvelope is the real hub's per-envelope cap (96 MiB).
const fakeHubDefaultMaxEnvelope = 96 << 20

// fakeHubQuietAfter mirrors aghub.QuietAfter: how long without collecting
// mail before the hub reports a recipient as quiet.
const fakeHubQuietAfter = time.Hour

// fakeHubRoundDuration renders a gap the way the real hub's roundDuration
// does, so the sentence a test sees is the sentence production produces.
func fakeHubRoundDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}

// newFakeHub starts an httptest server backed by a fresh fake Hub and cleans it up with the test.
func newFakeHub(t *testing.T) *httptest.Server {
	t.Helper()
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHub{
		agents: map[string]*fakeHubAgent{}, reviews: map[string]hubapi.ReviewView{},
		cardHighWater: map[string]uint64{},
		departedKEL:   map[string][]byte{},
		self:          self,
		balance:       map[string]uint64{}, entries: map[string][]map[string]any{},
		settled:  map[string]string{},
		lastSeen: map[string]time.Time{},
		wire:     hubapi.WireVersion,
		sendsBy:  map[string]int{}, sigSeen: map[string]bool{},
		keysOverride: map[string]hubapi.KeysResponse{},
		registerKeys: map[string]string{},
		a2aCards:     map[string][]byte{}, a2aMarks: map[string]a2acard.Mark{},
		registerCards: map[string]string{},
	}
	// The hub is an agent on its own registry, so its settlement
	// signatures can be checked the same way everyone else's are.
	selfKEL, err := identity.MarshalKEL(self.KEL())
	if err != nil {
		t.Fatal(err)
	}
	h.agents[self.AID()] = &fakeHubAgent{
		view: hubapi.AgentView{AID: self.AID(), Name: "hub"}, kel: selfKEL}
	srv := httptest.NewServer(h.handler())
	t.Cleanup(srv.Close)
	hubsByURL.Store(srv.URL, h)
	return srv
}

// fakeHubAt returns the fake behind a server URL.
func fakeHubAt(t *testing.T, url string) *fakeHub {
	t.Helper()
	v, ok := hubsByURL.Load(url)
	if !ok {
		t.Fatal("no fake hub at " + url)
	}
	return v.(*fakeHub)
}

// hubsByURL lets a test reach the fake behind a server URL.
var hubsByURL sync.Map

// relayCount reports how many deliveries this hub carried.
func relayCountFor(url string) int {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return 0
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.relaySends
}

func (h *fakeHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", h.hRegister)
	mux.HandleFunc("POST /agents/{aid}/deregister", h.hDeregister)
	mux.HandleFunc("POST /agents/{aid}/visibility", h.hVisibility)
	mux.HandleFunc("POST /agents/{aid}/p2p", h.hP2P)
	mux.HandleFunc("POST /profile", h.hProfile)
	mux.HandleFunc("GET /agents", h.hAgents)
	mux.HandleFunc("GET /agents/{aid}", h.hAgent)
	mux.HandleFunc("POST /reviews", h.hUploadReview)
	mux.HandleFunc("POST /relay/send", h.hRelaySend)
	mux.HandleFunc("POST /relay/poll", h.hRelayPoll)
	mux.HandleFunc("POST /relay/ack", h.hRelayAck)
	mux.HandleFunc("GET /agents/{aid}/keys", h.hKeysGet)
	mux.HandleFunc("POST /agents/{aid}/keys", h.hKeysPost)
	// The facilitator half of the contract. A fake that serves only the
	// endpoints the happy path happens to touch is how a seam gets shipped
	// unconnected — the real hub answers these, so this one does too.
	mux.HandleFunc("GET /hub/identity", h.hIdentity)
	mux.HandleFunc("GET /agents/{aid}/kel", h.hAgentKEL)
	mux.HandleFunc("POST /x402/settle", h.hSettle)
	mux.HandleFunc("GET /agents/{aid}/balance", h.hBalance)
	mux.HandleFunc("GET /agents/{aid}/ledger", h.hLedgerRead)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		wire := h.wire
		h.mu.Unlock()
		w.Header().Set(hubapi.WireVersionHeader, strconv.Itoa(wire))
		// The real wire-2 hub refuses /relay/* from a daemon that states no
		// version or an older one, with a body naming the release needed.
		if wire >= 2 && strings.HasPrefix(r.URL.Path, "/relay/") {
			if v, err := strconv.Atoi(r.Header.Get(hubapi.WireVersionHeader)); err != nil || v < 2 {
				fakeHubJSON(w, http.StatusUpgradeRequired, map[string]string{"error": "this hub requires anet >= 0.2.0 (wire 2)"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// readBody reads a request body under a cap, answering 413 over it.
func (h *fakeHub) readBody(w http.ResponseWriter, r *http.Request, limit int) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "body unreadable"})
		return nil, false
	}
	if len(b) > limit {
		fakeHubJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request too large"})
		return nil, false
	}
	return b, true
}

// verifyAuth checks relayauth v2 headers against kel (or, when kel is nil,
// against the registered KEL of the header AID) for action, over the raw
// body the handler read. It answers 401 itself and returns "" on failure.
func (h *fakeHub) verifyAuth(w http.ResponseWriter, r *http.Request, body []byte, action string, kel []byte) string {
	fail := func(why string) string {
		h.mu.Lock()
		h.authFailures++
		h.mu.Unlock()
		fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "auth: " + why})
		return ""
	}
	aid := r.Header.Get(hubapi.HeaderAID)
	ts, err1 := strconv.ParseUint(r.Header.Get(hubapi.HeaderTS), 10, 64)
	seq, err2 := strconv.ParseUint(r.Header.Get(hubapi.HeaderSeq), 10, 64)
	sigHdr := r.Header.Get(hubapi.HeaderSig)
	sig, err3 := relayauth.DecodeSig(sigHdr)
	if aid == "" || err1 != nil || err2 != nil || err3 != nil {
		return fail("missing or malformed X-ANet-* headers")
	}
	now := uint64(time.Now().UnixMilli())
	if ts+relayauth.MaxSkewMillis < now || ts > now+relayauth.MaxSkewMillis {
		return fail("timestamp outside the window")
	}
	if kel == nil {
		h.mu.Lock()
		a := h.agents[aid]
		h.mu.Unlock()
		if a == nil {
			return fail("unknown agent")
		}
		kel = a.kel
	}
	events, err := identity.UnmarshalKEL(kel)
	if err != nil {
		return fail("KEL undecodable")
	}
	pre := relayauth.PreimageV2(action, aid, h.self.AID(), ts, r.Method, r.URL.RequestURI(), body)
	if err := identity.VerifyObject(events, aid, seq, ts, pre, sig); err != nil {
		return fail("signature: " + err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sigSeen[aid+"|"+sigHdr] {
		h.authFailures++
		fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "auth: replayed signature"})
		return ""
	}
	h.sigSeen[aid+"|"+sigHdr] = true
	return aid
}

func fakeHubJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// hRegister mirrors the Hub's POST /register at wire 2: relayauth v2 against
// the KEL in the body, which must replay to the registrant's AID and extend
// any KEL already held (409 otherwise); the card gate; and enc_keys,
// reported per field rather than failing the registration.
func (h *fakeHub) hRegister(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<20)
	if !ok {
		return
	}
	var req hubapi.RegisterRequest
	if err := json.Unmarshal(body, &req); err != nil || req.AID == "" || req.KEL == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "aid + kel required"})
		return
	}
	kelBytes, err := base64.StdEncoding.DecodeString(req.KEL)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "kel not base64"})
		return
	}
	kelEvents, err := identity.UnmarshalKEL(kelBytes)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "kel undecodable"})
		return
	}
	if got, err := replayedAID(kelEvents); err != nil || got != req.AID {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "kel does not replay to aid"})
		return
	}
	if aid := h.verifyAuth(w, r, body, relayauth.ActionRegister, kelBytes); aid != req.AID {
		if aid != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the registrant"})
		}
		return
	}
	h.mu.Lock()
	if prior, ok := h.agents[req.AID]; ok {
		old, _ := identity.UnmarshalKEL(prior.kel)
		if err := identity.ExtendsKEL(old, kelEvents); err != nil {
			h.mu.Unlock()
			fakeHubJSON(w, http.StatusConflict, map[string]string{"error": "kel does not extend the registered one: " + err.Error()})
			return
		}
	}
	h.mu.Unlock()
	// The card gate, with the same rule the real hub applies.
	//
	// This fake used not to read the card field at all. That made every
	// card-related defect invisible on this side: the daemon minted a
	// sequence, the fake stored an agent, both suites went green, and the
	// hub refused the registration in production. A fake that implements
	// only the happy path is a fake that agrees with whatever the code
	// does — see the STALE_SEQ defect this arrived with.
	if len(req.Card) > 0 {
		var card adp.AgentCard
		if err := json.Unmarshal(req.Card, &card); err != nil {
			fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "card malformed"})
			return
		}
		if card.SubjectDID != req.AID {
			fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "card subject is not the registrant"})
			return
		}
		h.mu.Lock()
		high := h.cardHighWater[req.AID]
		h.mu.Unlock()
		if _, err := adp.AdmitCard(&card, time.Now(), high, kelEvents, map[uint16]bool{1: true}, nil); err != nil {
			fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "card refused: " + err.Error()})
			return
		}
		h.mu.Lock()
		h.cardHighWater[req.AID] = card.Seq
		h.mu.Unlock()
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[req.AID]
	if !ok {
		a = &fakeHubAgent{view: hubapi.AgentView{
			AID:          req.AID,
			RegisteredAt: time.Now().UTC().Format(time.RFC3339),
		}}
		h.agents[req.AID] = a
	}
	a.view.Name = req.Name
	a.view.Caps = req.Caps
	a.kel = kelBytes
	// Registering counts as being seen: the real hub writes last_seen_at on
	// register too, so a node that just joined is never reported quiet.
	h.lastSeen[req.AID] = time.Now()
	out := hubapi.RegisterResponse{AID: req.AID, Status: "registered", KeysStatus: hubapi.KeysStatusAbsent, CardStatus: "absent"}
	if req.EncKeys != "" {
		out.KeysStatus, out.KeysError = h.admitKeysLocked(a, req.EncKeys, kelEvents)
	}
	h.registerKeys[req.AID] = out.KeysStatus
	if len(req.A2ACard) > 0 {
		h.a2aCardSends++
		out.CardStatus, out.CardError = h.admitA2ACardLocked(req.AID, req.A2ACard, kelEvents)
	}
	h.registerCards[req.AID] = out.CardStatus
	fakeHubJSON(w, http.StatusOK, out)
}

// admitA2ACardLocked admits an A2A network card for a registering agent
// (A2A-DESIGN §3.7, §10.3; hub brief H1): verified against the submitted
// KEL, then the params.seq rule against the stored mark. The caller holds
// h.mu.
func (h *fakeHub) admitA2ACardLocked(aid string, raw []byte, kel []identity.SignedEvent) (string, string) {
	v, err := a2acard.Verify(raw, func(a string) ([]identity.SignedEvent, error) {
		if a != aid {
			return nil, fmt.Errorf("not the registrant")
		}
		return kel, nil
	}, uint64(time.Now().UnixMilli()))
	if err != nil {
		return hubapi.CardStatusInvalid, err.Error()
	}
	if v.AID != aid {
		return hubapi.CardStatusInvalid, "card is signed for another AID"
	}
	var stored *a2acard.Mark
	if m, ok := h.a2aMarks[aid]; ok {
		stored = &m
	}
	dec, err := a2acard.CheckHighWater(stored, v.Mark())
	if err != nil {
		return hubapi.CardStatusConflict, err.Error()
	}
	h.a2aCards[aid] = append([]byte(nil), raw...)
	h.a2aMarks[aid] = v.Mark()
	if dec == a2acard.Same {
		return hubapi.CardStatusUnchanged, ""
	}
	return hubapi.CardStatusOK, ""
}

// admitKeysLocked applies the publisher rules of §3.1 to a key set for a
// registered agent and returns the per-field status (hubapi.KeysStatus*)
// and, for a refusal, why. The caller holds h.mu.
func (h *fakeHub) admitKeysLocked(a *fakeHubAgent, b64 string, kel []identity.SignedEvent) (string, string) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return hubapi.KeysStatusInvalid, "not base64"
	}
	signed, err := seal.UnmarshalSignedEncKeySet(raw)
	if err != nil {
		return hubapi.KeysStatusInvalid, "undecodable"
	}
	set, err := seal.VerifyEncKeySet(signed, a.view.AID, kel, uint64(time.Now().UnixMilli()))
	if err != nil {
		return hubapi.KeysStatusInvalid, err.Error()
	}
	var seen *seal.Seen
	if len(a.keyset) > 0 {
		if prev, err := seal.UnmarshalSignedEncKeySet(a.keyset); err == nil {
			var ps seal.EncKeySet
			if coredet.Unmarshal(prev.Set, &ps) == nil {
				seen = &seal.Seen{Seq: ps.Seq, Set: prev.Set}
			}
		}
	}
	switch seal.DecideHighWater(seen, set.Seq, signed.Set) {
	case seal.Replace:
		a.keyset = raw
		return hubapi.KeysStatusOK, ""
	case seal.Same:
		return hubapi.KeysStatusUnchanged, ""
	default:
		return hubapi.KeysStatusConflict, "seq does not supersede the stored key set"
	}
}

// hKeysPost mirrors POST /agents/{aid}/keys: signed by the AID itself,
// verified against the registered KEL, publisher high-water (200 or 409).
func (h *fakeHub) hKeysPost(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<20)
	if !ok {
		return
	}
	aid := h.verifyAuth(w, r, body, relayauth.ActionKeys, nil)
	if aid == "" {
		return
	}
	if aid != r.PathValue("aid") {
		fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the path AID"})
		return
	}
	var req hubapi.KeysPublishRequest
	if err := json.Unmarshal(body, &req); err != nil || req.KeySet == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "keyset required"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.agents[aid]
	kel, err := identity.UnmarshalKEL(a.kel)
	if err != nil {
		fakeHubJSON(w, http.StatusInternalServerError, map[string]string{"error": "stored kel unreadable"})
		return
	}
	switch st, why := h.admitKeysLocked(a, req.KeySet, kel); st {
	case hubapi.KeysStatusOK, hubapi.KeysStatusUnchanged:
		fakeHubJSON(w, http.StatusOK, hubapi.KeysPublishResponse{AID: aid, KeysStatus: st})
	case hubapi.KeysStatusConflict:
		fakeHubJSON(w, http.StatusConflict, map[string]string{"error": why})
	default:
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": why})
	}
}

// hKeysGet mirrors GET /agents/{aid}/keys: local agents first, then peer
// hubs (the federated lookup), else 404.
func (h *fakeHub) hKeysGet(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	if out, ok := h.keysFor(aid, true); ok {
		fakeHubJSON(w, http.StatusOK, out)
		return
	}
	fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "no keys for " + aid})
}

// keysFor answers a keys lookup for aid; federate asks the peer fakes.
func (h *fakeHub) keysFor(aid string, federate bool) (hubapi.KeysResponse, bool) {
	h.mu.Lock()
	if out, ok := h.keysOverride[aid]; ok {
		h.mu.Unlock()
		return out, true
	}
	a := h.agents[aid]
	peers := append([]*fakeHub(nil), h.peers...)
	var out hubapi.KeysResponse
	found := a != nil && len(a.keyset) > 0
	if found {
		out = hubapi.KeysResponse{AID: aid, KeySet: base64.StdEncoding.EncodeToString(a.keyset),
			KEL: base64.StdEncoding.EncodeToString(a.kel)}
	}
	h.mu.Unlock()
	if found || !federate {
		return out, found
	}
	for _, p := range peers {
		if out, ok := p.keysFor(aid, false); ok {
			return out, true
		}
	}
	return hubapi.KeysResponse{}, false
}

// hDeregister mirrors POST /agents/{aid}/deregister.
//
// The fake had no deregister at all, so hub-leave answered 404 here while
// working against the real hub — the same shape of gap as the card gate:
// a fake that implements only the happy path agrees with whatever the code
// does. It removes the routing and keeps the evidence, which is the real
// hub's rule: reviews and balances record things that happened, and
// deleting them to tidy the directory would be rewriting history.
func (h *fakeHub) hDeregister(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	body, ok := h.readBody(w, r, 1<<16)
	if !ok {
		return
	}
	h.mu.Lock()
	_, known := h.agents[aid]
	h.mu.Unlock()
	if !known {
		fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "no such agent"})
		return
	}
	if who := h.verifyAuth(w, r, body, relayauth.ActionDeregister, nil); who != aid {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the path AID"})
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.agents[aid]
	// Undelivered mail is reported rather than dropped silently: somebody
	// sent work and is waiting for it.
	undelivered := 0
	for _, m := range h.mailbox {
		if m.toAID == aid {
			undelivered++
		}
	}
	delete(h.agents, aid)
	h.departedKEL[aid] = a.kel // evidence stays verifiable after the routing goes
	fakeHubJSON(w, http.StatusOK, map[string]any{
		"aid": aid, "status": "deregistered", "undelivered": undelivered,
	})
}

// hProfile mirrors POST /profile (relayauth v2, action profile).
func (h *fakeHub) hProfile(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<20)
	if !ok {
		return
	}
	var req struct {
		AID     string `json:"aid"`
		Summary string `json:"summary"`
		Readme  string `json:"readme"`
		Pricing string `json:"pricing"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.AID == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "aid required"})
		return
	}
	if who := h.verifyAuth(w, r, body, relayauth.ActionProfile, nil); who != req.AID {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the profile's agent"})
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[req.AID]
	if !ok {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "agent not registered"})
		return
	}
	a.view.Summary, a.view.Readme, a.view.Pricing = req.Summary, req.Readme, req.Pricing
	fakeHubJSON(w, http.StatusOK, map[string]any{"aid": req.AID, "status": "profile_updated"})
}

// hVisibility mirrors POST /agents/{aid}/visibility (relayauth v2).
func (h *fakeHub) hVisibility(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<16)
	if !ok {
		return
	}
	if who := h.verifyAuth(w, r, body, relayauth.ActionVisibility, nil); who != r.PathValue("aid") {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the path AID"})
		}
		return
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"status": "set"})
}

// hP2P mirrors POST /agents/{aid}/p2p (relayauth v2).
func (h *fakeHub) hP2P(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<16)
	if !ok {
		return
	}
	if who := h.verifyAuth(w, r, body, relayauth.ActionP2P, nil); who != r.PathValue("aid") {
		if who != "" {
			fakeHubJSON(w, http.StatusUnauthorized, map[string]string{"error": "signer is not the path AID"})
		}
		return
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"status": "published"})
}

// viewWithAggregates fills the derived fields (Listed + rating aggregate) the way the real Hub does.
// Caller must hold h.mu.
func (h *fakeHub) viewWithAggregates(a *fakeHubAgent) hubapi.AgentView {
	v := a.view
	v.Listed = len(v.Caps) > 0 || v.Summary != "" || v.Readme != "" || v.Pricing != ""
	var sum, n int
	for _, rv := range h.reviews {
		if rv.SubjectAID == v.AID {
			sum += rv.Rating
			n++
		}
	}
	v.ReviewCount = n
	if n > 0 {
		v.AvgRating = float64(sum) / float64(n)
	}
	return v
}

// hAgents mirrors GET /agents?q= — LISTED agents only, case-insensitive substring filter.
func (h *fakeHub) hAgents(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agentsQueries = append(h.agentsQueries, r.URL.RawQuery)
	agents := []hubapi.AgentView{}
	for _, a := range h.agents {
		v := h.viewWithAggregates(a)
		if !v.Listed {
			continue
		}
		if q != "" {
			hay := strings.ToLower(v.AID + " " + v.Name + " " + strings.Join(v.Caps, " ") + " " + v.Summary + " " + v.Readme)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		agents = append(agents, v)
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// hAgent mirrors GET /agents/{aid} — one agent + its reviews (newest first).
func (h *fakeHub) hAgent(w http.ResponseWriter, r *http.Request) {
	aid := r.PathValue("aid")
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.agents[aid]
	if !ok {
		fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "hub: agent " + aid + " not found"})
		return
	}
	reviews := []hubapi.ReviewView{}
	for _, rv := range h.reviews {
		if rv.SubjectAID == aid {
			reviews = append(reviews, rv)
		}
	}
	sort.Slice(reviews, func(i, j int) bool { return reviews[i].CreatedAt > reviews[j].CreatedAt })
	fakeHubJSON(w, http.StatusOK, map[string]any{"agent": h.viewWithAggregates(a), "reviews": reviews})
}

// hUploadReview mirrors POST /reviews after the content removal (A2A-DESIGN §9, ANetHub
// hUploadReview): the body is the receipt and the review and nothing else; a body that still carries
// request_doc or deliverable is refused with 400, as the hub refuses it. It keeps the structural
// checks (interlock, rating range, comment length, one review per interaction) and skips the two
// signature verifications. The stored view carries no content and says so.
func (h *fakeHub) hUploadReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		hubapi.UploadReviewRequest
		RequestDoc  string `json:"request_doc"`
		Deliverable string `json:"deliverable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Receipt == "" || req.Review == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt + review required"})
		return
	}
	if req.RequestDoc != "" || req.Deliverable != "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "this hub takes no task content with a review"})
		return
	}
	rcBytes, err1 := base64.StdEncoding.DecodeString(req.Receipt)
	rvBytes, err2 := base64.StdEncoding.DecodeString(req.Review)
	if err1 != nil || err2 != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt/review not base64"})
		return
	}
	rc, err := evidence.UnmarshalReceipt(rcBytes)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt undecodable"})
		return
	}
	rv, err := evidence.UnmarshalReview(rvBytes)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "review undecodable"})
		return
	}
	if !rv.ValidRating() {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "rating out of range"})
		return
	}
	if len([]rune(rv.Comment)) > 280 {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "comment over 280 characters"})
		return
	}
	if rc.InteractionID != rv.InteractionID || rv.ReviewerAID != rc.RequesterAID || rv.SubjectAID != rc.ProviderAID {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt/review interlock mismatch"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, dup := h.reviews[rv.InteractionID]; dup {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction already reviewed"})
		return
	}
	h.reviews[rv.InteractionID] = hubapi.ReviewView{
		InteractionID:  rv.InteractionID,
		SubjectAID:     rv.SubjectAID,
		ReviewerAID:    rv.ReviewerAID,
		Rating:         rv.Rating,
		Comment:        rv.Comment,
		ReceiptCID:     rv.ReceiptCID,
		RequestCID:     rc.RequestCID,
		ResultCID:      rc.ResultCID,
		ContentBinding: hubapi.ContentBindingUnverified,
		CompletedAt:    rc.CompletedAt,
		CreatedAt:      rv.CreatedAt,
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"interaction_id": rv.InteractionID, "status": "accepted"})
}

// hRelaySend mirrors POST /relay/send at wire 2.
func (h *fakeHub) hRelaySend(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	maxEnv := h.maxEnvelope
	h.mu.Unlock()
	if maxEnv == 0 {
		maxEnv = fakeHubDefaultMaxEnvelope
	}
	// base64 plus the JSON around it.
	body, ok := h.readBody(w, r, maxEnv/3*4+4096)
	if !ok {
		return
	}
	sender := h.verifyAuth(w, r, body, relayauth.ActionSend, nil)
	if sender == "" {
		return
	}
	var req hubapi.RelaySendRequest
	if err := json.Unmarshal(body, &req); err != nil || req.ToAID == "" || req.Envelope == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "to_aid + envelope required"})
		return
	}
	env, err := base64.StdEncoding.DecodeString(req.Envelope)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "envelope not base64"})
		return
	}
	if len(env) > maxEnv {
		fakeHubJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "envelope over the cap"})
		return
	}
	// The structural check the real hub makes, and the only look at the
	// envelope it takes: the outer header, never the ciphertext.
	outer, err := seal.ParseOuter(env)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "not a sealed envelope: " + err.Error()})
		return
	}
	if outer.To != req.ToAID {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "envelope to does not match to_aid"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.agents[req.ToAID]; !ok {
		fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "recipient not registered"})
		return
	}
	if h.senderLimit > 0 && h.sendsBy[sender] >= h.senderLimit {
		w.Header().Set("Retry-After", "1")
		fakeHubJSON(w, http.StatusTooManyRequests, map[string]string{"error": "sender over its budget"})
		return
	}
	if h.mailboxCap > 0 {
		held := 0
		for _, m := range h.mailbox {
			if m.toAID == req.ToAID {
				held++
			}
		}
		if held >= h.mailboxCap {
			fakeHubJSON(w, http.StatusInsufficientStorage, map[string]string{"error": "recipient mailbox full"})
			return
		}
	}
	h.sendsBy[sender]++
	h.relaySends++
	h.nextID++
	h.mailbox = append(h.mailbox, &fakeHubMsg{
		id: h.nextID, toAID: req.ToAID, payload: env,
		createdAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	out := hubapi.RelaySendResponse{ID: h.nextID, Status: "queued"}
	// Queued either way, and the sender is told when the recipient has not
	// collected its mail in a long time — word for word what the real hub
	// puts on this response. A never-seen agent is unknown, not quiet.
	if seen, ok := h.lastSeen[req.ToAID]; ok {
		if gap := time.Since(seen); gap > fakeHubQuietAfter {
			out.RecipientQuiet = true
			out.Warning = fmt.Sprintf(
				"queued, but %s has not collected its mail for %s — it may not be running",
				req.ToAID, fakeHubRoundDuration(gap))
		}
	}
	fakeHubJSON(w, http.StatusOK, out)
}

// backdateLastSeen makes an agent look like it stopped collecting its mail
// `ago` ago, the way aghub.SetLastSeenForTest does on the real hub — the
// only way to exercise the quiet path without waiting an hour.
func backdateLastSeen(url, aid string, ago time.Duration) {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSeen[aid] = time.Now().Add(-ago)
}

// hRelayPoll mirrors POST /relay/poll — undelivered envelopes for the
// authenticated caller, oldest first.
func (h *fakeHub) hRelayPoll(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<16)
	if !ok {
		return
	}
	aid := h.verifyAuth(w, r, body, relayauth.ActionPoll, nil)
	if aid == "" {
		return
	}
	var req hubapi.RelayPollRequest
	if err := json.Unmarshal(body, &req); err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Collecting mail IS the liveness signal the real hub records (SeenPolling).
	h.lastSeen[aid] = time.Now()
	out := []hubapi.RelayMessage{}
	for _, m := range h.mailbox {
		if m.toAID != aid {
			continue
		}
		out = append(out, hubapi.RelayMessage{ID: m.id, Envelope: base64.StdEncoding.EncodeToString(m.payload)})
		if len(out) >= limit {
			break
		}
	}
	fakeHubJSON(w, http.StatusOK, hubapi.RelayPollResponse{Messages: out})
}

// hRelayAck mirrors POST /relay/ack — delete the acked rows, scoped to the
// caller's own mailbox. At wire 2 an ack deletes; nothing is retained.
func (h *fakeHub) hRelayAck(w http.ResponseWriter, r *http.Request) {
	body, ok := h.readBody(w, r, 1<<20)
	if !ok {
		return
	}
	aid := h.verifyAuth(w, r, body, relayauth.ActionAck, nil)
	if aid == "" {
		return
	}
	var req hubapi.RelayAckRequest
	if err := json.Unmarshal(body, &req); err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	drop := map[int64]bool{}
	for _, id := range req.IDs {
		drop[id] = true
	}
	kept := h.mailbox[:0]
	acked := 0
	for _, m := range h.mailbox {
		if m.toAID == aid && drop[m.id] {
			acked++
			continue
		}
		kept = append(kept, m)
	}
	h.mailbox = kept
	fakeHubJSON(w, http.StatusOK, map[string]any{"acked": acked})
}

// queuedFor returns the envelopes the hub holds for toAID, oldest first.
func queuedFor(t *testing.T, srv *httptest.Server, toAID string) [][]byte {
	t.Helper()
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	defer h.mu.Unlock()
	var out [][]byte
	for _, m := range h.mailbox {
		if m.toAID == toAID {
			out = append(out, append([]byte(nil), m.payload...))
		}
	}
	return out
}

// onlyQueuedEnvelope returns the single envelope the hub holds for toAID —
// the bytes a real second poll would deliver.
func onlyQueuedEnvelope(t *testing.T, srv *httptest.Server, toAID string) []byte {
	t.Helper()
	q := queuedFor(t, srv, toAID)
	if len(q) != 1 {
		t.Fatalf("%d envelopes queued for %s, want 1", len(q), toAID)
	}
	return q[0]
}

// injectEnvelope puts arbitrary bytes into a mailbox, the way anyone who
// can reach the hub can: the hub does not authenticate what is inside.
func injectEnvelope(t *testing.T, srv *httptest.Server, toAID string, env []byte) {
	t.Helper()
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	h.mailbox = append(h.mailbox, &fakeHubMsg{id: h.nextID, toAID: toAID, payload: env,
		createdAt: time.Now().UTC().Format(time.RFC3339Nano)})
}

// clearMailbox drops everything queued for toAID.
func clearMailbox(t *testing.T, srv *httptest.Server, toAID string) {
	t.Helper()
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.mailbox[:0]
	for _, m := range h.mailbox {
		if m.toAID != toAID {
			kept = append(kept, m)
		}
	}
	h.mailbox = kept
}

// hIdentity is how a node learns which hub's ledger its credits live on.
// The AID goes into every authorization's network field, so a payment
// signed for one hub cannot be replayed at another.
func (h *fakeHub) hIdentity(w http.ResponseWriter, _ *http.Request) {
	kel, err := identity.MarshalKEL(h.self.KEL())
	if err != nil {
		fakeHubJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	fakeHubJSON(w, http.StatusOK, hubapi.HubIdentity{AID: h.self.AID(), KEL: base64.StdEncoding.EncodeToString(kel)})
}

// grant credits an account, standing in for the admin grant and the
// registration allowance the real hub has.
func (h *fakeHub) grant(aid string, amount uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.balance[aid] += amount
	h.entries[aid] = append(h.entries[aid], map[string]any{
		"kind": "grant", "delta": amount, "balance": h.balance[aid]})
}

func balanceOf(url, aid string) uint64 {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return 0
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.balance[aid]
}

// hSettle is the facilitator: verify the payer's signature over the
// authorization, move the credit, and sign a receipt saying it did.
//
// It verifies rather than trusting, unlike the rest of this fake, because
// verification is the property under test — a settlement that took an
// unsigned authorization would let the paid-work test pass with the
// signature stripped out.
func (h *fakeHub) hSettle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PaymentPayload payment.PaymentPayload `json:"paymentPayload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fakeHubJSON(w, http.StatusBadRequest, payment.SettlementResponse{
			Success: false, ErrorReason: "malformed"})
		return
	}
	encoded, _ := req.PaymentPayload.Payload["authorization"].(string)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: false, ErrorReason: "authorization not base64"})
		return
	}
	auth, err := payment.UnmarshalAuthorization(raw)
	if err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: false, ErrorReason: "authorization malformed"})
		return
	}
	h.mu.Lock()
	payer := h.agents[auth.Payer]
	h.mu.Unlock()
	if payer == nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: false, ErrorReason: "payer unknown to this hub"})
		return
	}
	kel, err := identity.UnmarshalKEL(payer.kel)
	if err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: false, ErrorReason: "payer key history unreadable"})
		return
	}
	if err := auth.Verify(kel, time.Now().UnixMilli()); err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: false, ErrorReason: "bad signature: " + err.Error()})
		return
	}
	authID, err := auth.ID()
	if err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{Success: false, ErrorReason: err.Error()})
		return
	}
	h.mu.Lock()
	if tx, done := h.settled[authID]; done {
		h.mu.Unlock()
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: true, Transaction: tx, Network: auth.Network, Payer: auth.Payer})
		return
	}
	if h.balance[auth.Payer] < auth.Amount {
		h.mu.Unlock()
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
			Success: false, ErrorReason: payment.ReasonInsufficientFunds})
		return
	}
	h.balance[auth.Payer] -= auth.Amount
	h.balance[auth.PayTo] += auth.Amount
	tx := "tx-" + authID
	h.settled[authID] = tx
	h.entries[auth.Payer] = append(h.entries[auth.Payer], map[string]any{
		"kind": "debit", "delta": auth.Amount, "balance": h.balance[auth.Payer], "tx": tx})
	h.entries[auth.PayTo] = append(h.entries[auth.PayTo], map[string]any{
		"kind": "credit", "delta": auth.Amount, "balance": h.balance[auth.PayTo], "tx": tx})
	h.mu.Unlock()

	rec := &payment.Receipt{
		AuthID: authID, Payer: auth.Payer, PayTo: auth.PayTo,
		Amount: auth.Amount, Network: auth.Network, SettleAt: time.Now().UnixMilli(),
	}
	if err := rec.Sign(h.self); err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{Success: false, ErrorReason: err.Error()})
		return
	}
	recRaw, err := rec.Marshal()
	if err != nil {
		fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{Success: false, ErrorReason: err.Error()})
		return
	}
	fakeHubJSON(w, http.StatusOK, payment.SettlementResponse{
		Success: true, Transaction: tx, Network: auth.Network, Payer: auth.Payer,
		Amount: payment.Amount(auth.Amount),
		Extensions: map[string]any{
			payment.ExtReceipt: base64.StdEncoding.EncodeToString(recRaw),
		},
	})
}

func (h *fakeHub) hBalance(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// "credits", the field the real hub uses. A fake answering a
	// different key is a fake that certifies a wire the system does not
	// have — which is exactly how the daemon shipped reading a balance of
	// zero off a funded account.
	fakeHubJSON(w, http.StatusOK, map[string]any{
		"aid": r.PathValue("aid"), "credits": h.balance[r.PathValue("aid")]})
}

func (h *fakeHub) hLedgerRead(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	es := h.entries[r.PathValue("aid")]
	if es == nil {
		es = []map[string]any{}
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"entries": es})
}

func (h *fakeHub) hAgentKEL(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	a := h.agents[r.PathValue("aid")]
	h.mu.Unlock()
	if a == nil {
		fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "unknown agent"})
		return
	}
	fakeHubJSON(w, http.StatusOK, map[string]string{
		"aid": r.PathValue("aid"), "kel": base64.StdEncoding.EncodeToString(a.kel)})
}

// grantOn credits an account on the fake hub behind url.
func grantOn(url, aid string, amount uint64) {
	if v, ok := hubsByURL.Load(url); ok {
		v.(*fakeHub).grant(aid, amount)
	}
}

func hubAIDOf(url string) string {
	v, ok := hubsByURL.Load(url)
	if !ok {
		return ""
	}
	return v.(*fakeHub).self.AID()
}

func hubKELOf(t *testing.T, url string) []identity.SignedEvent {
	t.Helper()
	v, ok := hubsByURL.Load(url)
	if !ok {
		t.Fatal("no fake hub at " + url)
	}
	return v.(*fakeHub).self.KEL()
}
