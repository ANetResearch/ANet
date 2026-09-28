package daemon

// Fuzz target for what a hub answers (docs/notes/0033). The hub is not
// trusted: every answer below is the fuzzer's, for every request.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/module"
)

// fuzzHub answers every request with the current status and body.
type fuzzHub struct {
	mu     sync.Mutex
	status int
	wire   string
	body   []byte
}

func (h *fuzzHub) set(status int, wire string, body []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.wire, h.body = status, wire, body
}

func (h *fuzzHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	status, wire, body := h.status, h.wire, h.body
	h.mu.Unlock()
	if wire != "" {
		w.Header().Set(hubapi.WireVersionHeader, wire)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Operations of FuzzHubAnswers: each reads one kind of hub answer.
const (
	hubOpPoll = iota
	hubOpKeys
	hubOpIdentity
	hubOpAgents
	hubOpAgentsUncarded
	hubOpFind
	hubOpBalance
	hubOpCard
	hubOpRegister
	hubOpCount
)

var fuzzHubStatuses = []int{200, 200, 200, 404, 405, 426, 429, 500, 507, 201, 204, 302}

// FuzzHubAnswers: whatever a hub answers — to a mailbox poll, a key
// lookup, its own identity, the registry, the directory, the balance, a
// card, a registration — the daemon returns without panicking, stores no
// peer key set that does not verify under a KEL that replays to the peer,
// and shows no card or hub statement for an agent whose card it did not
// verify.
func FuzzHubAnswers(f *testing.F) {
	quietLog(f)
	hub := &fuzzHub{status: 200, wire: "2"}
	srv := httptest.NewServer(hub)
	f.Cleanup(srv.Close)
	d := buildTestDaemon(f, srv.URL, nil)
	peer, err := identity.Incept()
	if err != nil {
		f.Fatal(err)
	}
	peerKEL, _ := identity.MarshalKEL(peer.KEL())
	peerKeys := signedKeysFor(f, peer, 0)
	card, err := d.signedCard("fuzz", []string{"text.echo"})
	if err != nil {
		f.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString
	keysAnswer, _ := json.Marshal(hubapi.KeysResponse{KeySet: b64(peerKeys), KEL: b64(peerKEL)})
	idAnswer, _ := json.Marshal(hubapi.HubIdentity{AID: peer.AID(), KEL: b64(peerKEL)})
	reg, _ := json.Marshal(hubapi.A2AAgentList{Agents: []hubapi.A2AAgentEntry{{AID: d.AID(), Card: card,
		CardVerification: "ok", HomeHub: "https://h", ReviewCount: 3, AvgRating: 4.5}, {AID: peer.AID(), Card: card}}})
	seeds := []struct {
		op     uint8
		status uint8
		wire   string
		body   string
	}{
		{hubOpPoll, 0, "2", `{"messages":[{"id":1,"envelope":"AAEC"},{"id":-1,"envelope":"!!"}]}`},
		{hubOpPoll, 0, "2", `{"messages":[]}`},
		{hubOpKeys, 0, "2", string(keysAnswer)},
		{hubOpKeys, 3, "", `{"error":"no key set"}`},
		{hubOpIdentity, 0, "", string(idAnswer)},
		{hubOpAgents, 0, "2", string(reg)},
		{hubOpAgentsUncarded, 0, "2", `{"agents":[{"aid":"` + peer.AID() + `","name":"n","caps":["a"],"summary":"s","avg_rating":1e308}]}`},
		{hubOpFind, 0, "2", `{"agents":[{"aid":"` + peer.AID() + `","name":"x"}]}`},
		{hubOpBalance, 0, "2", `{"balance":5,"entries":[{"amount":-3}]}`},
		{hubOpCard, 0, "2", string(card)},
		{hubOpRegister, 0, "2", `{"aid":"x","status":"registered","card":{"status":"accepted","seq":1}}`},
		{hubOpPoll, 5, "1", `{"error":"upgrade"}`},
	}
	for _, s := range seeds {
		f.Add(s.op, s.status, s.wire, []byte(s.body))
	}
	f.Fuzz(func(t *testing.T, op, status uint8, wire string, body []byte) {
		hub.set(fuzzHubStatuses[int(status)%len(fuzzHubStatuses)], wire, body)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		switch op % hubOpCount {
		case hubOpPoll:
			_ = d.pollOnce(ctx)
		case hubOpKeys:
			ks, err := d.fetchRecipientKeys(ctx, srv.URL, peer.AID(), "")
			if err == nil {
				if ks == nil || ks.set == nil || ks.set.AID != peer.AID() {
					t.Fatalf("a key set for another AID was taken: %+v", ks)
				}
				row, rerr := d.ix.PeerIdentity(peer.AID())
				if rerr != nil {
					t.Fatalf("keys taken but no peer row: %v", rerr)
				}
				kel, kerr := identity.UnmarshalKEL(row.KEL)
				if kerr != nil {
					t.Fatalf("a stored KEL does not decode: %v", kerr)
				}
				if !replaysTo(kel, peer.AID()) {
					t.Fatalf("a KEL stored for %s does not replay to it", peer.AID())
				}
			}
		case hubOpIdentity:
			d.hubIDMu.Lock()
			d.hubIDs = nil
			d.hubIDMu.Unlock()
			aid, kel, err := d.hubIdentity(ctx, srv.URL)
			if err == nil {
				if !replaysTo(kel, aid) {
					t.Fatalf("the hub's identity %s was pinned with a KEL that does not replay to it", aid)
				}
			}
		case hubOpAgents, hubOpAgentsUncarded:
			q := module.AgentQuery{IncludeUncarded: op%hubOpCount == hubOpAgentsUncarded, Query: string(body[:min(len(body), 3)])}
			agents, _, err := d.listAgents(ctx, q)
			if err == nil {
				checkAgents(t, agents)
			}
		case hubOpFind:
			_, _ = d.Find(ctx, "x")
			_, _ = d.FindByCapability(ctx, "text.echo")
		case hubOpBalance:
			_, _ = d.Balance(ctx)
		case hubOpCard:
			ra, err := d.agentCard(ctx, peer.AID())
			if err == nil {
				checkAgents(t, []module.RemoteAgent{ra})
			}
		case hubOpRegister:
			_ = d.RegisterWithHub(ctx, srv.URL, "fuzz", []string{"text.echo"}, "")
		}
	})
}

// replaysTo reports whether kel is a valid key history of aid.
func replaysTo(kel []identity.SignedEvent, aid string) bool {
	st, err := identity.Replay(kel)
	return err == nil && len(st) > 0 && st[0].AID == aid
}

// checkAgents: an agent whose card this node did not verify shows no card,
// no name from it, and none of the hub's statements beside it.
func checkAgents(t *testing.T, agents []module.RemoteAgent) {
	for _, ra := range agents {
		switch ra.Verification {
		case cardVerified:
			if len(ra.Card) == 0 {
				t.Fatalf("%s verified without a card", ra.AID)
			}
		case cardUnverified:
			if len(ra.Card) != 0 || ra.Name != "" || ra.HomeHub != "" || ra.ReviewCount != 0 || ra.LastSeen != "" {
				t.Fatalf("an unverified entry shows what it says: %+v", ra)
			}
		}
		if !validAgentID(ra.AID) {
			t.Fatalf("an entry with AID %s", strconv.Quote(ra.AID))
		}
	}
}
