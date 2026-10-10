package daemon

// oneshot.go is the one-shot sender (engine §2.12): the only source of outer
// key 8. A one-shot envelope — a stream_preview frame, an anet.card.get/1 or
// an anet.card/1 (core G11) — is sealed once, never queued, never sealed
// again and sent again at most once (ENG-O8a); its receiver makes no
// delivery token for it, so its ack_key is not kept (ENG-O10).
//
// What each kind is (ENG-O9):
//
//	kind          outer exp      layers               inner 10/11/12  inner 14            credential
//	preview       ts + 60 s      two, with this rk    none            none                the task's one-shot or the peer's
//	card.get      ts + 10 min    one                  all three       a one-shot reply    the paid path (kind 0 when priced)
//	card          ts + 10 min    one                  all three       none                only the request's one-shot
//
// A frame, a request or an answer that cannot go — the gate is closed, no
// credential, no key, no rk for a preview — is not sent, and counted; the
// caller learns why from OneShotResult. A hub's refusal is not met with a
// new sealing, kid_stale included (ENG-O8, FS-O5).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"
	"github.com/ANetResearch/ANetCore/seal"
)

// oneShotKind is what a one-shot envelope carries (engine §2.12): outer key
// 8, no outbox row, no retry, no delivery token.
type oneShotKind uint8

const (
	oneShotPreview oneShotKind = iota + 1 // a stream_preview frame
	oneShotCardGet                        // anet.card.get/1
	oneShotCard                           // anet.card/1
)

// OneShotResult is how a one-shot send ended (ENG-O8b): whether a byte of
// it left this process, and the reason code of what stopped it or of the
// hub's last answer ("" for a 200, or no answer at all).
//
// HubReason is the hub's reason code when the hub answered (strangers_busy,
// kid_stale, …); when nothing reached the hub it names the wait of ux C2
// that held the envelope back, as anet.delivery_wait names a queued
// message's: the gate's notice_stale or rotation_pending, or
// session_unavailable when this node's hub took no first contact from it.
// PAY-11 picks the {why} of card_price_unavailable from it.
type OneShotResult struct {
	Handed    bool
	HubReason string
}

// The one-shot envelopes' lives and the card's wait (engine §0.4). Code
// constants: there is nothing for an operator to tune.
const (
	// previewLifeMS is exp − ts of a preview frame (CUX-P7, ENG-K18): a
	// frame the hub has held this long is dropped there; the next one
	// replaces it anyway.
	previewLifeMS uint64 = 60 * 1000
	// cardLifeMS is exp − ts of both card messages (HPP §4.3.2).
	cardLifeMS uint64 = 10 * 60 * 1000
	// cardWaitMS is how long a requester waits for a price card (PAY-C12);
	// the one resend of ENG-O8a fits inside it or is not made.
	cardWaitMS uint64 = 10 * 1000
)

// Counters of the one-shot sender (engine ENG-O8, ENG-K19).
const (
	// cntOneShotDroppedPrefix and the reason: a one-shot envelope that did
	// not reach its recipient or its hub, or that the hub refused.
	cntOneShotDroppedPrefix = "oneshot-dropped-"
	// cntPreviewNoRK: a preview frame not sent because the task has no rk of
	// the peer's, or none of this node's, to seal two layers with.
	cntPreviewNoRK = "preview-no-rk"
	// cntCardGetResent: a card request sent again once after a 429 (ENG-O8a).
	cntCardGetResent = "oneshot-card-get-resent"
)

// The local reasons a one-shot envelope did not go, beside the hub's codes.
const (
	oneShotWhyGate    = "gate"
	oneShotWhyBody    = "body"
	oneShotWhyAccess  = "no_credential"
	oneShotWhyKeys    = "no_keys"
	oneShotWhyGrant   = "no_grant"
	oneShotWhySeal    = "seal"
	oneShotWhyNoHub   = "no_hub"
	oneShotWhyNetwork = "network"
	oneShotWhyDirect  = "direct"
	// oneShotWhyNoRK is a preview frame without the rks of two layers; it
	// is counted as cntPreviewNoRK alone.
	oneShotWhyNoRK = "no_rk"
)

