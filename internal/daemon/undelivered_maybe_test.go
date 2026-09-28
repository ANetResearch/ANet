package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// deliveredThenFailed is a direct transport whose round trip delivers the
// envelope to the recipient's pipeline and then reports failure — a p2p
// send whose answer timed out while the far side was still at work, which
// module.Transport requires be reported as a failure. With notDelivered
// set it reports the failure as one that reached nobody, and delivers
// nothing.
type deliveredThenFailed struct {
	to           *Daemon
	notDelivered bool
	calls        atomic.Int32
}

func (p *deliveredThenFailed) Name() string                           { return "p2p-answer-lost" }
func (p *deliveredThenFailed) Reachable(context.Context, string) bool { return true }
func (p *deliveredThenFailed) Send(ctx context.Context, _ string, env []byte) error {
	p.calls.Add(1)
	if p.notDelivered {
		return errors.New("p2p: no route: " + module.ErrNotDelivered.Error())
	}
	_ = p.to.receiveEnvelope(ctx, env)
	return errors.New("p2p: no answer within the send timeout")
}

// taskViewMap is the projection every door returns (control plane, MCP,
// module/a2a), decoded.
func taskViewMap(t *testing.T, d *Daemon, id string) (state, effectStatus, reason string) {
	t.Helper()
	ix, err := d.ix.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.taskView(ix, viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(task)
	var m struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	es, _ := m.Metadata[a2ashape.KeyEffectStatus].(string)
	r, _ := m.Metadata[a2ashape.KeyReason].(string)
	return m.Status.State, es, r
}

// [redteam:F12] SI-6: a delegation a direct transport delivered (its Send
// then failed, as a timed-out round trip must) and the hub then refused for
// good (413: only p2p could carry it) is abandoned — but it is not reported
// as "never delivered, did not run" (UNAVAILABLE): the provider ran it. The
// task fails with effect UNVERIFIED and the caller is told delivery was not
// confirmed.
func TestAnAbandonedDelegationThatMayHaveArrivedIsNotUnavailable(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	lamp := &lampProvider{}
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.maxEnvelope = 1 }) // the hub refuses the envelope for good (413)
	p2p := &deliveredThenFailed{to: prov}
	req.RegisterTransport(p2p)

	_, derr := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	list, err := req.ix.List(interactions.RoleOutbound, "", 0, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("outbound tasks: %v %d", err, len(list))
	}
	id := list[0].ID
	if len(lamp.invoked) != 1 || p2p.calls.Load() != 1 {
		t.Fatalf("setup: the provider ran the call %d times (p2p sends %d)", len(lamp.invoked), p2p.calls.Load())
	}
	state, es, reason := taskViewMap(t, req, id)
	if state != string(a2ashape.TaskStateFailed) || es != "UNVERIFIED" || reason != a2ashape.ReasonUndeliverable {
		t.Fatalf("requester: state=%s effect_status=%s reason=%s; want failed, UNVERIFIED, undeliverable", state, es, reason)
	}
	if derr == nil || !strings.Contains(derr.Error(), "not confirmed") {
		t.Fatalf("DelegateCapability = %v, want an error saying delivery was not confirmed", derr)
	}
}

// The same refusal when no attempt can have delivered it (the direct path
// said it reached nobody) is still reported as a call that did not run:
// UNAVAILABLE ([redteam:F12], the other side of the line).
func TestAnAbandonedDelegationThatReachedNobodyIsUnavailable(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Prov", nil, ""); err != nil {
		t.Fatal(err)
	}
	setFake(t, srv.URL, func(h *fakeHub) { h.maxEnvelope = 1 })
	req.RegisterTransport(&deliveredThenFailedNot{})
	_, derr := req.DelegateCapability(ctx, prov.AID(), lampCap, map[string]any{"on": true})
	list, err := req.ix.List(interactions.RoleOutbound, "", 0, 10)
	if err != nil || len(list) != 1 {
		t.Fatalf("outbound tasks: %v %d", err, len(list))
	}
	state, es, _ := taskViewMap(t, req, list[0].ID)
	if state != string(a2ashape.TaskStateFailed) || es != "UNAVAILABLE" {
		t.Fatalf("requester: state=%s effect_status=%s; want failed, UNAVAILABLE", state, es)
	}
	if derr == nil || !strings.Contains(derr.Error(), "not delivered") {
		t.Fatalf("DelegateCapability = %v", derr)
	}
}

// deliveredThenFailedNot is a direct transport that fails knowing it
// reached nobody (module.ErrNotDelivered).
type deliveredThenFailedNot struct{}

func (deliveredThenFailedNot) Name() string                           { return "p2p-unreachable" }
func (deliveredThenFailedNot) Reachable(context.Context, string) bool { return true }
func (deliveredThenFailedNot) Send(context.Context, string, []byte) error {
	return errors.Join(errors.New("p2p: dial failed"), module.ErrNotDelivered)
}
