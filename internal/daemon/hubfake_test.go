package daemon

// hubfake_test.go is a minimal in-memory stand-in for the Hub service (which is a separate
// closed-source service; only its wire types live in internal/hubapi). The daemon tests run against
// this fake instead of a real Hub. It implements exactly the endpoints the daemon's HTTP client
// calls, with wire shapes identical to the real Hub, but keeps everything in maps under one mutex
// and SKIPS all signature verification (the daemon still signs its challenges; the fake just
// doesn't check them — signature verification is covered by the protocol packages' own tests).

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/evidence"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// fakeHubAgent is one registered agent's row in the fake registry.
type fakeHubAgent struct {
	view hubapi.AgentView
	kel  []byte
}

// fakeHubMsg is one queued relay message (the fake's relay_message row).
type fakeHubMsg struct {
	id            int64
	toAID         string
	fromAID       string
	kind          string
	interactionID string
	payload       []byte
	createdAt     string
	delivered     bool
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
	// relaySends counts deliveries the hub actually carried, so a test can
	// tell "the hub delivered it" from "something else did".
	relaySends int
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
		self:    self,
		balance: map[string]uint64{}, entries: map[string][]map[string]any{},
		settled: map[string]string{},
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
	mux.HandleFunc("POST /profile", h.hProfile)
	mux.HandleFunc("GET /agents", h.hAgents)
	mux.HandleFunc("GET /agents/{aid}", h.hAgent)
	mux.HandleFunc("POST /reviews", h.hUploadReview)
	mux.HandleFunc("POST /relay/send", h.hRelaySend)
	mux.HandleFunc("POST /relay/poll", h.hRelayPoll)
	mux.HandleFunc("POST /relay/ack", h.hRelayAck)
	// The facilitator half of the contract. A fake that serves only the
	// endpoints the happy path happens to touch is how a seam gets shipped
	// unconnected — the real hub answers these, so this one does too.
	mux.HandleFunc("GET /hub/identity", h.hIdentity)
	mux.HandleFunc("GET /agents/{aid}/kel", h.hAgentKEL)
	mux.HandleFunc("POST /x402/settle", h.hSettle)
	mux.HandleFunc("GET /agents/{aid}/balance", h.hBalance)
	mux.HandleFunc("GET /agents/{aid}/ledger", h.hLedgerRead)
	return mux
}

func fakeHubJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// hRegister mirrors the Hub's POST /register: upsert the agent row (name/caps/quota/KEL). The real
// Hub derives the AID from the KEL and verifies a signed challenge; the fake trusts the caller.
func (h *fakeHub) hRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AID           string   `json:"aid"`
		Name          string   `json:"name"`
		Caps          []string `json:"caps"`
		Summary       string   `json:"summary"`
		Readme        string   `json:"readme"`
		Pricing       string   `json:"pricing"`
		GuestMessages *int     `json:"guest_messages"`
		KEL           string   `json:"kel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AID == "" || req.KEL == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "aid + kel required"})
		return
	}
	kelBytes, err := base64.StdEncoding.DecodeString(req.KEL)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "kel not base64"})
		return
	}
	quota := 5 // the real Hub's guestDefaultQuota
	if req.GuestMessages != nil {
		quota = *req.GuestMessages
	}
	if quota < 0 {
		quota = 0
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
	a.view.GuestQuota = quota
	a.kel = kelBytes
	// Optional profile carried on the registration (usually set later via /profile).
	if req.Summary != "" || req.Readme != "" || req.Pricing != "" {
		a.view.Summary, a.view.Readme, a.view.Pricing = req.Summary, req.Readme, req.Pricing
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"aid": req.AID, "status": "registered"})
}

// hProfile mirrors POST /profile (signature challenge skipped).
func (h *fakeHub) hProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AID     string `json:"aid"`
		Summary string `json:"summary"`
		Readme  string `json:"readme"`
		Pricing string `json:"pricing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AID == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "aid required"})
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

// fakeHubGoal re-derives the displayable goal from the request TaskDoc bytes (same as the real Hub).
func fakeHubGoal(docBytes []byte) string {
	var td tsir.TaskDoc
	if err := coredet.Unmarshal(docBytes, &td); err != nil || len(td.Tasks) == 0 {
		return ""
	}
	if b := td.Tasks[0].Intent.Body; b != "" {
		return b
	}
	return td.Tasks[0].Intent.Summary
}