// oneShotReq is one one-shot send as its options leave it.
type oneShotReq struct {
	// keys is the recipient's key set to seal to; nil resolves it as a
	// queued message's is (sealKeys).
	keys *seal.EncKeySet
}

// oneShotOption sets a part of a one-shot send.
type oneShotOption func(*oneShotReq)

// oneShotSealTo seals the envelope to keys, a key set of the recipient's the
// caller holds verified: a card answer goes to the key set its request
// carried (core G7), which this node need not have stored.
func oneShotSealTo(keys *seal.EncKeySet) oneShotOption {
	return func(r *oneShotReq) { r.keys = keys }
}

// oneShotSpec is what a kind is sealed and sent as (ENG-O9).
type oneShotSpec struct {
	typ  string
	life uint64
	// bare: no KEL, notice or key set (a preview frame).
	bare bool
	// twoLayers: sealed to the peer's rk with this node's rk inside
	// (FS-X6); not sent without both.
	twoLayers bool
	mode      accessMode
}

// oneShotSpecOf is kind's spec; ok false for a kind that is not one.
func oneShotSpecOf(kind oneShotKind) (oneShotSpec, bool) {
	switch kind {
	case oneShotPreview:
		return oneShotSpec{typ: seal.TypeMessage, life: previewLifeMS, bare: true, twoLayers: true, mode: accessPreview}, true
	case oneShotCardGet:
		return oneShotSpec{typ: delegation.TypeCardGet, life: cardLifeMS, mode: accessCardGet}, true
	case oneShotCard:
		return oneShotSpec{typ: delegation.TypeCard, life: cardLifeMS, mode: accessCard}, true
	}
	return oneShotSpec{}, false
}

// checkOneShotBody refuses a body that is not one of kind's: the one-shot
// sender carries exactly the three envelopes of core G11, and a receiver
// would drop anything else (CUX-P2, HPP §2.6).
func checkOneShotBody(kind oneShotKind, body []byte) error {
	switch kind {
	case oneShotPreview:
		m, err := delegation.UnmarshalChatMsg(body)
		if err != nil {
			return err
		}
		return delegation.ValidatePreview(m)
	case oneShotCardGet:
		_, err := delegation.UnmarshalCardGet(body)
		return err
	case oneShotCard:
		_, err := delegation.UnmarshalCardResp(body)
		return err
	}
	return fmt.Errorf("anet: one-shot kind %d is not one", kind)
}

// oneShotSend seals body once as kind, to `to` on interaction ix, and hands
// it over once (ENG-O8): a direct path first when one reaches `to` and is not
// backing off, the hub otherwise or after it failed. Nothing goes while the
// outbound gate is closed (ENG-K13, K21). An anet.card.get/1 the hub turns
// away as busy is sent again once, the same bytes, when the hub's wait fits
// in the card's (ENG-O8a). The result says whether a byte left this process,
// and why not or what the hub answered (ENG-O8b).
//
// ix is the task for a preview frame and the card-<32 hex> exchange for the
// cards. ctx bounds the whole send: a caller's deadline shortens the card's
// wait.
func (d *Daemon) oneShotSend(ctx context.Context, kind oneShotKind, to, ix string, body []byte, opts ...oneShotOption) OneShotResult {
	var req oneShotReq
	for _, o := range opts {
		o(&req)
	}
	start := d.nowMS()
	if g := d.identityGate(); !g.Open {
		return d.oneShotDropped(OneShotResult{HubReason: gateDeliveryWait(g)}, oneShotWhyGate)
	}
	wire, choice, why := d.sealOneShot(ctx, kind, to, ix, body, req)
	switch why {
	case "":
		return d.handOneShot(ctx, kind, to, wire, choice, start)
	case oneShotWhyNoRK:
		return OneShotResult{} // counted as preview-no-rk
	case oneShotWhyGate:
		// The gate closed while the envelope was being made.
		return d.oneShotDropped(OneShotResult{HubReason: gateDeliveryWait(d.identityGate())}, why)
	}
	return d.oneShotDropped(OneShotResult{}, why)
}

