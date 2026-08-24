package inv1_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/adp"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANet/module/inv1"
	"github.com/ANetResearch/ANet/module/org"
)

// The runtime INV-1 guard rejects org-scoped objects on a commons publish path — by value, by pointer,
// and inside a slice — while public commons types (a gossip card, a TaskDoc) pass.
func TestGuardCommonsPublish(t *testing.T) {
	orgScoped := []any{
		blackboard.CogUnit{},
		&blackboard.CogUnit{},
		// org.Credential joins this list with the org module; the guard's
		// reach into slices is what matters and CogUnit exercises it.
		[]*blackboard.CogUnit{{}},
	}
	for _, v := range orgScoped {
		if err := inv1.GuardCommonsPublish(v); !errors.Is(err, inv1.ErrOrgScopedOnCommons) {
			t.Errorf("GuardCommonsPublish(%T) = %v, want ErrOrgScopedOnCommons", v, err)
		}
	}

	public := []any{
		nil,
		&adp.GossipCardMessage{},
		adp.GossipCardMessage{},
		&tsir.TaskDoc{},
		[]byte("encoded record"),
		"a plain string",
	}
	for _, v := range public {
		if err := inv1.GuardCommonsPublish(v); err != nil {
			t.Errorf("GuardCommonsPublish(%T) = %v, want nil (public/neutral type)", v, err)
		}
	}
}

// An org-scoped object hidden behind an interface must be caught.
//
// The guard walked static types only, and the boundary it now defends
// publishes map[string]any. The walk therefore reached `interface{}`,
// found no marker on it, and passed — so attaching the guard to the one
// real publish path in this repository would have proved nothing about
// that path. The limit was documented and that did not make it safe.
func TestAnOrgObjectHiddenInAnInterfaceIsCaught(t *testing.T) {
	cred := org.Credential{OrgID: "org-1", Subject: "did:anet:x", Role: org.RoleMember}
	unit := &blackboard.CogUnit{TaskID: "t", Type: "claim"}

	// The shape everything published here actually has.
	for name, body := range map[string]any{
		"top level":    map[string]any{"aid": "did:anet:me", "extra": cred},
		"pointer":      map[string]any{"aid": "did:anet:me", "unit": unit},
		"inside slice": map[string]any{"caps": []any{"work.do", cred}},
		"nested map":   map[string]any{"card": map[string]any{"claims": cred}},
		"as a map key": map[string]any{"index": map[any]bool{cred: true}},
		"two deep":     map[string]any{"a": map[string]any{"b": []any{unit}}},
	} {
		if err := inv1.GuardCommonsPublish(body); !errors.Is(err, inv1.ErrOrgScopedOnCommons) {
			t.Errorf("%s: an org-scoped object reached a commons publish: %v", name, err)
		}
	}

	// And an ordinary publication still goes out. A guard that refuses
	// everything protects nothing and gets switched off.
	ok := map[string]any{
		"aid": "did:anet:me", "name": "node", "caps": []any{"text.stats"},
		"card": map[string]any{"seq": 1, "sig": []byte{1, 2, 3}},
	}
	if err := inv1.GuardCommonsPublish(ok); err != nil {
		t.Errorf("an ordinary registration was refused: %v", err)
	}
}

// A structure that points at itself must not hang the guard.
//
// The value walk follows pointers, and a publication body is not
// guaranteed to be a tree.
func TestTheValueWalkTerminatesOnACycle(t *testing.T) {
	type node struct {
		Name string
		Next *node
	}
	a := &node{Name: "a"}
	a.Next = a
	done := make(chan error, 1)
	go func() { done <- inv1.GuardCommonsPublish(map[string]any{"n": a}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a plain cyclic structure was refused: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guard did not terminate on a self-referential value")
	}
}
