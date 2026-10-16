package daemon

// tasks_events.go is POST /tasks/events (correctness-ux §2.7, §4.8): the
// control plane's stream of one task, as server-sent events.
//
//	event: task      the task now, bounded to maxEventTaskBytes
//	event: preview   {seq, at, text, omitted, reasoning?, reasoning_omitted?}
//	event: delivery  {delivery, detail, wait, text}   (UX-C8)
//
// A task event follows each change of the task; the stream ends after the
// one of a terminal state. Preview frames are the only place a provider's
// reasoning is shown (CUX-P17). Delivery events do not move anet.state_seq
// and are not in the A2A stream.
//
// The route takes the control token only: it is neither on the console's
// session allowlist nor an MCP tool.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// EventDelivery is a change of the delivery state of a task's latest
// message (RC-5 publishes it, publishDelivery). It changes nothing stored.
const EventDelivery = "delivery"

// maxEventTaskBytes bounds the task of one task event.
const maxEventTaskBytes = 24576

// eventsKeepAlive is how often an idle stream writes a comment line, so
// that a proxy or a client does not take a quiet task for a dead
// connection.
const eventsKeepAlive = 20 * time.Second

// DeliveryEvent is the data of a delivery event (ux UX-C8).
type DeliveryEvent struct {
	Delivery string `json:"delivery"`
	Detail   string `json:"detail"`
	Wait     string `json:"wait"`
	Text     string `json:"text"`
}

// PreviewEvent is the data of a preview event (§2.7).
type PreviewEvent struct {
	Seq              uint64 `json:"seq"`
	At               uint64 `json:"at"`
	Text             string `json:"text"`
	Omitted          uint64 `json:"omitted"`
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningOmitted uint64 `json:"reasoning_omitted,omitempty"`
}

// publishDelivery publishes a delivery event for task ix.
func (d *Daemon) publishDelivery(ix string, e DeliveryEvent) {
	d.bus.publish(Event{IX: ix, Kind: EventDelivery, At: d.nowMS(), delivery: &e})
}

func previewEventData(f previewFrame) PreviewEvent {
	return PreviewEvent{Seq: f.Seq, At: f.At, Text: f.Text, Omitted: f.Omitted,
		Reasoning: f.Reasoning, ReasoningOmitted: f.ReasoningOmitted}
}

func (d *Daemon) hTasksEvents(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID string `json:"task_id"`
	}
	if err := readJSON(r, &req); err != nil {
		badTaskRequest(w, "body: %v", err)
		return
	}
	if req.TaskID == "" {
		badTaskRequest(w, "task_id is required")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	snap, events, cancel, err := d.Watch(req.TaskID)
	if err != nil {
		cancel()
		if _, nf := d.ix.Get(req.TaskID); nf != nil {
			writeRefusal(w, http.StatusNotFound, a2ashape.Refuse("no_local_task", a2ashape.Args{"task": req.TaskID, "retention": d.ContentRetention().String()}))
			return
		}
		writeTaskError(w, err)
		return
	}
	defer func() { cancel() }()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(name string, data any) bool {
		b, err := json.Marshal(data)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	lastMsg := d.lastPeerMessage(snap)
	lastSeq := snap.StateSeq
	emitTask := func(ix *interactions.Interaction) bool {
		t, err := d.taskView(ix, viewOpts{artifacts: true})
		if err != nil {
			return false
		}
		return emit("task", a2ashape.TaskReadWithin(t, maxEventTaskBytes))
	}
	if !emitTask(snap) || snap.IsTerminal() {
		return
	}
	if f, ok := d.previews().Current(snap.ID); ok && !emit("preview", previewEventData(f)) {
		return
	}
	tick := time.NewTicker(eventsKeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
			continue
		case ev, ok := <-events:
			switch {
			case !ok:
				// Dropped for falling behind: watch again; the new snapshot is
				// compared like any event.
				cancel()
				if _, events, cancel, err = d.Watch(snap.ID); err != nil {
					cancel = func() {}
					return
				}
			case ev.Kind == EventPreview:
				if f, ok := d.previews().Current(snap.ID); ok && !emit("preview", previewEventData(f)) {
					return
				}
				continue
			case ev.Kind == EventDelivery:
				if ev.delivery != nil && !emit("delivery", ev.delivery) {
					return
				}
				continue
			}
		}
		cur, err := d.ix.Get(snap.ID)
		if err != nil {
			return
		}
		msg := d.lastPeerMessage(cur)
		if cur.StateSeq <= lastSeq && msg <= lastMsg {
			continue
		}
		if !emitTask(cur) {
			return
		}
		lastSeq, lastMsg = cur.StateSeq, msg
		if cur.IsTerminal() {
			return
		}
	}
}