// hUploadReview mirrors POST /reviews. It keeps the cheap structural checks (interlock + rating range
// + one-review-per-interaction) so the stored view carries the same verified content the real Hub
// stores, but skips the two signature verifications and the CID re-hashing.
func (h *fakeHub) hUploadReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Receipt     string `json:"receipt"`
		Review      string `json:"review"`
		RequestDoc  string `json:"request_doc"`
		Deliverable string `json:"deliverable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Receipt == "" || req.Review == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt + review required"})
		return
	}
	if req.RequestDoc == "" || req.Deliverable == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "request_doc + deliverable required"})
		return
	}
	rcBytes, err1 := base64.StdEncoding.DecodeString(req.Receipt)
	rvBytes, err2 := base64.StdEncoding.DecodeString(req.Review)
	docBytes, err3 := base64.StdEncoding.DecodeString(req.RequestDoc)
	delivBytes, err4 := base64.StdEncoding.DecodeString(req.Deliverable)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "receipt/review/content not base64"})
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
		InteractionID: rv.InteractionID,
		SubjectAID:    rv.SubjectAID,
		ReviewerAID:   rv.ReviewerAID,
		Rating:        rv.Rating,
		Comment:       rv.Comment,
		ReceiptCID:    rv.ReceiptCID,
		Goal:          fakeHubGoal(docBytes),
		Deliverable:   string(delivBytes),
		RequestCID:    rc.RequestCID,
		ResultCID:     rc.ResultCID,
		CompletedAt:   rc.CompletedAt,
		CreatedAt:     rv.CreatedAt,
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"interaction_id": rv.InteractionID, "status": "accepted"})
}

// hRelaySend mirrors POST /relay/send — enqueue an opaque payload for a registered recipient.
func (h *fakeHub) hRelaySend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ToAID         string `json:"to_aid"`
		FromAID       string `json:"from_aid"`
		Kind          string `json:"kind"`
		InteractionID string `json:"interaction_id"`
		Payload       string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ToAID == "" || req.Payload == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "to_aid + payload required"})
		return
	}
	if req.Kind != hubapi.RelayKindDelegate && req.Kind != hubapi.RelayKindResult && req.Kind != hubapi.RelayKindMessage {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be delegate|result|message"})
		return
	}
	payload, err := base64.StdEncoding.DecodeString(req.Payload)
	if err != nil {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "payload not base64"})
		return
	}
	h.mu.Lock()
	h.relaySends++
	defer h.mu.Unlock()
	if _, ok := h.agents[req.ToAID]; !ok {
		fakeHubJSON(w, http.StatusNotFound, map[string]string{"error": "recipient not registered"})
		return
	}
	h.nextID++
	h.mailbox = append(h.mailbox, &fakeHubMsg{
		id: h.nextID, toAID: req.ToAID, fromAID: req.FromAID, kind: req.Kind,
		interactionID: req.InteractionID, payload: payload,
		createdAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	fakeHubJSON(w, http.StatusOK, map[string]any{"id": h.nextID, "status": "queued"})
}

// fakeHubMsgView is the wire shape of a mailbox message (payload base64) — identical to the Hub's.
type fakeHubMsgView struct {
	ID            int64  `json:"id"`
	FromAID       string `json:"from_aid"`
	Kind          string `json:"kind"`
	InteractionID string `json:"interaction_id"`
	Payload       string `json:"payload"`
	CreatedAt     string `json:"created_at"`
}

// hRelayPoll mirrors POST /relay/poll — undelivered messages for aid, oldest first (auth skipped).
func (h *fakeHub) hRelayPoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AID   string `json:"aid"`
		Limit int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AID == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []fakeHubMsgView{}
	for _, m := range h.mailbox {
		if m.toAID != req.AID || m.delivered {
			continue
		}
		out = append(out, fakeHubMsgView{
			ID: m.id, FromAID: m.fromAID, Kind: m.kind, InteractionID: m.interactionID,
			Payload: base64.StdEncoding.EncodeToString(m.payload), CreatedAt: m.createdAt,
		})
		if len(out) >= limit {
			break
		}
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"messages": out})
}

// hRelayAck mirrors POST /relay/ack — mark ids delivered, scoped to the caller's own mailbox.
func (h *fakeHub) hRelayAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AID string  `json:"aid"`
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AID == "" {
		fakeHubJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	acked := 0
	for _, id := range req.IDs {
		for _, m := range h.mailbox {
			if m.id == id && m.toAID == req.AID && !m.delivered {
				m.delivered = true
				acked++
			}
		}
	}
	fakeHubJSON(w, http.StatusOK, map[string]any{"acked": acked})
}

// onlyRelayPayload returns the single queued relay payload of a kind for a
// recipient — the bytes the hub is holding, so a test can redeliver
// exactly what a real second poll would deliver.
func onlyRelayPayload(t *testing.T, srv *httptest.Server, toAID, kind string) []byte {
	t.Helper()
	v, ok := hubsByURL.Load(srv.URL)
	if !ok {
		t.Fatal("no fake hub for this server")
	}
	h := v.(*fakeHub)
	h.mu.Lock()
	defer h.mu.Unlock()
	var found []byte
	for _, m := range h.mailbox {
		if m.toAID == toAID && m.kind == kind {
			if found != nil {
				t.Fatalf("more than one %s queued for %s", kind, toAID)
			}
			found = m.payload
		}
	}
	if found == nil {
		t.Fatalf("no %s queued for %s", kind, toAID)
	}
	return found
}

// hIdentity is how a node learns which hub's ledger its credits live on.
// The AID goes into every authorization's network field, so a payment
// signed for one hub cannot be replayed at another.
func (h *fakeHub) hIdentity(w http.ResponseWriter, _ *http.Request) {
	fakeHubJSON(w, http.StatusOK, map[string]string{"aid": h.self.AID()})
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
