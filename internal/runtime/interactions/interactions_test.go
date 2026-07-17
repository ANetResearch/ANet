package interactions_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

func open(t *testing.T) *interactions.Store {
	t.Helper()
	s, err := interactions.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// An inbound task is stored queued, then transitions to done carrying the deliverable + receipt; List
// with a status filter reflects the transition.
func TestInboundLifecycle(t *testing.T) {
	s := open(t)
	if err := s.Put("ix_1", interactions.RoleInbound, "did:anet:requester", "bake bread", "cid_req", []byte("TASKDOC")); err != nil {
		t.Fatalf("put: %v", err)
	}
	pending, err := s.List(interactions.RoleInbound, interactions.StatusQueued, 0, 0)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d (%v)", len(pending), err)
	}
	if err := s.SetResult("ix_1", []byte("DELIVERABLE"), "cid_res", []byte("RECEIPT")); err != nil {
		t.Fatalf("set result: %v", err)
	}
	if got, _ := s.List(interactions.RoleInbound, interactions.StatusQueued, 0, 0); len(got) != 0 {
		t.Fatalf("still %d queued after done", len(got))
	}
	ix, err := s.Get("ix_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ix.Status != interactions.StatusDone || string(ix.Result) != "DELIVERABLE" || string(ix.Receipt) != "RECEIPT" || ix.ResultCID != "cid_res" {
		t.Fatalf("bad done state: %+v", ix)
	}
	if string(ix.RequestDoc) != "TASKDOC" {
		t.Fatalf("request doc not persisted: %q", ix.RequestDoc)
	}
}

// Put is idempotent on id: a retried delegate does not duplicate or clobber the record.
func TestPutIdempotent(t *testing.T) {
	s := open(t)
	_ = s.Put("ix_dup", interactions.RoleOutbound, "did:anet:p", "goal one", "", nil)
	_ = s.Put("ix_dup", interactions.RoleOutbound, "did:anet:p", "goal two", "", nil)
	all, _ := s.List(interactions.RoleOutbound, "", 0, 0)
	if len(all) != 1 {
		t.Fatalf("want 1 row, got %d", len(all))
	}
	if all[0].Goal != "goal one" {
		t.Fatalf("second Put clobbered the record: %q", all[0].Goal)
	}
}

// Inbound and outbound rows are isolated by role, and an unknown id is a clean ErrNotFound.
func TestRoleIsolationAndNotFound(t *testing.T) {
	s := open(t)
	_ = s.Put("in", interactions.RoleInbound, "did:anet:r", "g", "", nil)
	_ = s.Put("out", interactions.RoleOutbound, "did:anet:p", "g", "", nil)
	in, _ := s.List(interactions.RoleInbound, "", 0, 0)
	out, _ := s.List(interactions.RoleOutbound, "", 0, 0)
	if len(in) != 1 || len(out) != 1 || in[0].ID != "in" || out[0].ID != "out" {
		t.Fatalf("role isolation broken: in=%v out=%v", in, out)
	}
	if _, err := s.Get("nope"); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
