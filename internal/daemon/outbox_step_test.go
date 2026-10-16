package daemon

// Table cases of the engine's pure decision (outbox_step.go) and of the
// version views it decides on, for the orderings the integration cases
// reach only through other guards.

import (
	"bytes"
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// A1–A6 in their order (engine §2.4): the deadline before anything; a
// status query only of a hub that may be asked, and the only step a closed
// gate or a frozen row allows; nothing handed over while the gate is
// closed, while frozen, or while an earlier row holds it back.
func TestOutboxStepOrder(t *testing.T) {
	const now = 1_000_000
	rid := bytes.Repeat([]byte{1}, hubapi.RIDLen)
	open := GateState{true, IdentityOK, reasonIdentityOK}
	closed := GateState{false, IdentityNoticeStale, reasonNoticeStale}
	fresh := RowView{ID: 2, Exp: now + 1000}
	atHub := RowView{ID: 2, Exp: now + 1000, HandedAt: 5, CheckAt: now - 1}
	queued := []VerView{{Ver: 1, HandedAt: 5, RID: rid, HubState: interactions.OutboxQueued}}
	frozenNever := RowView{ID: 2, Exp: now + 1000, Frozen: true}
	frozenAttempting := RowView{ID: 2, Exp: now + 1000, Frozen: true, Attempting: true}
	for _, c := range []struct {
		name string
		row  RowView
		vers []VerView
		in   StepInput
		want StepAction
	}{
		{"past the deadline, gate closed", RowView{ID: 2, Exp: now - 1}, nil, StepInput{Now: now, Gate: closed}, stepAbandon},
		{"a query due, the hub may be asked", atHub, queued, StepInput{Now: now, Gate: open, HubOK: true}, stepQuery},
		{"a query due, gate closed", atHub, queued, StepInput{Now: now, Gate: closed, HubOK: true}, stepQuery},
		{"a query due, the hub may not be asked", atHub, queued, StepInput{Now: now, Gate: open}, stepWait},
		{"never handed, gate closed", fresh, nil, StepInput{Now: now, Gate: closed, HubOK: true, P2P: true}, stepWait},
		{"never handed, gate open", fresh, nil, StepInput{Now: now, Gate: open, HubOK: true}, stepTransmit},
		{"never handed, an earlier row out first", fresh, nil, StepInput{Now: now, Gate: open, HubOK: true, OrderBlocked: true}, stepBlocked},
		{"frozen, never handed", frozenNever, nil, StepInput{Now: now, Gate: open, HubOK: true}, stepWithdraw},
		{"frozen, an attempt under way", frozenAttempting, nil, StepInput{Now: now, Gate: open, HubOK: true, P2P: true}, stepWait},
		{"no path open", fresh, nil, StepInput{Now: now, Gate: open}, stepWait},
	} {
		if got := outboxStep(c.row, c.vers, c.in); got.Action != c.want {
			t.Errorf("%s: action %d, want %d", c.name, got.Action, c.want)
		}
	}
}

// ENG-D5: a version the hub took counts as unknown — not asked about, and
// to be handed over again — once its hub is no longer this node's, or its
// status queries have failed for an hour.
func TestAVersionAtAnotherHubOrUnansweredForAnHourIsUnknown(t *testing.T) {
	const hub, other = "https://hub.example", "https://old-hub.example"
	rid := func(b byte) []byte { return bytes.Repeat([]byte{b}, hubapi.RIDLen) }
	var d Daemon
	const now = 10 * statusStaleMS
	stored := []interactions.OutboxVer{
		{Ver: 1, HandedAt: 5, RID: rid(1), Hub: other, HubState: interactions.OutboxQueued},
		{Ver: 2, HandedAt: 6, RID: rid(2), Hub: hub, HubState: interactions.OutboxQueued},
	}
	views := d.verViews(9, stored, hub, now)
	if !views[0].Stale || views[1].Stale {
		t.Fatalf("stale: %v %v, want the version at the other hub alone", views[0].Stale, views[1].Stale)
	}
	if g := statusGroups(views, stored); len(g) != 1 || len(g[0].vers) != 1 || g[0].vers[0].Ver != 2 {
		t.Fatalf("status requests %+v, want one about version 2 alone", g)
	}
	row := RowView{ID: 9, Exp: now + 1000, HandedAt: 5}
	if !needsTransmit(row, d.verViews(9, stored[:1], hub, now)) {
		t.Fatal("a version queued at a hub no longer this node's holds the row back")
	}
	if needsTransmit(row, views) {
		t.Fatal("a version this node's hub holds queued does not hold the row back")
	}
	// The hub has not answered about version 2 for an hour.
	d.outboxMem.noteQuery(9, 2, "", true, now-statusStaleMS+1)
	if v := d.verViews(9, stored, hub, now); v[1].Stale {
		t.Fatal("a version stale before its queries failed for an hour")
	}
	if v := d.verViews(9, stored, hub, now+1); !v[1].Stale || !needsTransmit(row, v) {
		t.Fatal("a version whose queries failed for an hour still holds the row back")
	}
}
