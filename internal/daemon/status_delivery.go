package daemon

// status_delivery.go is /status.delivery (DLV-P-4): the outbound queue in
// numbers and in one sentence of ux §1.4 C4, which doctor's delivery.outbox
// says word for word.

import (
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// OutboxFinding is the sentence key and arguments that sum up an outbound
// queue, in the order DLV-P-4 gives: stuck, then unconfirmed for a day, then
// waiting for confirmation, then nothing waiting. The level is that of the
// key's row.
func OutboxFinding(s interactions.OutboxSummary) (key string, args a2ashape.Args, level a2ashape.Level) {
	switch {
	case s.Stuck > 0:
		key = "outbox.stuck"
		args = a2ashape.Args{"n": s.Stuck, "dur": time.Duration(s.StuckOldestMS) * time.Millisecond, "error": s.StuckError}
	case s.Unconfirmed24h > 0:
		key, args = "outbox.unconfirmed", a2ashape.Args{"n": s.Unconfirmed24h}
	case s.Awaiting > 0:
		key = "outbox.ok"
		args = a2ashape.Args{"n": s.Awaiting, "dur": time.Duration(s.OldestAwaitingMS) * time.Millisecond}
	default:
		key = "outbox.none"
	}
	level = a2ashape.LevelOK
	if r, ok := a2ashape.Lookup(key); ok {
		level = r.Level
	}
	return key, args, level
}

// deliveryStatus is the "delivery" member of /status.
func (d *Daemon) deliveryStatus() map[string]any {
	now := int64(d.nowMS())
	counters := map[string]uint64{}
	for k, v := range d.ReceiveStats() {
		if len(k) >= 5 && k[:5] == "dack-" {
			counters[k] = v
		}
	}
	out := map[string]any{"counters": counters}
	s, err := d.ix.OutboxSummary(now, d.identityGate().Open)
	if err != nil {
		key := "outbox.unreadable"
		out["level"], out["message"] = string(a2ashape.LevelUnknown), a2ashape.Sentence(key, a2ashape.Lang(), a2ashape.Args{"error": err.Error()})
		if n := a2ashape.Next(key, a2ashape.Lang(), nil); n != "" {
			out["next"] = n
		}
		return out
	}
	out["not_sent"], out["awaiting_confirmation"], out["queued_at_hub"] = s.NotSent, s.Awaiting, s.QueuedAtHub
	out["oldest_awaiting_ms"], out["stuck"], out["unconfirmed_24h"] = s.OldestAwaitingMS, s.Stuck, s.Unconfirmed24h
	if s.Stuck > 0 {
		// What doctor needs to say the same sentence (delivery.outbox).
		out["stuck_oldest_ms"], out["stuck_error"] = s.StuckOldestMS, s.StuckError
	}
	key, args, level := OutboxFinding(s)
	out["level"], out["message"] = string(level), a2ashape.Sentence(key, a2ashape.Lang(), args)
	if n := a2ashape.Next(key, a2ashape.Lang(), args); n != "" {
		out["next"] = n
	}
	return out
}
