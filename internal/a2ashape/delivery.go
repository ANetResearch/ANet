package a2ashape

// delivery.go places the delivery state of the messages this node sent on a
// projection (DLV-W-20 to 21d, DLV-P-2): confirmed and unconfirmed are the
// engine's stored words on the message; pending, its detail, its wait code
// and the order rule come from the queued row (Source.Outbox), computed by
// the engine's own functions. Nothing is read from a peer's message.

import (
	"time"
)

// deliveryKeys are the keys only this node computes (CORE-K17).
var deliveryKeys = []string{KeyDelivery, KeyDeliveryDetail, KeyDeliveryWait, KeyDeliverySince, KeyDeliveryEndsAt, KeyDeliveryOrdered}

// own reports whether stored message i was sent by this node: a task has
// two parties, and the other one is the peer.
func (p *projector) own(i int) bool { return p.msgs[i].SenderAID != p.ix.PeerAID }

// setDelivery makes the delivery keys of message i this node's own
// statement: a peer's message carries none whatever it said; one of this
// node's keeps what the engine stored (confirmed, unconfirmed) or, while its
// row is queued, is pending with the row's detail and wait.
func (p *projector) setDelivery(i int) {
	m := p.metas[i]
	if !p.own(i) {
		for _, k := range deliveryKeys {
			delete(m, k)
		}
		return
	}
	if m == nil {
		m = map[string]any{}
	}
	for _, k := range []string{KeyDeliveryWait, KeyDeliverySince, KeyDeliveryEndsAt, KeyDeliveryOrdered} {
		delete(m, k)
	}
	if id := p.msgs[i].MsgID; id != "" {
		for _, o := range p.outbox {
			if o.MsgID == id {
				m[KeyDelivery], m[KeyDeliveryDetail] = DeliveryPending, o.Detail
				if o.Wait != "" {
					m[KeyDeliveryWait] = o.Wait
				}
				break
			}
		}
	}
	switch m[KeyDelivery] {
	case DeliveryPending, DeliveryConfirmed, DeliveryUnconfirmed:
	default:
		delete(m, KeyDelivery)
		delete(m, KeyDeliveryDetail)
	}
	if m[KeyDelivery] == DeliveryConfirmed {
		delete(m, KeyDeliveryDetail)
	}
	if len(m) > 0 {
		p.metas[i] = m
	}
}

// deliveryMeta puts the task's delivery keys in m: those of the latest
// message this node queued on it (DLV-W-21a), none when there is no such
// message.
func (p *projector) deliveryMeta(m map[string]any) {
	for i := len(p.msgs) - 1; i >= 0; i-- {
		if !p.own(i) || p.metas[i] == nil {
			continue
		}
		d, _ := p.metas[i][KeyDelivery].(string)
		if d == "" {
			continue
		}
		m[KeyDelivery] = d
		if v, ok := p.metas[i][KeyDeliveryDetail]; ok {
			m[KeyDeliveryDetail] = v
		}
		if v, ok := p.metas[i][KeyDeliveryWait]; ok {
			m[KeyDeliveryWait] = v
		}
		if t, err := time.Parse(time.RFC3339Nano, p.msgs[i].CreatedAt); err == nil {
			m[KeyDeliverySince] = t.UTC().Format(time.RFC3339)
		}
		if d == DeliveryPending {
			for _, o := range p.outbox {
				if o.MsgID == p.msgs[i].MsgID && o.Ordered {
					m[KeyDeliveryOrdered] = true
				}
			}
			if at := deliveryEndsAt(p.deadlines(), p.opt.NoResponseAfter, p.ix.IsTerminal()); !at.IsZero() {
				m[KeyDeliveryEndsAt] = at.UTC().Format(time.RFC3339)
			}
		}
		return
	}
}

// deadlines are the task's queued rows as UX-R11 reads them.
func (p *projector) deadlines() []outboxDeadline {
	out := make([]outboxDeadline, 0, len(p.outbox))
	for _, o := range p.outbox {
		out = append(out, outboxDeadline{delegation: o.Delegation, handed: o.Handed, handedAt: o.HandedAt, exp: o.Exp, frozen: o.Frozen})
	}
	return out
}

// abandonText is the sentence of a task a delivery abandonment failed
// (DLV-S-35, DLV-A-3): delivery_unconfirmed with the variant stored with
// the task, or the neutral sentence of undeliverable. "" for any other
// reason, or one stored without its variant.
func (p *projector) abandonText(reason string) string {
	ix := p.ix
	switch reason {
	case "undeliverable":
		return Sentence(reason, Lang(), nil)
	case "delivery_unconfirmed":
		if ix.ReasonVariant == "" {
			return ""
		}
		arg := decodeObject([]byte(ix.ReasonArg))
		args := Args{}
		if ms := wholeMS(arg["limit_ms"]); ms > 0 {
			args["limit"] = time.Duration(min(ms, int64(1<<62)/int64(time.Millisecond))) * time.Millisecond
		}
		if w, _ := arg["wait"].(string); w != "" {
			args["wait"] = w
		}
		return Sentence(reason+"."+ix.ReasonVariant, Lang(), args)
	}
	return ""
}