// sealOneShot is the sealing of oneShotSend: the envelope with its outer key
// 7, and the credential it goes under; why is not "" when there is none to
// send (the reason it is counted under). The ack_key is wiped at once
// (ENG-O10).
func (d *Daemon) sealOneShot(ctx context.Context, kind oneShotKind, to, ix string, body []byte, req oneShotReq) ([]byte, AccessChoice, string) {
	var none AccessChoice
	spec, ok := oneShotSpecOf(kind)
	if !ok || checkOneShotBody(kind, body) != nil {
		log.Printf("anet: a one-shot envelope to %s that is not one of the three (kind %d) was not sent", to, kind)
		return nil, none, oneShotWhyBody
	}
	now := d.nowMS()
	var ixk sealIXKeys
	if spec.twoLayers {
		var err error
		if ixk, err = d.ixKeysForSeal(ix); err != nil {
			log.Printf("anet: %s: a preview frame to %s: %v", ix, to, err)
			return nil, none, oneShotWhySeal
		}
		if ixk.peer == nil || ixk.own == nil {
			// Two layers and this node's rk, or no frame (FS-X6, ENG-K19).
			d.count(cntPreviewNoRK)
			return nil, none, oneShotWhyNoRK
		}
	}
	// The credential first: without one nothing is sealed (ENG-K2) — a card
	// answer without the request's one-shot is not answered.
	choice, err := d.accessChoiceFor(to, ix, false, spec.mode, now)
	if err != nil || !choice.Send {
		return nil, none, oneShotWhyAccess
	}
	set := req.keys
	if set == nil {
		if set, err = d.sealKeys(ctx, to, ix, false); err != nil {
			if errors.Is(err, errGateClosed) {
				return nil, none, oneShotWhyGate
			}
			return nil, none, oneShotWhyKeys
		}
	}
	var grant []byte
	if kind == oneShotCardGet {
		// The reply capability exists before any byte that carries it
		// leaves (SSR-D10); no capability, no request: its answer could not
		// come back.
		if grant, err = d.cardGetGrant(to, ix, now); err != nil {
			log.Printf("anet: %s: a price card request to %s: %v", ix, to, err)
			return nil, none, oneShotWhyGrant
		}
	}
	exp := now + spec.life
	sv, err := d.sealMessage(sealMsg{to: to, typ: spec.typ, ix: ix, body: body, mid: seal.NewMID(), ts: now, exp: exp,
		grant: grant, rk: ixk.own, bare: spec.bare}, set, seal.SealOpts{PeerRK: ixk.peer, Exp: exp}, now)
	if err != nil {
		switch {
		case errors.Is(err, errGateClosed):
			return nil, none, oneShotWhyGate
		case errors.Is(err, errNoRecipientKeys):
			return nil, none, oneShotWhyKeys
		}
		log.Printf("anet: %s: seal a one-shot envelope to %s: %v", ix, to, err)
		return nil, none, oneShotWhySeal
	}
	clear(sv.ackKey[:]) // no token comes back for a one-shot envelope (ENG-O10)
	env := sv.env
	digest, err := seal.EnvDigest(env)
	var access *seal.Access
	if err == nil {
		access, err = accessFromChoice(choice, digest)
	}
	var wire []byte
	if err == nil {
		wire, err = seal.SetAccess(env, access)
	}
	if err != nil {
		log.Printf("anet: %s: the credential of a one-shot envelope to %s: %v", ix, to, err)
		return nil, none, oneShotWhyAccess
	}
	return wire, choice, ""
}

