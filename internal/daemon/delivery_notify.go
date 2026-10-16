package daemon

// delivery_notify.go is RC-5's part of the daemon: the outbound rows of a
// task as its projection reads them (DLV-W-21a to 21d), and the delivery
// event (DLV-P-3). Nothing here writes a table.

import (
	"encoding/hex"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// maxDeliveryNotes bounds what is remembered of published states; past it an
// arbitrary entry goes, which at worst repeats one event.
const maxDeliveryNotes = 4096

// deliveryNotes remembers, per task, the last delivery state noteDelivery
// published (in memory only: after a restart the first change is published
// as usual).
type deliveryNotes struct {
	mu   sync.Mutex
	last map[string][3]string
}

// changed records s as the task's published state and reports whether it
// differs from the one before.
func (n *deliveryNotes) changed(ix string, s [3]string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.last == nil {
		n.last = map[string][3]string{}
	}
	if old, ok := n.last[ix]; ok && old == s {
		return false
	}
	if len(n.last) >= maxDeliveryNotes {
		for k := range n.last {
			delete(n.last, k)
			break
		}
	}
	n.last[ix] = s
	return true
}

// outboxMsgs are the queued rows of task ix as the projection reads them:
// the detail of engine §2.3, the wait of ENG-P1 (deliveryWait) and the order
// rule of ENG-O1 to O3 (orderBlocked) — the engine's own functions, not a
// second copy (DLV-W-21b, 21d).
func (d *Daemon) outboxMsgs(ix string) []a2ashape.OutboxMsg {
	rows, err := d.ix.OutboxRows(ix)
	if err != nil || len(rows) == 0 {
		return nil
	}
	now := d.nowMS()
	hub := d.configuredHub()
	gate := d.identityGate()
	views := make([]RowView, len(rows))
	for i := range rows {
		views[i] = rowView(&rows[i])
	}
	out := make([]a2ashape.OutboxMsg, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		_, vers, err := d.ix.OutboxState(r.ID)
		if err != nil {
			continue
		}
		vv := d.verViews(r.ID, vers, hub, now)
		rv := views[i]
		m := a2ashape.OutboxMsg{MsgID: hex.EncodeToString(r.MID), Detail: a2ashape.DeliveryNotSent,
			Wait: deliveryWait(rv, vv, gate), Delegation: r.Type == seal.TypeDelegate,
			Frozen: r.Frozen, Exp: time.UnixMilli(int64(r.Exp)).UTC()}
		if r.HandedAt > 0 || r.Attempting {
			m.Handed = true
			m.Detail = a2ashape.DeliverySent
			if anyQueued(vers) {
				m.Detail = a2ashape.DeliveryQueuedAtHub
			}
			if r.HandedAt > 0 {
				m.HandedAt = time.UnixMilli(r.HandedAt).UTC()
			}
		} else {
			var ahead []RowView
			for j := range rows {
				if rows[j].ToAID == r.ToAID {
					ahead = append(ahead, views[j])
				}
			}
			m.Ordered = orderBlocked(rv, ahead)
		}
		out = append(out, m)
	}
	return out
}

// noteDelivery is RC-5's: after a transaction that changes what a task's
// delivery state may say (enqueue, A8, A12, a status answer, a
// confirmation, an abandonment, a withdrawal), the engine calls it with the
// task (engine ENG-P3, DLV-S-36). It writes nothing: it recomputes the
// task's delivery state and, when it differs from the one last published,
// publishes a delivery event. The event does not change anet.state_seq and
// is not part of the A2A stream (DLV-P-3).
func (d *Daemon) noteDelivery(ixID string) {
	ix, err := d.ix.Get(ixID)
	if err != nil {
		return
	}
	zero := 0
	t, err := d.taskView(ix, viewOpts{historyLen: &zero})
	if err != nil {
		return
	}
	s, ok := a2ashape.DeliveryOf(t.Metadata)
	if !ok || !d.deliveryNotes.changed(ixID, [3]string{s.Delivery, s.Detail, s.Wait}) {
		return
	}
	d.publishDelivery(ixID, DeliveryEvent{Delivery: s.Delivery, Detail: s.Detail, Wait: s.Wait, Text: s.Text(a2ashape.Lang())})
}
