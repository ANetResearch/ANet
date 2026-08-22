//go:build !no_x402

package x402

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/provider"
)

// Redeeming a voucher: the work half of "pay there, work here".
//
// A hub can host an x402 resource server on this node's behalf — take the
// 402, settle the credit — and hand the buyer a signed voucher instead of
// the result. The buyer brings the voucher here. The hub therefore never
// sees the request or the result, which is the same property the relay
// has and worth keeping for the same reason: it cannot leak what it never
// held, and cannot be compelled to produce it.
//
// The cost is stated rather than discovered: this face is public, so the
// buyer must be able to reach it. A node behind a NAT with no ingress
// cannot offer this, and should not configure it — the ordinary delegate
// path through the relay works there and this does not.
//
// It lives in the kernel next to pay.go rather than behind a module tag,
// because it is a second surface onto a kernel function rather than a
// subsystem. Putting the objects, the settlement and the quote in the
// kernel and the redemption behind a build tag would be half a feature
// each side of a seam.

// EvCapabilityEffect is the kernel's event type for a capability that
// ran. Named here with the same string on purpose: work bought through
// this door is as accountable as work delegated through the relay, and a
// separate event type would make it look like a separate kind of work.
const EvCapabilityEffect = "anet.capability.effect"

// EvVoucherRedeemed records a voucher this node honoured.
//
// It is also how the spent set survives a restart. An in-memory set alone
// would forget every voucher when the process did, and a buyer who kept
// one could then have the same work done twice for one payment. The chain
// is read back at startup, which is what the evidence read surface is
// for — a record nobody can consult is not a control.
const EvVoucherRedeemed = "anet.voucher.redeemed"

// EvVoucherRefused records one that was not honoured, and why.
//
// A refusal is evidence too. Without it, a buyer who paid and was turned
// away has this node's silence, and the operator has no way to find out
// it is refusing everything because its hub AID went stale.
const EvVoucherRefused = "anet.voucher.refused"

// redeemRequest is what a buyer presents.
type redeemRequest struct {
	// Voucher is base64 CoreDet-CBOR — the hub's signed statement that
	// this work has been paid for.
	Voucher string `json:"voucher"`
	// Capability and Args are the work. The capability must match what
	// the voucher bought; the args are free unless the voucher pinned
	// them, which a buyer who wants that guarantee asks for at purchase.
	Capability string         `json:"capability"`
	Args       map[string]any `json:"args,omitempty"`
}

// spentVouchers is the one-time guard.
//
// The hub cannot enforce it — it has no way to know whether a voucher was
// used, because it is not the party doing the work. So the provider is,
// and this is that. Loaded from the evidence chain on first use so it
// survives a restart.
type spentVouchers struct {
	mu     sync.Mutex
	loaded bool
	ids    map[string]bool
}

func newSpentVouchers() *spentVouchers { return &spentVouchers{ids: map[string]bool{}} }

// claim marks a voucher spent and reports whether this caller got it.
// False means somebody already redeemed it.
func (s *spentVouchers) claim(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[id] {
		return false
	}
	s.ids[id] = true
	return true
}

// release undoes a claim, for the case where the work could not even be
// attempted. A voucher held against work that never started is one the
// buyer paid for and did not get.
func (s *spentVouchers) release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ids, id)
}

// load reads previously redeemed voucher ids off this node's own chain.
func (s *spentVouchers) load(m *Module) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded || m.seam == nil {
		return
	}
	s.loaded = true
	recs := m.seam.ReadEvidence(EvVoucherRedeemed, maxSpentReplay)
	for _, r := range recs {
		if id, ok := r["voucher_id"].(string); ok && id != "" {
			s.ids[id] = true
		}
	}
	if len(recs) == maxSpentReplay {
		// Saying so rather than pretending the set is complete. Past this
		// depth an old voucher could be replayed — though only one that
		// is also still inside its window, which is minutes.
		log.Printf("anet: voucher spent-set truncated at %d entries", maxSpentReplay)
	}
}

// maxSpentReplay bounds the startup scan. Vouchers expire in minutes, so
// a set this deep covers far more history than any live voucher can span.
const maxSpentReplay = 10000

// redeemHandler serves the public voucher face.
func (m *Module) redeemHandler() http.Handler {
	mux := http.NewServeMux()
	// A GET says what this is, for whoever finds the port. Being findable
	// and unexplained is its own small hazard.
	mux.HandleFunc("GET /x402/redeem", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"aid":     m.AID(),
			"accepts": payment.SchemeCredit,
			"network": payment.CreditNetwork(m.hubAID()),
			"how": "POST a hub-signed voucher here with the capability it bought. " +
				"Buy one from this node's hub; the hub takes the payment and never sees the work.",
		})
	})
	mux.HandleFunc("POST /x402/redeem", m.hRedeem)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// hRedeem verifies a voucher and does the work it paid for.
func (m *Module) hRedeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), redeemTimeout)
	defer cancel()
	res, code := m.RedeemVoucher(ctx, req)
	writeJSON(w, code, res)
}

// redeemTimeout bounds one redemption. Generous: the buyer has already
// paid, and cutting the work short to keep a socket tidy would be
// charging for a timeout.
const redeemTimeout = 10 * time.Minute

