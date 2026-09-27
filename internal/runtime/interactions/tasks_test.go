package interactions_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// GetFor finds a row only under its own role and peer; every mismatch is
// the same ErrNotFound as an absent id (A2A-DESIGN §11.1 [C17]).
func TestGetForComparesRoleAndPeer(t *testing.T) {
	s := open(t)
	if err := s.Create(interactions.New{ID: "ix_a", Role: interactions.RoleOutbound, PeerAID: "peer-a", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(interactions.New{ID: "ix_in", Role: interactions.RoleInbound, PeerAID: "peer-a", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	if ix, err := s.GetFor("ix_a", interactions.RoleOutbound, "peer-a"); err != nil || ix.ID != "ix_a" {
		t.Fatalf("own task: %v %v", ix, err)
	}
	for _, c := range []struct {
		id   string
		role interactions.Role
		peer string
	}{
		{"ix_a", interactions.RoleOutbound, "peer-b"},  // another agent's task
		{"ix_in", interactions.RoleOutbound, "peer-a"}, // an inbound task
		{"ix_none", interactions.RoleOutbound, "peer-a"},
		{"ix_a", interactions.RoleOutbound, ""},
	} {
		if _, err := s.GetFor(c.id, c.role, c.peer); !errors.Is(err, interactions.ErrNotFound) {
			t.Errorf("GetFor(%s, %s, %q) = %v, want ErrNotFound", c.id, c.role, c.peer, err)
		}
	}
}

// A client message id is found in its context, for its peer and task, and
// a message whose metadata is not JSON does not break the query.
func TestFindByClientMessage(t *testing.T) {
	s := open(t)
	mk := func(id, peer, ctx string) {
		t.Helper()
		if err := s.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: peer, Goal: "g", ContextID: ctx}); err != nil {
			t.Fatal(err)
		}
	}
	mk("ix_1", "peer-a", "ctx-1")
	mk("ix_2", "peer-a", "ctx-1")
	mk("ix_3", "peer-b", "ctx-2")
	add := func(ix, meta string) {
		t.Helper()
		if _, _, err := s.AddMessageRecord(interactions.MessageRecord{InteractionID: ix, SenderAID: "me",
			Kind: interactions.MsgText, Body: "b", Metadata: []byte(meta)}); err != nil {
			t.Fatal(err)
		}
	}
	add("ix_1", "")
	add("ix_1", "not json")
	add("ix_2", `{"a2a.messageId":"m-1","other":1}`)
	add("ix_3", `{"a2a.messageId":"m-1"}`)

	q := interactions.ClientMessageQuery{Role: interactions.RoleOutbound, ContextID: "ctx-1", ClientMsgID: "m-1"}
	if ix, err := s.FindByClientMessage(q); err != nil || ix.ID != "ix_2" {
		t.Fatalf("find = %v %v, want ix_2", ix, err)
	}
	q.PeerAID = "peer-a"
	if ix, err := s.FindByClientMessage(q); err != nil || ix.ID != "ix_2" {
		t.Fatalf("find with peer = %v %v", ix, err)
	}
	// No context: the peer's tasks in every context (a client that gave no
	// contextId retries without one).
	if ix, err := s.FindByClientMessage(interactions.ClientMessageQuery{Role: interactions.RoleOutbound,
		PeerAID: "peer-b", ClientMsgID: "m-1"}); err != nil || ix.ID != "ix_3" {
		t.Fatalf("find by peer = %v %v, want ix_3", ix, err)
	}
	for name, miss := range map[string]interactions.ClientMessageQuery{
		"other peer":             {Role: interactions.RoleOutbound, ContextID: "ctx-1", PeerAID: "peer-b", ClientMsgID: "m-1"},
		"other task":             {Role: interactions.RoleOutbound, ContextID: "ctx-1", TaskID: "ix_1", ClientMsgID: "m-1"},
		"other id":               {Role: interactions.RoleOutbound, ContextID: "ctx-1", ClientMsgID: "m-2"},
		"other context":          {Role: interactions.RoleOutbound, ContextID: "ctx-9", ClientMsgID: "m-1"},
		"inbound":                {Role: interactions.RoleInbound, ContextID: "ctx-1", ClientMsgID: "m-1"},
		"no scope":               {Role: interactions.RoleOutbound, ClientMsgID: "m-1"},
		"no context, other peer": {Role: interactions.RoleOutbound, PeerAID: "peer-c", ClientMsgID: "m-1"},
	} {
		if _, err := s.FindByClientMessage(miss); !errors.Is(err, interactions.ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
}

func TestContextPeersAndMergeMessageMeta(t *testing.T) {
	s := open(t)
	for _, n := range []interactions.New{
		{ID: "ix_1", Role: interactions.RoleOutbound, PeerAID: "peer-a", Goal: "g", ContextID: "ctx"},
		{ID: "ix_2", Role: interactions.RoleOutbound, PeerAID: "peer-a", Goal: "g", ContextID: "ctx"},
		{ID: "ix_3", Role: interactions.RoleInbound, PeerAID: "peer-b", Goal: "g", ContextID: "ctx"},
	} {
		if err := s.Create(n); err != nil {
			t.Fatal(err)
		}
	}
	peers, err := s.ContextPeers(interactions.RoleOutbound, "ctx")
	if err != nil || len(peers) != 1 || peers[0] != "peer-a" {
		t.Fatalf("outbound peers of ctx = %v %v", peers, err)
	}
	if peers, _ := s.ContextPeers(interactions.RoleOutbound, "unknown"); len(peers) != 0 {
		t.Fatalf("unknown context has peers %v", peers)
	}
	seq, _, err := s.AddMessageRecord(interactions.MessageRecord{InteractionID: "ix_1", SenderAID: "me",
		Kind: interactions.MsgText, Body: "b", Metadata: []byte(`{"k":"v"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MergeMessageMeta("ix_1", seq, map[string]any{interactions.ClientMessageIDKey: "m-1"}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := s.Messages("ix_1")
	if msgs[0].Metadata != `{"a2a.messageId":"m-1","k":"v"}` {
		t.Fatalf("merged metadata = %s", msgs[0].Metadata)
	}
	// A nil value removes its key; removing an absent key changes nothing.
	if err := s.MergeMessageMeta("ix_1", seq, map[string]any{interactions.ClientMessageIDKey: nil, "gone": nil}); err != nil {
		t.Fatal(err)
	}
	msgs, _ = s.Messages("ix_1")
	if msgs[0].Metadata != `{"k":"v"}` {
		t.Fatalf("metadata after removal = %s", msgs[0].Metadata)
	}
	if err := s.MergeMessageMeta("ix_2", seq, map[string]any{"x": 1}); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatalf("merge into a message of another interaction: %v", err)
	}
}
