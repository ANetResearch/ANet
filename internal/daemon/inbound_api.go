package daemon

// inbound_api.go holds the control-plane handlers of the inbound policy and
// the peer lists (A2A-DESIGN §5). All of them are bearer-only routes.
//
// A peer list edit withdraws the delivery credentials it takes away from
// the peer (sealed-sender §4.3.8 item 1) after the list file is written and
// before the route answers: the operator who removed or denied a peer is
// told once the change is in force. A stop between the two is made good at
// the next start (reconcileCapsWithLists).
//
// The routes that grant (peers/allow, peers/trust, inbound/approve) do not
// themselves require a TTY: the daemon cannot tell how its caller obtained
// the control token. The CLI commands that call them read a confirmation
// from /dev/tty and refuse without one; that boundary is stated in §21
// item 13.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// errAcceptOn is the answer to the wire-1 "accept on" switch.
var errAcceptOn = errors.New("`accept on` was removed: it let anyone delegate to this node. " +
	"Inbound delegations are now governed by inbound.policy: closed (the default: only peers on the " +
	"allow list), approve (anyone else is held for your approval), open (anyone may send a " +
	"natural-language task; capability calls still need inbound.public_capabilities). " +
	"To accept one peer, run `anet peers allow <aid>`; to change the policy, `anet inbound policy <closed|approve|open>`")

type aidRequest struct {
	AID string `json:"aid"`
}

func readAID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req aidRequest
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.AID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "aid required"})
		return "", false
	}
	return strings.TrimSpace(req.AID), true
}

func (d *Daemon) hPeersAllow(w http.ResponseWriter, r *http.Request) { d.peersGrant(w, r, ListAllow) }
func (d *Daemon) hPeersTrust(w http.ResponseWriter, r *http.Request) { d.peersGrant(w, r, ListTrust) }

func (d *Daemon) peersGrant(w http.ResponseWriter, r *http.Request, list string) {
	aid, ok := readAID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	res, err := d.AllowPeer(ctx, aid, list)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := d.withdrawAfterListEdit(aid, listEditAllow); err != nil {
		writeLocalWriteFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (d *Daemon) hPeersDeny(w http.ResponseWriter, r *http.Request) {
	aid, ok := readAID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), hubCallTimeout)
	defer cancel()
	res, err := d.DenyPeer(ctx, aid)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := d.withdrawAfterListEdit(aid, listEditDeny); err != nil {
		writeLocalWriteFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (d *Daemon) hPeersRemove(w http.ResponseWriter, r *http.Request) {
	aid, ok := readAID(w, r)
	if !ok {
		return
	}
	res, err := d.RemovePeer(aid)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := d.withdrawAfterListEdit(aid, listEditRemove); err != nil {
		writeLocalWriteFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// hInboundPolicy reports the inbound state, and sets the policy, the
// public capabilities and inbound.admit when the request names them
// ("admit": null removes inbound.admit). A write that conflicts with the
// auto-reply or backend configuration is refused with 409.
func (d *Daemon) hInboundPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy             string              `json:"policy"`
		PublicCapabilities *[]PublicCapability `json:"public_capabilities"`
		Admit              json.RawMessage     `json:"admit"`
	}
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if req.Policy != "" {
		if err := d.SetInboundPolicy(strings.TrimSpace(req.Policy)); err != nil {
			writePolicyError(w, err)
			return
		}
	}
	if req.PublicCapabilities != nil {
		if err := d.SetPublicCapabilities(*req.PublicCapabilities); err != nil {
			writePolicyError(w, err)
			return
		}
	}
	if len(req.Admit) > 0 {
		admit, ref := readInboundAdmit(req.Admit)
		if ref != nil {
			writeRefusal(w, http.StatusConflict, ref)
			return
		}
		if err := d.SetInboundAdmit(admit); err != nil {
			writePolicyError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, d.InboundStatus())
}

// readInboundAdmit reads the admit member of a write: null for none, else an
// object of exactly capability and cache_seconds.
func readInboundAdmit(raw json.RawMessage) (*InboundAdmit, *a2ashape.Refusal) {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var a InboundAdmit
	if err := dec.Decode(&a); err != nil {
		return nil, configInvalid("inbound.admit", ruleText(
			"须是 null，或只含 capability 与 cache_seconds 的对象",
			"it is null, or an object with capability and cache_seconds only"))
	}
	return &a, nil
}

// writePolicyError answers a refused inbound write: a value the daemon
// would not start with is config_invalid, 409, in the reason table's
// words (CUX §2.10); a conflict with the backends 409; anything else 400.
func writePolicyError(w http.ResponseWriter, err error) {
	var ref *a2ashape.Refusal
	if errors.As(err, &ref) {
		writeRefusal(w, http.StatusConflict, ref)
		return
	}
	writeJSON(w, policyErrorCode(err), map[string]string{"error": err.Error()})
}

func policyErrorCode(err error) int {
	if errors.Is(err, ErrPolicyConflict) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// hInboundPending lists the held delegations, metadata only (A2A-DESIGN
// §5.3). This is also the data the MCP inbound_pending tool returns.
func (d *Daemon) hInboundPending(w http.ResponseWriter, _ *http.Request) {
	items, err := d.PendingList()
	if err != nil {
		writeLocalWriteFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": items})
}

type ixRequest struct {
	InteractionID string `json:"interaction_id"`
}

func (d *Daemon) hInboundApprove(w http.ResponseWriter, r *http.Request) {
	var req ixRequest
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.InteractionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id required"})
		return
	}
	ix, err := d.ApprovePending(strings.TrimSpace(req.InteractionID))
	if errors.Is(err, ErrNotPending) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	var ref *a2ashape.Refusal
	if errors.As(err, &ref) {
		// key_state.approve (KEL D-25): the reason table's sentence.
		writeRefusal(w, http.StatusConflict, ref)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": ix.ID, "state": ix.State, "trust": ix.Trust,
		"requester": ix.PeerAID, "status": "approved"})
}

func (d *Daemon) hInboundReject(w http.ResponseWriter, r *http.Request) {
	var req ixRequest
	if err := readJSON(r, &req); err != nil || strings.TrimSpace(req.InteractionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "interaction_id required"})
		return
	}
	if err := d.RejectPending(strings.TrimSpace(req.InteractionID)); err != nil {
		if !errors.Is(err, ErrNotPending) {
			writeLocalWriteFailed(w, err)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": req.InteractionID, "status": "rejected"})
}
