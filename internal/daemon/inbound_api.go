package daemon

// inbound_api.go holds the control-plane handlers of the inbound policy and
// the peer lists (A2A-DESIGN §5). All of them are bearer-only routes.
//
// The routes that grant (peers/allow, peers/trust, inbound/approve) do not
// themselves require a TTY: the daemon cannot tell how its caller obtained
// the control token. The CLI commands that call them read a confirmation
// from /dev/tty and refuse without one; that boundary is stated in §21
// item 13.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
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

// hPeersList reports the inbound policy and the three peer lists.
func (d *Daemon) hPeersList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.InboundStatus())
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
	writeJSON(w, http.StatusOK, res)
}

// hInboundPolicy reports the inbound state, and sets the policy and the
// public capabilities when the request names them. A write that conflicts
// with the auto-reply or backend configuration is refused with 409.
func (d *Daemon) hInboundPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy             string              `json:"policy"`
		PublicCapabilities *[]PublicCapability `json:"public_capabilities"`
	}
	if err := readJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if req.Policy != "" {
		if err := d.SetInboundPolicy(strings.TrimSpace(req.Policy)); err != nil {
			writeJSON(w, policyErrorCode(err), map[string]string{"error": err.Error()})
			return
		}
	}
	if req.PublicCapabilities != nil {
		if err := d.SetPublicCapabilities(*req.PublicCapabilities); err != nil {
			writeJSON(w, policyErrorCode(err), map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, d.InboundStatus())
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
		code := http.StatusInternalServerError
		if errors.Is(err, ErrNotPending) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interaction_id": req.InteractionID, "status": "rejected"})
}
