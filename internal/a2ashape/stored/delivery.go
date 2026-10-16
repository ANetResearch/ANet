package stored

// delivery.go places the delivery state of the messages this node sent on a
// projection (DLV-W-20 to 21d, DLV-P-2): confirmed and unconfirmed are the
// engine's stored words on the message; pending, its detail, its wait code
// and the order rule come from the queued row (Source.Outbox), computed by
// the engine's own functions. Nothing is read from a peer's message.

import (
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// deliveryKeys are the keys only this node computes (CORE-K17).
var deliveryKeys = []string{a2ashape.KeyDelivery, a2ashape.KeyDeliveryDetail, a2ashape.KeyDeliveryWait, a2ashape.KeyDeliverySince, a2ashape.KeyDeliveryEndsAt, a2ashape.KeyDeliveryOrdered}

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
	for _, k := range []string{a2ashape.KeyDeliveryWait, a2ashape.KeyDeliverySince, a2ashape.KeyDeliveryEndsAt, a2ashape.KeyDeliveryOrdered} {
		delete(m, k)
	}
	if id := p.msgs[i].MsgID; id != "" {
		for _, o := range p.outbox {
			if o.MsgID == id {
				m[a2ashape.KeyDelivery], m[a2ashape.KeyDeliveryDetail] = a2ashape.DeliveryPending, o.Detail
				if o.Wait != "" {
					m[a2ashape.KeyDeliveryWait] = o.Wait
				}
				break
			}
		}
	}
	switch m[a2ashape.KeyDelivery] {
	case a2ashape.DeliveryPending, a2ashape.DeliveryConfirmed, a2ashape.DeliveryUnconfirmed:
	default:
		delete(m, a2ashape.KeyDelivery)
		delete(m, a2ashape.KeyDeliveryDetail)
	}
	if m[a2ashape.KeyDelivery] == a2ashape.DeliveryConfirmed {
		delete(m, a2ashape.KeyDeliveryDetail)
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
		d, _ := p.metas[i][a2ashape.KeyDelivery].(string)
		if d == "" {
			continue
		}
		m[a2ashape.KeyDelivery] = d
		if v, ok := p.metas[i][a2ashape.KeyDeliveryDetail]; ok {
			m[a2ashape.KeyDeliveryDetail] = v
		}
		if v, ok := p.metas[i][a2ashape.KeyDeliveryWait]; ok {
			m[a2ashape.KeyDeliveryWait] = v
		}
		if t, err := time.Parse(time.RFC3339Nano, p.msgs[i].CreatedAt); err == nil {
			m[a2ashape.KeyDeliverySince] = t.UTC().Format(time.RFC3339)
		}
		if d == a2ashape.DeliveryPending {
			for _, o := range p.outbox {
				if o.MsgID == p.msgs[i].MsgID && o.Ordered {
					m[a2ashape.KeyDeliveryOrdered] = true
				}
			}
			if at := a2ashape.DeliveryEndsAt(p.deadlines(), p.opt.NoResponseAfter, p.ix.IsTerminal()); !at.IsZero() {
				m[a2ashape.KeyDeliveryEndsAt] = at.UTC().Format(time.RFC3339)
			}
		}
		return
	}
}

// deadlines are the task's queued rows as UX-R11 reads them.
func (p *projector) deadlines() []a2ashape.OutboxDeadline {
	out := make([]a2ashape.OutboxDeadline, 0, len(p.outbox))
	for _, o := range p.outbox {
		out = append(out, a2ashape.OutboxDeadline{Delegation: o.Delegation, Handed: o.Handed, HandedAt: o.HandedAt, Exp: o.Exp, Frozen: o.Frozen})
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
		return a2ashape.Sentence(reason, a2ashape.Lang(), nil)
	case "delivery_unconfirmed":
		if ix.ReasonVariant == "" {
			return ""
		}
		arg := a2ashape.DecodeObject([]byte(ix.ReasonArg))
		args := a2ashape.Args{}
		if ms := wholeMS(arg["limit_ms"]); ms > 0 {
			args["limit"] = time.Duration(min(ms, int64(1<<62)/int64(time.Millisecond))) * time.Millisecond
		}
		if w, _ := arg["wait"].(string); w != "" {
			args["wait"] = w
		}
		return a2ashape.Sentence(reason+"."+ix.ReasonVariant, a2ashape.Lang(), args)
	}
	return ""
}
