package daemon

// oneshot.go is the one-shot sender (engine §2.12): the only source of outer
// key 8. A one-shot envelope — a stream_preview frame, an anet.card.get/1 or
// an anet.card/1 (core G11) — is sealed once, never queued, never sealed
// again and sent again at most once (ENG-O8a); its receiver makes no
// delivery token for it, so its ack_key is not kept (ENG-O10).

import (
	"context"

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

// oneShotSend seals body once as kind, to `to` on interaction ix, and hands
// it over once (ENG-O8): a direct path first when one reaches `to`, the hub
// otherwise or after it failed.
//
// v030-todo(ENG-1b): the one-shot sender; until then nothing is sent.
func (d *Daemon) oneShotSend(ctx context.Context, kind oneShotKind, to, ix string, body []byte, opts ...oneShotOption) OneShotResult {
	return OneShotResult{}
}
