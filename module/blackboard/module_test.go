//go:build !no_blackboard

package blackboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// testHost is a module.Host that resolves only the authors it was given.
type testHost struct {
	aid  string
	reg  *provider.Registry
	kels map[string][]identity.SignedEvent
}

func (h *testHost) AID() string                      { return h.aid }
func (h *testHost) Providers() *provider.Registry    { return h.reg }
func (h *testHost) RecordEvidence(string, any) error { return nil }
func (h *testHost) ResolveKEL(aid string) ([]identity.SignedEvent, bool) {
	k, ok := h.kels[aid]
	return k, ok
}

func startBoard(t *testing.T, kels map[string][]identity.SignedEvent) (*Module, *testHost) {
	t.Helper()
	h := &testHost{aid: "node-1", reg: provider.NewRegistry(), kels: kels}
	m := &Module{}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return m, h
}

// The board reaches the network as capabilities and nothing else — no new
// path through the daemon, and no second notion of what a peer is.
func TestBoardIsOfferedThroughC1(t *testing.T) {
	_, h := startBoard(t, nil)

	for _, capID := range []string{CapAdd, CapSnapshot, CapConclude} {
		if _, ok := h.reg.Resolve(capID); !ok {
			t.Errorf("capability %q must be resolvable", capID)
		}
	}
}

