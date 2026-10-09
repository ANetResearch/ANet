package daemon

// Fuzz target for what a hub answers (an internal note). The hub is not
// trusted: every answer below is the fuzzer's, for every request.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/dirbox"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/netcard"
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
	hub := &fuzzHub{status: 200, wire: "3"}
	srv := httptest.NewServer(hub)
	f.Cleanup(srv.Close)
	d := buildTestDaemon(f, srv.URL, nil)
	peer, err := identitytest.Hub()
	if err != nil {
		f.Fatal(err)
	}
	peerKEL, _ := identity.MarshalKEL(peer.KEL())
	peerKeys := signedKeysFor(f, peer, 0)
	card, err := d.signedCard("fuzz", []string{"text.echo"})
	if err != nil {
		f.Fatal(err)
	}
	// A bucket that holds the peer's entry, with its card, and one entry
	// of nobody.
	peerHash, _ := dirbox.Hash(peer.AID())
	peerBox, err := dirbox.Seal(peer.AID(), &dirbox.DirEntry{AID: peer.AID(), Home: peer.AID(), KEL: peerKEL,
		Keys: peerKeys, Card: card}, nil)
	if err != nil {
		f.Fatal(err)
	}
	keysAnswer, _ := json.Marshal(hubapi.DirBucketResponse{Entries: []hubapi.DirBucketEntry{
		{H: hubapi.B64(peerHash), Box: hubapi.B64(peerBox)}, {H: hubapi.B64(make([]byte, 32)), Box: "AAAA"}}})
	idAnswer, _ := json.Marshal(hubapi.HubIdentity{AID: peer.AID(), KEL: hubapi.B64(peerKEL)})
	reg, _ := json.Marshal(hubapi.A2AAgentList{Agents: []hubapi.A2AAgentEntry{{AID: d.AID(), Card: card,
		CardVerification: "ok", HomeHub: "https://h", ReviewCount: 3, AvgRating: 4.5}, {AID: peer.AID(), Card: card}}})
	// Registry entries that carry key material (SSR-D15 step 2): the
	// peer's, under a network card it signed, which verifies and is kept as
	// a discovery, with a notice that is no notice; and this node's own
	// under a card that is not a network card.
	unsignedCard, err := netcard.Build(netcard.Input{AID: peer.AID(), Name: "peer", HubURL: "https://hub.example",
		Seq: 1, IssuedAtMs: 1, NotBeforeMs: 1, Skills: []netcard.Skill{{ID: "text.echo", Name: "echo", Description: "echoes"}}})
	if err != nil {
		f.Fatal(err)
	}
	peerCard, err := a2acard.SignWithSnapshot(unsignedCard, peer, netcard.JKU("https://hub.example", peer.AID()))
	if err != nil {
		f.Fatal(err)
	}
	selfKEL, _ := identity.MarshalKEL(d.self.KEL())
	regKeys, _ := json.Marshal(hubapi.A2AAgentList{Agents: []hubapi.A2AAgentEntry{
		{AID: peer.AID(), Card: peerCard, KEL: hubapi.B64(peerKEL), Keys: hubapi.B64(peerKeys), KeyStateNotice: "AAAA"},
		{AID: d.AID(), Card: card, KEL: hubapi.B64(selfKEL), Keys: hubapi.B64(signedKeysFor(f, d.self, 1))}}})
	seeds := []struct {
		op     uint8
		status uint8
		wire   string
		body   string
	}{
		{hubOpPoll, 0, "3", `{"messages":[{"id":1,"envelope":"AAEC"},{"id":-1,"envelope":"!!"}]}`},
		{hubOpPoll, 0, "3", `{"messages":[]}`},
		{hubOpKeys, 0, "3", string(keysAnswer)},
		{hubOpKeys, 0, "3", `{"max_bits":0,"entries":[],"partial":true}`},
		{hubOpKeys, 3, "", `{"reason":"bucket_too_narrow","max_bits":0}`},
		{hubOpCard, 0, "3", string(keysAnswer)},
		{hubOpIdentity, 0, "", string(idAnswer)},
		{hubOpAgents, 0, "3", string(reg)},
		{hubOpAgents, 0, "3", string(regKeys)},
		{hubOpAgentsUncarded, 0, "3", `{"agents":[{"aid":"` + peer.AID() + `","name":"n","caps":["a"],"summary":"s","avg_rating":1e308}]}`},
		{hubOpFind, 0, "3", `{"agents":[{"aid":"` + peer.AID() + `","name":"x"}]}`},
		{hubOpBalance, 0, "3", `{"balance":5,"entries":[{"amount":-3}]}`},
		{hubOpCard, 0, "3", string(card)},
		{hubOpRegister, 0, "3", `{"aid":"x","status":"registered","card":{"status":"accepted","seq":1}}`},
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
			ks, err := bucketKeys(d, peer.AID(), "")
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
			checkDiscoveries(t, d)
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
			_ = d.RegisterWithHub(ctx, srv.URL, "fuzz", []string{"text.echo"})
		}
	})
}

// checkDiscoveries: whatever a registry listing returned, the discovery
// cache (SSR-D15 step 2) holds only material that verified here — a KEL
// that replays to the AID it is kept under, a key set of that AID, and a
// card — so nothing a hub made up can reach a seal through it.
func checkDiscoveries(t *testing.T, d *Daemon) {
	d.peers.mu.Lock()
	defer d.peers.mu.Unlock()
	for aid, m := range d.peers.disc {
		if m.AID != aid || !replaysTo(m.KEL, aid) {
			t.Fatalf("a discovery of %s holds a KEL that does not replay to it", aid)
		}
		if m.Keys != nil && (m.Keys.set == nil || m.Keys.set.AID != aid) {
			t.Fatalf("a discovery of %s holds another agent's key set", aid)
		}
		if len(m.Card) == 0 {
			t.Fatalf("a discovery of %s without the card it was kept for", aid)
		}
	}
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
