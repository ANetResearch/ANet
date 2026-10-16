package a2ashape_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func dlvSource(state interactions.State, msgs ...interactions.Message) a2ashape.Source {
	return a2ashape.Source{
		Interaction: &interactions.Interaction{ID: "ix1", Role: interactions.RoleOutbound, PeerAID: peer, State: state,
			UpdatedAt: "2026-10-01T14:30:00Z"},
		Messages: msgs,
	}
}

func ownMsg(seq int64, id, meta string) interactions.Message {
	return interactions.Message{Seq: seq, SenderAID: self, Kind: interactions.MsgText, Body: "hi", MsgID: id,
		Metadata: meta, CreatedAt: "2026-10-01T14:05:00Z"}
}

// DLV-W-21a, DLV-W-20: the task states the delivery of the latest message
// this node queued; a peer's message never carries the keys, whatever it said.
func TestProjectDelivery(t *testing.T) {
	handed := time.Date(2026, 10, 1, 14, 5, 0, 0, time.UTC)
	src := dlvSource(interactions.StateSubmitted,
		ownMsg(1, "m1", `{"anet.delivery":"confirmed"}`),
		interactions.Message{Seq: 2, SenderAID: peer, Kind: interactions.MsgText, Body: "hello", MsgID: "p1",
			Metadata: `{"anet.delivery":"confirmed","anet.delivery_wait":"rate_limited"}`},
		ownMsg(3, "m3", ""),
	)
	src.Outbox = []a2ashape.OutboxMsg{{MsgID: "m3", Detail: a2ashape.DeliveryQueuedAtHub, Wait: "mailbox_full",
		Delegation: true, Handed: true, HandedAt: handed, Exp: handed.Add(14 * 24 * time.Hour)}}
	task := a2ashape.Project(src, a2ashape.Options{NoResponseAfter: 15 * time.Minute})
	m := task.Metadata
	if m[a2ashape.KeyDelivery] != "pending" || m[a2ashape.KeyDeliveryDetail] != "queued_at_hub" || m[a2ashape.KeyDeliveryWait] != "mailbox_full" {
		t.Fatalf("task keys: %v", m)
	}
	if m[a2ashape.KeyDeliveryEndsAt] != "2026-10-01T14:20:00Z" {
		t.Errorf("ends_at %v, want handed_at + no_response_after", m[a2ashape.KeyDeliveryEndsAt])
	}
	for _, h := range task.History {
		switch h.ID {
		case "m1":
			if h.Metadata[a2ashape.KeyDelivery] != "confirmed" {
				t.Errorf("m1: %v", h.Metadata)
			}
		case "p1":
			for _, k := range []string{a2ashape.KeyDelivery, a2ashape.KeyDeliveryWait, a2ashape.KeyDeliveryDetail} {
				if _, ok := h.Metadata[k]; ok {
					t.Errorf("the peer's message carries %s", k)
				}
			}
		case "m3":
			if h.Metadata[a2ashape.KeyDelivery] != "pending" {
				t.Errorf("m3: %v", h.Metadata)
			}
		}
	}
}

// A task with no message of this node's through the queue states nothing,
// and a peer's claim does not make one.
func TestProjectDeliveryAbsent(t *testing.T) {
	src := dlvSource(interactions.StateWorking, interactions.Message{Seq: 1, SenderAID: peer, Kind: interactions.MsgText,
		Body: "x", MsgID: "p1", Metadata: `{"anet.delivery":"pending","anet.delivery_detail":"sent"}`})
	task := a2ashape.Project(src, a2ashape.Options{})
	for _, k := range []string{a2ashape.KeyDelivery, a2ashape.KeyDeliveryDetail, a2ashape.KeyDeliveryWait} {
		if _, ok := task.Metadata[k]; ok {
			t.Errorf("%s on a task with no message of this node's", k)
		}
	}
}

// A confirmed message states no detail; an unconfirmed one keeps it; a value
// outside the three is dropped (M33: nothing is read from stored text blindly).
func TestProjectDeliveryStoredStates(t *testing.T) {
	cases := []struct{ meta, want, detail string }{
		{`{"anet.delivery":"confirmed","anet.delivery_detail":"sent"}`, "confirmed", ""},
		{`{"anet.delivery":"unconfirmed","anet.delivery_detail":"sent"}`, "unconfirmed", "sent"},
		{`{"anet.delivery":"bogus"}`, "", ""},
	}
	for _, c := range cases {
		task := a2ashape.Project(dlvSource(interactions.StateSubmitted, ownMsg(1, "m1", c.meta)), a2ashape.Options{})
		got, _ := task.Metadata[a2ashape.KeyDelivery].(string)
		d, _ := task.Metadata[a2ashape.KeyDeliveryDetail].(string)
		if got != c.want || d != c.detail {
			t.Errorf("%s: got %q/%q", c.meta, got, d)
		}
	}
}

// DLV-S-35, DLV-A-3: a task a delivery abandonment failed says the stored
// variant's sentence; the next step is the variant's.
func TestAbandonedTaskSentence(t *testing.T) {
	src := dlvSource(interactions.StateFailed, ownMsg(1, "m1", `{"anet.delivery":"unconfirmed","anet.delivery_detail":"sent"}`))
	src.Interaction.ResultMeta = `{"anet.reason":"delivery_unconfirmed"}`
	src.Interaction.ReasonVariant = "waiting"
	src.Interaction.ReasonArg = `{"wait":"strangers_busy","limit_ms":900000}`
	task := a2ashape.Project(src, a2ashape.Options{})
	want := a2ashape.Sentence("delivery_unconfirmed.waiting", a2ashape.Lang(), a2ashape.Args{"limit": 15 * time.Minute, "wait": "strangers_busy"})
	if task.Status.Message == nil || len(task.Status.Message.Parts) == 0 || task.Status.Message.Parts[0].Text != want {
		t.Fatalf("status.message %+v, want %q", task.Status.Message, want)
	}
	if strings.Contains(want, "{") {
		t.Errorf("unfilled placeholder in %q", want)
	}
}