// RedeemVoucher is the whole check-and-do, separated from HTTP so it is testable
// without a listener and callable from anywhere else that grows a need.
func (m *Module) RedeemVoucher(ctx context.Context, req redeemRequest) (map[string]any, int) {
	refuse := func(code int, reason string, detail map[string]any) (map[string]any, int) {
		if m.host != nil {
			payload := map[string]any{"reason": reason, "capability": req.Capability}
			for k, v := range detail {
				payload[k] = v
			}
			if err := m.record(EvVoucherRefused, payload); err != nil {
				log.Printf("anet: voucher refusal evidence: %v", err)
			}
		}
		return map[string]any{"status": string(effect.PaymentRequired), "error": reason}, code
	}
	if req.Capability == "" {
		return refuse(http.StatusBadRequest, "no capability named", nil)
	}
	raw, err := base64.StdEncoding.DecodeString(req.Voucher)
	if err != nil || len(raw) == 0 {
		return refuse(http.StatusPaymentRequired, "voucher missing or not base64", nil)
	}
	v, err := payment.UnmarshalVoucher(raw)
	if err != nil {
		return refuse(http.StatusPaymentRequired, "voucher malformed: "+err.Error(), nil)
	}

	// Who signed it has to be our hub, checked against our hub's key
	// history rather than against the AID the voucher names itself. An
	// object that names its own signer proves only that somebody signed.
	hubAID := m.hubAID()
	if hubAID == "" {
		return refuse(http.StatusServiceUnavailable,
			"this node has no hub, so it cannot tell whose signature to trust", nil)
	}
	kel, err := m.hubKEL()
	if err != nil {
		return refuse(http.StatusServiceUnavailable, "cannot read the hub's key history: "+err.Error(), nil)
	}
	if err := v.Verify(kel, hubAID, m.AID(), req.Capability,
		payment.CreditNetwork(hubAID), time.Now().UnixMilli()); err != nil {
		return refuse(http.StatusPaymentRequired, err.Error(), map[string]any{"payer": v.Payer})
	}
	if v.ArgsCID != "" {
		got, err := argsCID(req.Args)
		if err != nil {
			return refuse(http.StatusBadRequest, "arguments not encodable: "+err.Error(), nil)
		}
		if got != v.ArgsCID {
			return refuse(http.StatusPaymentRequired,
				"this voucher pinned different arguments", map[string]any{"payer": v.Payer})
		}
	}

	id, err := v.ID()
	if err != nil {
		return refuse(http.StatusInternalServerError, err.Error(), nil)
	}
	m.spent.load(m)
	if !m.spent.claim(id) {
		return refuse(http.StatusConflict, "this voucher has already been redeemed",
			map[string]any{"voucher_id": id, "payer": v.Payer})
	}

	if m.host.Providers() == nil {
		m.spent.release(id)
		return refuse(http.StatusServiceUnavailable, "this node serves no capabilities", nil)
	}
	p, ok := m.host.Providers().Resolve(req.Capability)
	if !ok {
		// Paid for, and we cannot do it. Release the claim: the buyer
		// should be able to take this to the hub, and a voucher we
		// consumed without working is one they cannot show is unspent.
		m.spent.release(id)
		return refuse(http.StatusNotFound,
			"this node does not serve "+req.Capability, map[string]any{"payer": v.Payer})
	}

	// Spent BEFORE the work, on the chain, so a crash mid-work cannot
	// leave a redeemed voucher looking unredeemed. The buyer whose work
	// was interrupted has a real complaint and the evidence to make it;
	// the alternative lets a crash be farmed for free work.
	if lerr := m.record(EvVoucherRedeemed, map[string]any{
		"voucher_id": id, "auth_id": v.AuthID, "payer": v.Payer,
		"capability": v.Capability, "amount": v.Amount, "network": v.Network,
	}); lerr != nil {
		m.spent.release(id)
		log.Printf("anet: voucher evidence: %v", lerr)
		return refuse(http.StatusInternalServerError, "could not record the redemption", nil)
	}

	eff, err := p.Invoke(ctx, provider.Call{
		Capability: req.Capability, Args: req.Args, CallID: id, CallerAID: v.Payer})
	out := map[string]any{"capability": req.Capability, "voucher_id": id, "payer": v.Payer}
	if err != nil {
		out["status"], out["message"] = string(effect.Failed), err.Error()
	} else {
		out["status"] = string(eff.Status)
		out["verifiable"] = eff.Verifiable()
		out["message"] = eff.Message
		out["evidence"] = provider.Provenance(eff.Evidence)
		if eff.Record != nil {
			out["metrics"] = eff.Record.Metrics
		}
	}
	// The effect goes on the chain like any other, so paid-by-voucher work
	// is as accountable as work delegated through the relay. Different
	// door, same evidence.
	if lerr := m.record(EvCapabilityEffect, map[string]any{
		"interaction_id": id, "capability": req.Capability,
		"status": out["status"], "via": "voucher",
	}); lerr != nil {
		log.Printf("anet: voucher effect evidence: %v", lerr)
	}
	return out, http.StatusOK
}

// startRedeemFace brings up the public voucher listener, if configured.
func (m *Module) startRedeemFace(ctx context.Context) error {
	addr := m.cfg.VoucherAddr
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("anet: voucher face on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: m.redeemHandler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("anet: voucher redemption face on %s (public: buyers reach it directly, the hub does not)", ln.Addr())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("anet: voucher face: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	return nil
}

// argsCID names one exact call, for a voucher that pinned its arguments.
func argsCID(args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return anetcid.Sum(b)
}
