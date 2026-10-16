package main

// doctor_delivery.go is doctor's delivery.outbox (DLV-L-7): the outbound
// queue in the words of /status.delivery.message. With the daemon running
// it reads /status; without, it opens interactions.db read-only and
// immutable, and the gate from the identity file, and changes nothing.

import (
	"errors"
	"io/fs"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func deliveryOutboxCheck(say func(id, status, key string, args a2ashape.Args), rep *doctorReport, env doctorEnv,
	layout daemon.Layout, lang a2ashape.Language) {
	const id = "delivery.outbox"
	var sum interactions.OutboxSummary
	if rep.Control.Running && env.status != nil {
		if st, err := env.status(rep.Control.Addr, layout.ControlTokenPath()); err == nil {
			if d, ok := st["delivery"].(map[string]any); ok {
				if _, has := d["stuck"]; has {
					sum = summaryOf(d)
					sayOutbox(say, id, sum)
					return
				}
			}
		}
	}
	open := true
	if st, err := daemon.ReadIdentityStatus(layout, lang); err == nil {
		open = st.State == "ok"
	}
	sum, err := interactions.ReadOutboxSummary(layout.Root, time.Now().UnixMilli(), open)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		sum = interactions.OutboxSummary{}
	case err != nil:
		say(id, stUnknown, "outbox.unreadable", a2ashape.Args{"error": err.Error()})
		return
	}
	sayOutbox(say, id, sum)
}

func sayOutbox(say func(id, status, key string, args a2ashape.Args), id string, sum interactions.OutboxSummary) {
	key, args, level := daemon.OutboxFinding(sum)
	status := stOK
	if level == a2ashape.LevelWarn {
		status = stWarn
	}
	say(id, status, key, args)
}

// summaryOf reads the numbers of /status.delivery.
func summaryOf(d map[string]any) interactions.OutboxSummary {
	num := func(k string) int64 { f, _ := d[k].(float64); return int64(f) }
	s, _ := d["stuck_error"].(string)
	return interactions.OutboxSummary{NotSent: int(num("not_sent")), Awaiting: int(num("awaiting_confirmation")),
		QueuedAtHub: int(num("queued_at_hub")), OldestAwaitingMS: num("oldest_awaiting_ms"), Stuck: int(num("stuck")),
		Unconfirmed24h: int(num("unconfirmed_24h")), StuckOldestMS: num("stuck_oldest_ms"), StuckError: s}
}