// A contribution whose author this node cannot vouch for is refused.
//
// This is the line that keeps a shared board from becoming a place anyone
// can write anything: the board verifies, it does not sign, and an author
// it cannot resolve is not an author it can accept.
func TestContributionFromAnUnknownAuthorIsRefused(t *testing.T) {
	m, h := startBoard(t, nil) // resolves nobody

	ctrl := newController(t)
	unit := signedUnit(t, ctrl, "task-1", []byte("thinking"))
	raw, err := unit.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	p, _ := h.reg.Resolve(CapAdd)
	eff, err := p.Invoke(context.Background(), provider.Call{
		Capability: CapAdd,
		Args:       map[string]any{"unit": base64.StdEncoding.EncodeToString(raw)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.Failed {
		t.Fatalf("status = %v, want FAILED for an unresolvable author", eff.Status)
	}
	if !strings.Contains(eff.Message, "author") {
		t.Errorf("the refusal should say what was wrong: %q", eff.Message)
	}
	if m.Board().Len() != 0 {
		t.Fatal("a refused contribution must not be on the board")
	}
}

// A contribution from an author this node has verified is merged, and the
// effect says so with evidence rather than a bare success.
func TestContributionFromAKnownAuthorIsMerged(t *testing.T) {
	ctrl := newController(t)
	aid := ctrl.AID()
	m, h := startBoard(t, map[string][]identity.SignedEvent{aid: ctrl.KEL()})

	unit := signedUnit(t, ctrl, "task-1", []byte("thinking"))
	raw, _ := unit.Marshal()
	p, _ := h.reg.Resolve(CapAdd)

	eff, err := p.Invoke(context.Background(), provider.Call{
		Capability: CapAdd,
		Args:       map[string]any{"unit": base64.StdEncoding.EncodeToString(raw)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("status = %v (%s), want OK", eff.Status, eff.Message)
	}
	if eff.Record == nil || eff.Record.Metrics["added"] != 1 {
		t.Fatalf("the effect must report the merge: %+v", eff.Record)
	}
	// Reading its own state back after merging is an independent
	// confirmation, not the caller's word for it.
	if eff.Evidence == nil || eff.Evidence.VerifyTrust != 3 {
		t.Fatalf("a readback of the board is V3 evidence, got %+v", eff.Evidence)
	}
	if m.Board().Len() != 1 {
		t.Fatalf("board holds %d units, want 1", m.Board().Len())
	}

	// Re-adding is idempotent: the CRDT is add-wins and order-independent,
	// so a peer that retries does not double-count.
	eff2, err := p.Invoke(context.Background(), provider.Call{
		Capability: CapAdd,
		Args:       map[string]any{"unit": base64.StdEncoding.EncodeToString(raw)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if eff2.Record.Metrics["added"] != 0 {
		t.Errorf("a repeated contribution must report added=0, got %v", eff2.Record.Metrics)
	}
	if m.Board().Len() != 1 {
		t.Errorf("a repeated contribution must not grow the board")
	}
}

// A snapshot returns what is on the board, in causal order, as the units
// themselves — so a caller can verify every one of them independently.
func TestSnapshotReturnsVerifiableUnits(t *testing.T) {
	ctrl := newController(t)
	m, h := startBoard(t, map[string][]identity.SignedEvent{ctrl.AID(): ctrl.KEL()})
	add, _ := h.reg.Resolve(CapAdd)

	for _, body := range [][]byte{[]byte("one"), []byte("two")} {
		raw, _ := signedUnit(t, ctrl, "task-1", body).Marshal()
		if _, err := add.Invoke(context.Background(), provider.Call{
			Capability: CapAdd,
			Args:       map[string]any{"unit": base64.StdEncoding.EncodeToString(raw)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	snap, _ := h.reg.Resolve(CapSnapshot)
	eff, err := snap.Invoke(context.Background(), provider.Call{Capability: CapSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Record.Metrics["units"] != 2 {
		t.Fatalf("snapshot reports %v units, want 2", eff.Record.Metrics["units"])
	}
	var encoded []string
	if err := json.Unmarshal([]byte(eff.Evidence.ObservedState), &encoded); err != nil {
		t.Fatal(err)
	}
	for _, e := range encoded {
		b, err := base64.StdEncoding.DecodeString(e)
		if err != nil {
			t.Fatal(err)
		}
		u, err := Unmarshal(b)
		if err != nil {
			t.Fatalf("a snapshotted unit must decode: %v", err)
		}
		if err := u.Verify(ctrl.KEL(), nowMS()); err != nil {
			t.Fatalf("a snapshotted unit must still verify: %v", err)
		}
	}
	_ = m
}

// Concluding a task freezes its cognition, which is what makes sedimentation
// safe: nothing can be added to a board that has been declared finished.
func TestConcludeFreezesTheTask(t *testing.T) {
	ctrl := newController(t)
	m, h := startBoard(t, map[string][]identity.SignedEvent{ctrl.AID(): ctrl.KEL()})

	concl, _ := h.reg.Resolve(CapConclude)
	eff, err := concl.Invoke(context.Background(), provider.Call{
		Capability: CapConclude, Args: map[string]any{"task_id": "task-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if eff.Status != effect.OK {
		t.Fatalf("conclude failed: %s", eff.Message)
	}

	raw, _ := signedUnit(t, ctrl, "task-1", []byte("late thought")).Marshal()
	add, _ := h.reg.Resolve(CapAdd)
	after, err := add.Invoke(context.Background(), provider.Call{
		Capability: CapAdd,
		Args:       map[string]any{"unit": base64.StdEncoding.EncodeToString(raw)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != effect.Failed {
		t.Fatal("a concluded task must not accept further contributions")
	}
	if m.Board().Len() != 0 {
		t.Fatal("the late contribution must not be on the board")
	}
}

// The module is configurable off without being removed from the config.
func TestDisabledBoardRegistersNothing(t *testing.T) {
	off := false
	h := &testHost{aid: "node-1", reg: provider.NewRegistry()}
	m := &Module{cfg: Config{Enabled: &off}}
	if err := m.Start(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.reg.Resolve(CapAdd); ok {
		t.Fatal("a disabled board must offer no capabilities")
	}
}

// newController mints an identity to author contributions with.
func newController(t *testing.T) *identity.Controller {
	t.Helper()
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// signedUnit builds and signs one contribution. The AUTHOR stamps it before
// signing — the stamp is inside the signed preimage, so a board that
// re-stamped a pulled unit would destroy the causal order its author
// established.
func signedUnit(t *testing.T, ctrl *identity.Controller, taskID string, body []byte) *CogUnit {
	t.Helper()
	u := &CogUnit{
		TaskID: taskID, Scope: "org", Type: "note", Body: body,
		Stamp: HLC{Wall: nowMS(), Logical: 0, NodeID: ctrl.AID()},
	}
	if err := u.Sign(ctrl); err != nil {
		t.Fatal(err)
	}
	return u
}

func nowMS() int64 { return time.Now().UnixMilli() }

var _ module.Host = (*testHost)(nil)