// handOneShot hands wire over once (ENG-O8): directly, then through the hub
// unless the direct path acknowledged it. The gate is read again before
// each hand-over (ENG-K13).
func (d *Daemon) handOneShot(ctx context.Context, kind oneShotKind, to string, wire []byte, choice AccessChoice, start uint64) OneShotResult {
	var res OneShotResult
	if g := d.identityGate(); !g.Open {
		res.HubReason = gateDeliveryWait(g)
		return d.oneShotDropped(res, oneShotWhyGate)
	}
	if t := d.p2pPath(ctx, to, d.nowMS()); t != nil {
		_, handed, err := t.Send(ctx, to, wire)
		res.Handed = res.Handed || handed
		d.noteTransport(t.Name(), err)
		if err == nil {
			return res // acknowledged by the recipient's node
		}
		if handed {
			// The bytes went and were not taken: the path backs off, as
			// for a queued message (DLV-S-30). No token is expected of a
			// one-shot envelope, so an ack ends no backoff either.
			d.outboxMem.p2pFailed(to, d.nowMS())
		}
	}
	hub := d.configuredHub()
	if hub == "" || !d.hubAskable() {
		why := oneShotWhyNoHub
		if res.Handed {
			why = oneShotWhyDirect
		}
		return d.oneShotDropped(res, why)
	}
	if choice.Kind == seal.AccessKindSession {
		// A first contact needs this node's session with its hub first;
		// without one nothing goes (ENG-A6).
		if _, err := d.firstContactSession(ctx, hub); err != nil {
			res.HubReason = codeSessionUnavailable
			return d.oneShotDropped(res, codeSessionUnavailable)
		}
	}
	for resent := false; ; resent = true {
		if g := d.identityGate(); !g.Open {
			if !res.Handed {
				res.HubReason = gateDeliveryWait(g)
			}
			return d.oneShotDropped(res, oneShotWhyGate)
		}
		r, handed, err := d.relaySendV3(ctx, wire, choice)
		res.Handed = res.Handed || handed
		if err == nil {
			res.HubReason = ""
			if r.Quiet {
				d.noteQuietPeer(to)
			}
			return res
		}
		ans := hubAnswerOf(r, handed, err)
		res.HubReason = oneShotHubReason(ans)
		if !resent && kind == oneShotCardGet {
			if wait, ok := cardGetBusyWait(ans, err); ok && d.waitForCardResend(ctx, wait, start) {
				d.count(cntCardGetResent)
				continue
			}
		}
		why := res.HubReason
		if why == "" {
			why = oneShotWhyNetwork
			if ans.status != 0 {
				why = fmt.Sprintf("hub_%d", ans.status)
			}
		}
		return d.oneShotDropped(res, why)
	}
}

// oneShotHubReason is OneShotResult.HubReason for a hub's answer: its code,
// session_unavailable for a first contact without a session; "" for no
// answer.
func oneShotHubReason(a hubAnswer) string {
	if a.kind == ansSessionUnavailable {
		return codeSessionUnavailable
	}
	return a.code
}

// cardGetBusyWait is ENG-O8a's test of a hub's answer to a card request:
// 429 rate_limited or strangers_busy with a Retry-After (the hub's
// first-contact bucket refills within seconds) — the wait it asks for.
func cardGetBusyWait(a hubAnswer, err error) (time.Duration, bool) {
	if a.status != http.StatusTooManyRequests || (a.code != "rate_limited" && a.code != "strangers_busy") {
		return 0, false
	}
	return retryAfter(err)
}

// waitForCardResend waits wait before a card request goes again (ENG-O8a),
// and reports whether it may: the wait must end within the card's
// (cardWaitMS from start, and the caller's deadline), and the caller must
// still be waiting.
func (d *Daemon) waitForCardResend(ctx context.Context, wait time.Duration, start uint64) bool {
	now := d.nowMS()
	left := time.Duration(0)
	if end := start + cardWaitMS; end > now {
		left = time.Duration(end-now) * time.Millisecond
	}
	if dl, ok := ctx.Deadline(); ok {
		left = min(left, time.Until(dl))
	}
	if wait > left {
		return false
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-d.ctx.Done():
		return false
	}
}

// oneShotDropped counts a one-shot envelope that did not go (or that the
// hub refused) under why, and returns res.
func (d *Daemon) oneShotDropped(res OneShotResult, why string) OneShotResult {
	d.count(cntOneShotDroppedPrefix + why)
	return res
}
