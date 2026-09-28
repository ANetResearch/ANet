package interactions_test

import (
	"errors"
	"fmt"
	"strings"
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

// A listing reads the covering index idx_ix_list — which carries every column a
// listing filters or orders on — and fetches only the rows of its page. With the
// filters of the local A2A interface (role and peer), of the control plane and of
// the inbound feed, SQLite otherwise picked the role or peer index and sorted the
// full rows: every goal, request_doc and result of every matching interaction read
// for each call. After an hour of real-client traffic with a few 3 MiB messages
// that was 13–33 s per ListTasks (docs/notes/0035).
func TestListingReadsTheCoveringIndex(t *testing.T) {
	s := open(t)
	for i, f := range []interactions.ListFilter{
		{Role: interactions.RoleOutbound, PeerAID: "peer-a"},
		{Role: interactions.RoleOutbound, PeerAID: "peer-a", States: []interactions.State{interactions.StateCompleted}},
		{Role: interactions.RoleInbound},
		{Role: interactions.RoleInbound, Active: true, ExcludeCapability: true, ExcludeTrust: []string{"public_cap"}},
		{PeerAID: "peer-a", UpdatedAfter: 5, Cursor: "10.3"},
		{},
	} {
		for what, explain := range map[string]func() ([]string, error){
			"list":  func() ([]string, error) { return s.ExplainList(f, 50) },
			"count": func() ([]string, error) { return s.ExplainCount(f) },
		} {
			plan, err := explain()
			if err != nil {
				t.Fatalf("filter %d %s: %v", i, what, err)
			}
			p := strings.Join(plan, "; ")
			if !strings.Contains(p, "COVERING INDEX") || strings.Contains(p, "TEMP B-TREE") {
				t.Errorf("filter %d %s: plan %q, want a covering index and no sort", i, what, p)
			}
		}
	}
}

// A listing of one context reads idx_ix_context: a context holds a few tasks.
// Left to itself SQLite took the peer index for the role and peer that the
// local A2A interface always adds, and read the context of every task with
// the peer from its row — past the long columns (docs/notes/0035: 4 s for a
// ListTasks by contextId after a 30-minute soak).
func TestContextListingReadsTheContextIndex(t *testing.T) {
	s := open(t)
	for i, f := range []interactions.ListFilter{
		{ContextID: "c"},
		{Role: interactions.RoleOutbound, PeerAID: "peer-a", ContextID: "c"},
		{Role: interactions.RoleOutbound, PeerAID: "peer-a", ContextID: "c", States: []interactions.State{interactions.StateCompleted}},
	} {
		for what, explain := range map[string]func() ([]string, error){
			"list":  func() ([]string, error) { return s.ExplainList(f, 50) },
			"count": func() ([]string, error) { return s.ExplainCount(f) },
		} {
			plan, err := explain()
			if err != nil {
				t.Fatalf("filter %d %s: %v", i, what, err)
			}
			if p := strings.Join(plan, "; "); !strings.Contains(p, "idx_ix_context") {
				t.Errorf("filter %d %s: plan %q, want idx_ix_context", i, what, p)
			}
		}
	}
}

// ContextPeers, asked on every SendMessage that names a context, reads the
// context index, not every row of the role.
func TestContextPeersReadsTheContextIndex(t *testing.T) {
	s := open(t)
	plan, err := s.ExplainContextPeers()
	if err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(plan, "; "); !strings.Contains(p, "idx_ix_context") {
		t.Fatalf("plan %q, want idx_ix_context", p)
	}
	if err := s.Create(interactions.New{ID: "ix_1", Role: interactions.RoleOutbound, PeerAID: "peer-a", Goal: "g",
		ContextID: "c"}); err != nil {
		t.Fatal(err)
	}
	if peers, err := s.ContextPeers(interactions.RoleOutbound, "c"); err != nil || len(peers) != 1 || peers[0] != "peer-a" {
		t.Fatalf("ContextPeers = %v %v", peers, err)
	}
}

// The two-step listing (ids from the index, then the rows) keeps ListPage's
// order, page boundaries and whole rows.
func TestListPageTwoStepKeepsOrderAndRows(t *testing.T) {
	s := open(t)
	big := strings.Repeat("g", 1<<20)
	for i := 0; i < 7; i++ {
		goal := "small"
		if i%3 == 0 {
			goal = big
		}
		peer := "peer-a"
		if i == 4 {
			peer = "peer-b"
		}
		if err := s.Create(interactions.New{ID: fmt.Sprintf("ix_%d", i), Role: interactions.RoleOutbound, PeerAID: peer,
			Goal: goal, ContextID: "c"}); err != nil {
			t.Fatal(err)
		}
	}
	f := interactions.ListFilter{Role: interactions.RoleOutbound, PeerAID: "peer-a", Limit: 4}
	var got []string
	for {
		p, err := s.ListPage(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, ix := range p.Items {
			got = append(got, ix.ID)
			if (ix.ID == "ix_0" || ix.ID == "ix_3" || ix.ID == "ix_6") != (ix.Goal == big) {
				t.Fatalf("%s: goal of %d bytes", ix.ID, len(ix.Goal))
			}
		}
		if p.Next == "" {
			break
		}
		f.Cursor = p.Next
	}
	if strings.Join(got, " ") != "ix_6 ix_5 ix_3 ix_2 ix_1 ix_0" {
		t.Fatalf("pages gave %v", got)
	}
	if n, err := s.Count(interactions.ListFilter{Role: interactions.RoleOutbound, PeerAID: "peer-a"}); err != nil || n != 6 {
		t.Fatalf("count %d %v", n, err)
	}
	if p, err := s.ListPage(interactions.ListFilter{ContextID: "c", Limit: 10}); err != nil || len(p.Items) != 7 {
		t.Fatalf("by context: %d %v", len(p.Items), err)
	}
}

// The client-retry lookup (FindByClientMessage) finds the message by an index
// on its client message id. Without one, a SendMessage that names no context
// read the metadata of every message of every task with the peer — a column
// SQLite reaches only through the overflow pages of the body before it, the
// whole of every long message each time (docs/notes/0035: 0.15–0.3 s per send
// after an hour, growing with the conversation).
func TestClientMessageLookupReadsAnIndex(t *testing.T) {
	s := open(t)
	for _, q := range []interactions.ClientMessageQuery{
		{ClientMsgID: "m-1", Role: interactions.RoleOutbound, PeerAID: "peer-a"},
		{ClientMsgID: "m-1", Role: interactions.RoleOutbound, PeerAID: "peer-a", OpenOnly: true},
		{ClientMsgID: "m-1", Role: interactions.RoleOutbound, ContextID: "c"},
	} {
		plan, err := s.ExplainFindByClientMessage(q)
		if err != nil {
			t.Fatal(err)
		}
		if p := strings.Join(plan, "; "); !strings.Contains(p, "idx_msg_client") {
			t.Errorf("%+v: plan %q, want the message lookup through idx_msg_client", q, p)
		}
	}
}