// TestDeliveryLine: line 2 of the human lines (DLV-L-2), with the tail
// sentence, without it for an ended task, and the end moment of
// no_response_after = "0" being exp.
func TestDeliveryLine(t *testing.T) {
	handed := time.Date(2026, 10, 1, 14, 5, 0, 0, time.UTC)
	nowAt := handed.Add(time.Minute)
	pending := func(nra time.Duration) a2ashape.Task {
		src := dlvSource(interactions.StateSubmitted, ownMsg(1, "m1", ""))
		src.Outbox = []a2ashape.OutboxMsg{{MsgID: "m1", Detail: a2ashape.DeliverySent, Delegation: true, Handed: true,
			HandedAt: handed, Exp: handed.Add(14 * 24 * time.Hour)}}
		return a2ashape.Project(src, a2ashape.Options{NoResponseAfter: nra})
	}
	// With the tail sentence.
	line, ok := a2ashape.DeliveryLine(pending(15*time.Minute), a2ashape.English, nowAt.UnixMilli())
	end := time.Date(2026, 10, 1, 14, 20, 0, 0, time.UTC)
	wantTail := a2ashape.Sentence("delivery.ends_at", a2ashape.English, a2ashape.Args{"time": end, "now": end})
	if !ok || !strings.HasPrefix(line, a2ashape.Sentence("delivery.sent", a2ashape.English, nil)) ||
		!strings.Contains(line, wantTail) || !strings.HasSuffix(line, "(waited 1 minute)") {
		t.Errorf("pending: %q (tail %q)", line, wantTail)
	}
	// no_response_after = "0": the moment is exp, 14 days on.
	line, _ = a2ashape.DeliveryLine(pending(0), a2ashape.English, nowAt.UnixMilli())
	exp := handed.Add(14 * 24 * time.Hour)
	if !strings.Contains(line, a2ashape.Sentence("delivery.ends_at", a2ashape.English, a2ashape.Args{"time": exp, "now": exp})) {
		t.Errorf("no_response_after 0: %q", line)
	}
	// Chinese form.
	line, _ = a2ashape.DeliveryLine(pending(15*time.Minute), a2ashape.Chinese, nowAt.UnixMilli())
	if !strings.HasSuffix(line, "(已等 1 分钟)") {
		t.Errorf("zh: %q", line)
	}
	// An ended task: no tail; the wait is to the moment it ended.
	src := dlvSource(interactions.StateFailed, ownMsg(1, "m1", `{"anet.delivery":"unconfirmed","anet.delivery_detail":"sent"}`))
	src.Interaction.StateAt = time.Date(2026, 10, 1, 14, 20, 0, 0, time.UTC).UnixMilli()
	line, ok = a2ashape.DeliveryLine(a2ashape.Project(src, a2ashape.Options{NoResponseAfter: 15 * time.Minute}), a2ashape.English, nowAt.UnixMilli())
	if !ok || strings.Contains(line, "If it is still not delivered") || !strings.HasSuffix(line, "(waited 15 minutes)") {
		t.Errorf("unconfirmed: %q", line)
	}
	// No timestamp of the end: the sentence stands without the wait.
	src.Interaction.StateAt, src.Interaction.UpdatedAt = 0, ""
	line, _ = a2ashape.DeliveryLine(a2ashape.Project(src, a2ashape.Options{}), a2ashape.English, nowAt.UnixMilli())
	if strings.Contains(line, "waited") || line == "" {
		t.Errorf("no end moment: %q", line)
	}
	// Confirmed and absent states give no line.
	src = dlvSource(interactions.StateSubmitted, ownMsg(1, "m1", `{"anet.delivery":"confirmed"}`))
	if l, ok := a2ashape.DeliveryLine(a2ashape.Project(src, a2ashape.Options{}), a2ashape.English, 0); ok {
		t.Errorf("confirmed: %q", l)
	}
}

// DLV-A-3 (TestWaitClassNoStatusMessage): a wait is never status.message; a
// client that reads only the text must not take it for an answer.
func TestWaitClassNoStatusMessage(t *testing.T) {
	for _, wait := range []string{"rate_limited", "strangers_busy", "mailbox_full", "home_unreachable", "session_unavailable", "notice_stale", "rotation_pending"} {
		src := dlvSource(interactions.StateSubmitted, ownMsg(1, "m1", ""))
		src.Outbox = []a2ashape.OutboxMsg{{MsgID: "m1", Detail: a2ashape.DeliveryNotSent, Wait: wait}}
		task := a2ashape.Project(src, a2ashape.Options{})
		if task.Status.Message != nil {
			t.Errorf("%s: status.message %+v", wait, task.Status.Message)
		}
		if task.Metadata[a2ashape.KeyDeliveryWait] != wait {
			t.Errorf("%s: %v", wait, task.Metadata)
		}
		if a2ashape.DeliveryText("pending", "not_sent", wait, false, time.Time{}, a2ashape.English) == "" {
			t.Errorf("%s has no sentence", wait)
		}
	}
}
