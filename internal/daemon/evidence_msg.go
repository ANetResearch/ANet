package daemon

// evidence_msg.go writes anet.message.sent and anet.message.received
// (A2A-DESIGN §14): one event per conversation message this node sends or
// stores, carrying identifiers and sizes, never content.
//
// The CID is taken over the message payload as it travels inside the
// sealed envelope (the ChatMsg encoding, which includes the sender's random
// message id), so the two sides of one message record the same CID and a
// short message's CID cannot be found by guessing its text. The body alone
// is never hashed on its own.
//
// Messages on interactions with parties this node did not name (trust
// public and public_cap) are counted into the inbound summary window
// (inbound.go) instead of being written one by one: anyone can cause those,
// and every chain append is a signature and an fsync.

import (
	"log"

	"github.com/ANetResearch/ANetCore/anetcid"

	"github.com/ANetResearch/ANet/internal/evtypes"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// EvMessageSent records a conversation message this node sent (or queued
// for delivery in the same write that recorded it).
const EvMessageSent = evtypes.MessageSent

// EvMessageReceived records a conversation message this node received and
// stored. A redelivery that stores nothing records nothing.
const EvMessageReceived = evtypes.MessageReceived

// messageKind is the kind a message is recorded under on both sides: a
// message carrying x402.payment.status is a payment message, anything else
// is text. It is the classification the receiving side stores
// (ingestMessage), so the two sides agree.
func messageKind(meta []byte) string {
	if hasPaymentStatus(meta) {
		return interactions.MsgPayment
	}
	return interactions.MsgText
}

// maxEvidenceMsgID bounds a message id recorded as it is. This node mints
// "msg_" and 32 hex digits; a peer's id is its own choice.
const maxEvidenceMsgID = 128

// evidenceMsgID is the message id as the chain records it. An id this
// node could have minted, or one of the same modest shape, is kept as it
// is, so the two sides of one message can be matched. Anything else (too
// long, or with characters an identifier does not need) is a peer putting
// something other than an identifier there; the chain is permanent and
// carries no content, so it gets the CID of the id instead.
func evidenceMsgID(id string) string {
	ok := len(id) <= maxEvidenceMsgID
	for i := 0; ok && i < len(id); i++ {
		c := id[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-' || c == '.' || c == ':'
	}
	if ok {
		return id
	}
	c, err := anetcid.SumRaw([]byte(id))
	if err != nil {
		return ""
	}
	return "cid:" + c
}

// messageEvidence is the payload of one message event.
func messageEvidence(ix, msgID, kind string, payload []byte, attachments int) (map[string]any, error) {
	c, err := anetcid.Sum(payload)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"interaction_id": ix,
		"msg_id":         evidenceMsgID(msgID),
		"kind":           kind,
		"cid":            c,
		"bytes":          len(payload),
		"attachments":    attachments,
	}, nil
}

// strangerTrust reports whether an interaction of this trust is with a
// party this node did not name.
func strangerTrust(trust string) bool {
	return trust == interactions.TrustPublic || trust == interactions.TrustPublicCap
}

// recordMessageSent writes anet.message.sent for a message on ix, or counts
// it into the window when ix is with a stranger.
func (d *Daemon) recordMessageSent(ix *interactions.Interaction, msgID, kind string, payload []byte, attachments int) {
	d.recordMessage(EvMessageSent, ix, msgID, kind, payload, attachments)
}

// recordMessageReceived is recordMessageSent for a stored inbound message.
func (d *Daemon) recordMessageReceived(ix *interactions.Interaction, msgID, kind string, payload []byte, attachments int) {
	d.recordMessage(EvMessageReceived, ix, msgID, kind, payload, attachments)
}

func (d *Daemon) recordMessage(typ string, ix *interactions.Interaction, msgID, kind string, payload []byte, attachments int) {
	if d.ledger == nil || ix == nil {
		return
	}
	if strangerTrust(ix.Trust) {
		d.noteMessagePublic(typ, ix.PeerAID, ix.Trust, len(payload))
		return
	}
	ev, err := messageEvidence(ix.ID, msgID, kind, payload, attachments)
	if err != nil {
		log.Printf("anet: %s: message evidence: %v", ix.ID, err)
		return
	}
	if _, err := d.ledger.Append(typ, ev); err != nil {
		log.Printf("anet: %s: message evidence: %v", ix.ID, err)
	}
}

// msgWindow counts one direction of stranger messages in the current
// inbound summary window.
type msgWindow struct {
	count   int
	bytes   int
	byTrust map[string]int
	aids    []string
}

// msgAgg is the message part of inboundAgg; it is guarded by inboundAgg.mu.
type msgAgg struct {
	sent, received msgWindow
}

// noteMessagePublic counts one stranger message into the current window.
func (d *Daemon) noteMessagePublic(typ, peer, trust string, n int) {
	a := &d.inAgg
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.start == 0 {
		a.start = int64(d.nowMS())
	}
	w := &a.msg.received
	if typ == EvMessageSent {
		w = &a.msg.sent
	}
	w.count++
	w.bytes += n
	if w.byTrust == nil {
		w.byTrust = map[string]int{}
	}
	w.byTrust[trust]++
	w.aids = addAID(w.aids, peer)
}

// appendMessageSummary writes the window's aggregated message events. It is
// called by flushInboundSummary with the window it took.
func (d *Daemon) appendMessageSummary(m msgAgg, start, end int64) {
	d.appendMessageWindow(EvMessageSent, m.sent, start, end)
	d.appendMessageWindow(EvMessageReceived, m.received, start, end)
}

func (d *Daemon) appendMessageWindow(typ string, w msgWindow, start, end int64) {
	if w.count == 0 || d.ledger == nil {
		return
	}
	if _, err := d.ledger.Append(typ, map[string]any{
		"aggregated": true, "window_start": start, "window_end": end,
		"count": w.count, "bytes": w.bytes, "by_trust": w.byTrust, "first_aids": w.aids,
	}); err != nil {
		log.Printf("anet: message summary evidence: %v", err)
	}
}
