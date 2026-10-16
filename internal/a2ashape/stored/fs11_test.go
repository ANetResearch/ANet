package stored_test

// The visible surface of forward secrecy (FS §5, FS-U4 to FS-U7): the
// privacy line's order, anet.content_expires_at, the kept line and the
// sentences of a task a rollback or an emergency rotation ended.

import (
	"errors"
	"github.com/ANetResearch/ANet/internal/a2ashape/stored"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func said(key string) *a2ashape.Said {
	return &a2ashape.Said{Key: key, Args: a2ashape.Args{"n": 2, "time": "x", "dur": time.Hour}}
}

// TestPrivacyRowPriority (FS-U7): the first row that holds is the line.
func TestPrivacyRowPriority(t *testing.T) {
	in := a2ashape.StatusInput{HubRegistered: true, Retention: a2ashape.Said{Key: "retention.summary", Args: a2ashape.Args{"retention": "30d"}},
		LocalStateRepeated: said("local_state.repeated"), LocalStateDetected: said("local_state.detected"),
		Clock: said("clock.skewed"), Keys: said("keys.kept_long")}
	for _, want := range []string{"local_state.repeated", "local_state.detected", "clock.skewed", "keys.kept_long", "retention.summary"} {
		if got := a2ashape.PrivacyLine(in).Key; got != want {
			t.Fatalf("privacy line %s, want %s", got, want)
		}
		switch want {
		case "local_state.repeated":
			in.LocalStateRepeated = nil
		case "local_state.detected":
			in.LocalStateDetected = nil
		case "clock.skewed":
			in.Clock = nil
		case "keys.kept_long":
			in.Keys = nil
		}
	}
	// Repeated beats detected even when only the second is told; the
	// summary carries the topic and the level of the chosen row.
	in.LocalStateRepeated, in.LocalStateDetected = said("local_state.repeated"), said("local_state.detected")
	lines := a2ashape.StatusSummary(in, a2ashape.English)
	var got *a2ashape.SummaryLine
	for i := range lines {
		if lines[i].Topic == a2ashape.TopicPrivacy {
			got = &lines[i]
		}
	}
	if got == nil || got.Level != a2ashape.LevelFail || got.Next != "anet identity rotate --emergency" {
		t.Fatalf("the privacy line %+v, want local_state.repeated (fail) with the emergency rotation as next", got)
	}
}

// TestPrivacyRowSilentWhileNotRegistered (FS-U7): the clock sentences wait
// for the hub line to be the one that speaks; the keys and the rollback do
// not wait.
func TestPrivacyRowSilentWhileNotRegistered(t *testing.T) {
	in := a2ashape.StatusInput{Retention: a2ashape.Said{Key: "retention.summary", Args: a2ashape.Args{"retention": "30d"}},
		Clock: said("clock.unverified")}
	if got := a2ashape.PrivacyLine(in).Key; got != "retention.summary" {
		t.Fatalf("not registered: %s, want retention.summary", got)
	}
	in.Keys = said("keys.unknown_drops")
	if got := a2ashape.PrivacyLine(in).Key; got != "keys.unknown_drops" {
		t.Fatalf("not registered with keys: %s", got)
	}
	in.HubRegistered = true
	if got := a2ashape.PrivacyLine(in).Key; got != "clock.unverified" {
		t.Fatalf("registered: %s, want clock.unverified", got)
	}
}

func ended(role interactions.Role, trust string, state interactions.State, at time.Time) *interactions.Interaction {
	return &interactions.Interaction{ID: "ix_a1", Role: role, PeerAID: notePeer, State: state, StateSeq: 3, StateAt: at.UnixMilli(),
		ContextID: "c", Trust: trust}
}

// TestContentExpiresAt (FS-U4, FS-L5): an ended task carries the day after
// state_at plus the retention; a task that has not ended, one kept for ever
// and a backend's view carry none.
func TestContentExpiresAt(t *testing.T) {
	at := time.Date(2026, 10, 1, 15, 30, 0, 0, time.UTC)
	r30, _ := interactions.ParseRetention("30d")
	forever, _ := interactions.ParseRetention("forever")
	get := func(ix *interactions.Interaction, o stored.Options) any {
		return stored.Project(stored.Source{Interaction: ix}, o).Metadata[a2ashape.KeyContentExpiresAt]
	}
	if got, want := get(ended(interactions.RoleOutbound, "", interactions.StateCompleted, at), stored.Options{Retention: r30}), "2026-11-01T00:00:00Z"; got != want {
		t.Errorf("30 days from %s: %v, want %s", at, got, want)
	}
	if got := get(ended(interactions.RoleInbound, interactions.TrustPublic, interactions.StateFailed, at), stored.Options{Retention: r30}); got != "2026-10-09T00:00:00Z" {
		t.Errorf("a public call: %v, want 7 days", got)
	}
	if got := get(ended(interactions.RoleOutbound, "", interactions.StateCompleted, at), stored.Options{Retention: forever}); got != nil {
		t.Errorf("forever: %v, want none", got)
	}
	if got := get(ended(interactions.RoleInbound, interactions.TrustPublic, interactions.StateCompleted, at), stored.Options{Retention: forever}); got != "2026-10-09T00:00:00Z" {
		t.Errorf("forever but public: %v", got)
	}
	if got := get(ended(interactions.RoleOutbound, "", interactions.StateWorking, at), stored.Options{Retention: r30}); got != nil {
		t.Errorf("a task that has not ended: %v", got)
	}
	if got := get(ended(interactions.RoleOutbound, "", interactions.StateCompleted, at), stored.Options{Retention: r30, ForBackend: true}); got != nil {
		t.Errorf("a backend's view: %v", got)
	}
	if got := get(ended(interactions.RoleOutbound, "", interactions.StateCompleted, at), stored.Options{}); got != nil {
		t.Errorf("no setting: %v", got)
	}
}

// TestKeptLine (UX-C2 line 4): the retention in the sentence is the one
// that applies to the task.
func TestKeptLine(t *testing.T) {
	t.Setenv("LANG", "C")
	at := time.Date(2026, 10, 1, 15, 30, 0, 0, time.UTC)
	r30, _ := interactions.ParseRetention("30d")
	for _, c := range []struct {
		ix   *interactions.Interaction
		days string
	}{
		{ended(interactions.RoleOutbound, "", interactions.StateCompleted, at), "30 days"},
		{ended(interactions.RoleInbound, interactions.TrustPublicCap, interactions.StateCompleted, at), "7 days"},
	} {
		task := stored.Project(stored.Source{Interaction: c.ix}, stored.Options{Retention: r30})
		line, ok := a2ashape.KeptLine(task, a2ashape.English, at)
		if !ok || !strings.Contains(line, "retention "+c.days) || !strings.HasPrefix(line, "Content kept until ") {
			t.Errorf("%s: %q %v", c.ix.Trust, line, ok)
		}
	}
	if _, ok := a2ashape.KeptLine(stored.Project(stored.Source{Interaction: ended(interactions.RoleOutbound, "", interactions.StateWorking, at)},
		stored.Options{Retention: r30}), a2ashape.English, at); ok {
		t.Error("a task that has not ended has a kept line")
	}
}

// TestPeerVariantSelection (FS-U6): the .peer sentence when the reason came
// with a message the other side sent, the plain one when this node's own
// state wrote it; the next step is the emergency rotation for the plain
// local_state_lost only.
func TestPeerVariantSelection(t *testing.T) {
	t.Setenv("LANG", "C")
	at := time.Date(2026, 10, 1, 15, 30, 0, 0, time.UTC)
	for _, reason := range []string{"local_state_lost", "key_compromise"} {
		ix := ended(interactions.RoleOutbound, "", interactions.StateFailed, at)
		ix.ResultMeta = `{"anet.reason":"` + reason + `"}`
		own := stored.Project(stored.Source{Interaction: ix}, stored.Options{})
		if got, want := own.Status.Message.Parts[0].Text, a2ashape.Sentence(reason, a2ashape.English, nil); got != want {
			t.Errorf("%s own: %q, want %q", reason, got, want)
		}
		if own.Metadata[a2ashape.KeyReason] != reason {
			t.Errorf("%s own: anet.reason %v", reason, own.Metadata[a2ashape.KeyReason])
		}
		ix2 := ended(interactions.RoleOutbound, "", interactions.StateFailed, at)
		msgs := []interactions.Message{{Seq: 1, SenderAID: notePeer, Kind: interactions.MsgStatus, Body: "peer words",
			Metadata: `{"anet.reason":"` + reason + `"}`}}
		peer := stored.Project(stored.Source{Interaction: ix2, Messages: msgs}, stored.Options{})
		if got, want := peer.Status.Message.Parts[0].Text, a2ashape.Sentence(reason+".peer", a2ashape.English, nil); got != want {
			t.Errorf("%s peer: %q, want %q", reason, got, want)
		}
		if peer.Metadata[a2ashape.KeyReason] != reason {
			t.Errorf("%s peer: anet.reason %v", reason, peer.Metadata[a2ashape.KeyReason])
		}
		_, hasNext := peer.Metadata[a2ashape.KeyNext]
		next, _ := own.Metadata[a2ashape.KeyNext].(string)
		if hasNext || (reason == "local_state_lost") != (next == "anet identity rotate --emergency") {
			t.Errorf("%s: next own %q, peer has one: %v", reason, next, hasNext)
		}
	}
}

// TestNoLocalTaskIsATaskNotFound (FS-U5): the refusal is a TaskNotFound for
// every caller that asks, and carries the audit command.
func TestNoLocalTaskIsATaskNotFound(t *testing.T) {
	ref := a2ashape.Refuse("no_local_task", a2ashape.Args{"task": "ix_a1", "retention": "30d"})
	if !errors.Is(ref, a2ashape.ErrTaskNotFound) || errors.Is(ref, a2ashape.ErrInvalidParams) {
		t.Fatal("no_local_task is not exactly a TaskNotFoundError")
	}
	body := ref.Body(a2ashape.English)
	if body.Reason != "no_local_task" || body.Next != "anet audit --interaction ix_a1" || !strings.Contains(body.Error, "30 days") {
		t.Fatalf("the body %+v", body)
	}
	if e := ref.A2A(a2ashape.English); e.Name != "TaskNotFoundError" || e.HTTPStatus != 404 || e.Message != body.Error {
		t.Fatalf("the A2A error %+v", e)
	}
}
