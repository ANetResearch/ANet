package daemon

// RC-5: the delivery state the projection, the event stream and /status
// state (DLV-P-2 to P-4, DLV-S-35, DLV-S-36).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func deliveryEvents(ch <-chan Event) []DeliveryEvent {
	var out []DeliveryEvent
	for {
		select {
		case e := <-ch:
			if e.Kind == EventDelivery && e.delivery != nil {
				out = append(out, *e.delivery)
			}
		case <-time.After(100 * time.Millisecond):
			return out
		}
	}
}

// DLV T39 (DLV-P-3): the delegation is pending once handed over, and
// confirmed after the confirmation: one delivery event each, text =
// DeliveryText, the state sequence unchanged; the same value is not
// published twice.
func TestRC5DeliveryEventAndProjection(t *testing.T) {
	_, req, prov := registeredPair(t)
	id, err := req.Delegate(context.Background(), prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, _ := req.ix.Get(id)
	task, err := req.taskView(ix, viewOpts{})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := a2ashape.DeliveryOf(task.Metadata)
	if !ok || s.Delivery != a2ashape.DeliveryPending || (s.Detail != a2ashape.DeliverySent && s.Detail != a2ashape.DeliveryQueuedAtHub) {
		t.Fatalf("handed over: %v", task.Metadata)
	}
	if s.EndsAt.IsZero() {
		t.Errorf("a pending delegation has a give-up moment (UX-R11): %v", task.Metadata)
	}
	_, events, cancel, err := req.Watch(id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	req.noteDelivery(id) // nothing changed since the last note: nothing is published
	if got := deliveryEvents(events); len(got) != 0 {
		t.Fatalf("an unchanged value was published: %+v", got)
	}
	row := outboxRows(t, req, id)[0]
	if err := req.confirmOutbox(row.ID, seal.OutcomeProcessed, seal.Note{}, cntDAckConfirmedHub); err != nil {
		t.Fatal(err)
	}
	got := deliveryEvents(events)
	if len(got) != 1 || got[0].Delivery != a2ashape.DeliveryConfirmed ||
		got[0].Text != a2ashape.DeliveryText("confirmed", "", "", false, time.Time{}, a2ashape.Lang()) {
		t.Fatalf("after the confirmation: %+v", got)
	}
	after, _ := req.ix.Get(id)
	task, _ = req.taskView(after, viewOpts{})
	if task.Metadata[a2ashape.KeyDelivery] != a2ashape.DeliveryConfirmed {
		t.Errorf("confirmed: %v", task.Metadata)
	}
	if _, ok := task.Metadata[a2ashape.KeyDeliveryWait]; ok {
		t.Errorf("a confirmed message has no wait: %v", task.Metadata)
	}
}

// DLV T40 (DLV-W-21b, 21c): a wait code from the last send shows as the
// task's wait; a peer's metadata cannot set it.
func TestRC5WaitCodeInProjection(t *testing.T) {
	_, req, prov := registeredPair(t)
	setHubURL(req, deadHubURL)
	id, err := req.Delegate(context.Background(), prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	ix, _ := req.ix.Get(id)
	task, _ := req.taskView(ix, viewOpts{})
	if task.Metadata[a2ashape.KeyDelivery] != a2ashape.DeliveryPending || task.Metadata[a2ashape.KeyDeliveryDetail] != a2ashape.DeliveryNotSent {
		t.Fatalf("not sent: %v", task.Metadata)
	}
}

// DLV-S-35 (T41): an abandonment writes the variant with the task, and the
// projection says its sentence in status.message.
func TestRC5AbandonVariants(t *testing.T) {
	for _, c := range []struct {
		name    string
		why     abandonWhy
		variant string
	}{
		{"deadline", abandonDeadline(reasonDeliveryUnconfirmed), "expired"},
		{"scan", abandonWhy{reason: reasonDeliveryUnconfirmed, trigger: a2ashape.DeliveryCauseScan, touchTask: true}, "sent"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, req, prov := registeredPair(t)
			id, err := req.Delegate(context.Background(), prov.AID(), "work", nil)
			if err != nil {
				t.Fatal(err)
			}
			row, vers, err := req.ix.OutboxState(outboxRows(t, req, id)[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			_ = req.abandonOutbox(row, vers, c.why, nil)
			ix, _ := req.ix.Get(id)
			if ix.State != interactions.StateFailed || ix.ReasonVariant == "" {
				t.Fatalf("state %s, variant %q", ix.State, ix.ReasonVariant)
			}
			if c.variant == "expired" && ix.ReasonVariant != "expired" {
				t.Errorf("variant %q, want expired", ix.ReasonVariant)
			}
			var arg map[string]any
			if json.Unmarshal([]byte(ix.ReasonArg), &arg) != nil || arg["limit_ms"] == nil {
				t.Errorf("reason_arg %q", ix.ReasonArg)
			}
			task, _ := req.taskView(ix, viewOpts{})
			text := task.Status.Message.Parts[0].Text
			if strings.Contains(text, "failed:") || strings.Contains(text, "{") || text == "" {
				t.Errorf("status.message %q", text)
			}
			if task.Metadata[a2ashape.KeyReason] != "delivery_unconfirmed" || task.Metadata[a2ashape.KeyDelivery] != a2ashape.DeliveryUnconfirmed {
				t.Errorf("metadata %v", task.Metadata)
			}
		})
	}
}

// DLV T48 (DLV-P-4): /status.delivery counts the queue and says the sentence
// of ux C4.
func TestRC5StatusDelivery(t *testing.T) {
	_, req, prov := registeredPair(t)
	rec := httptest.NewRecorder()
	req.hStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	dl, _ := st["delivery"].(map[string]any)
	if dl == nil || dl["message"] != a2ashape.Sentence("outbox.none", a2ashape.Lang(), nil) || dl["level"] != "ok" {
		t.Fatalf("empty queue: %v", dl)
	}
	if _, err := req.Delegate(context.Background(), prov.AID(), "work", nil); err != nil {
		t.Fatal(err)
	}
	d := req.deliveryStatus()
	if d["awaiting_confirmation"] != 1 || d["stuck"] != 0 || d["not_sent"] != 0 {
		t.Fatalf("one handed-over row: %v", d)
	}
	if !strings.Contains(d["message"].(string), "1") || d["level"] != "ok" {
		t.Errorf("message %v", d["message"])
	}
	// Eight days on, still unconfirmed after a day: the C4 warning.
	sum, err := req.ix.OutboxSummary(time.Now().Add(30*time.Hour).UnixMilli(), true)
	if err != nil {
		t.Fatal(err)
	}
	if key, _, level := OutboxFinding(sum); key != "outbox.unconfirmed" || level != a2ashape.LevelWarn {
		t.Errorf("a day later: %s %s %+v", key, level, sum)
	}
}
