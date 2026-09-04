package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A node that joins a hub after the daemon started must not need a
// restart before the payment surface works.
//
// The seams were withheld while no hub was configured. A module takes its
// seam once, at Start, and a fresh data directory necessarily has an empty
// hub_url then — so x402 got nil and kept it, and every payment command
// answered "no hub configured" until the daemon was restarted. There was
// no ordering that avoided it: hub-register needs a running daemon, so
// daemon-then-register is the only possible first run.
func TestSeamsAreGrantedBeforeAnyHubIsConfigured(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	d := newTestDaemon(t, "", true) // no hub at start, as on a fresh install
	host := moduleHost{d}

	if _, ok := host.PaymentSeam(); !ok {
		t.Fatal("the payment seam was withheld from a node with no hub yet")
	}
	if _, ok := host.HubSeam(); !ok {
		t.Fatal("the hub seam was withheld from a node with no hub yet")
	}

	// And the address is answered live, so joining a hub is enough.
	seam, _ := host.PaymentSeam()
	if got := seam.HubURL(); got != "" {
		t.Fatalf("with no hub the address must be empty, got %q", got)
	}
	if err := d.HubRegister(context.Background(), h.URL, "n", nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := seam.HubURL(); got != h.URL {
		t.Fatalf("the seam did not follow the hub the node just joined: %q", got)
	}
}

// Leaving the hub this node is pointed at must also stop pointing at it.
//
// Deregistering removed the routing at the hub; locally hub_url stayed, so
// the relay loop went on polling a hub that had just refused this node —
// once a second, every rejection a log line, surviving restarts.
func TestLeavingTheCurrentHubForgetsIt(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	d := newTestDaemon(t, h.URL, true)
	if err := d.HubRegister(context.Background(), h.URL, "n", nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.LeaveHub(context.Background(), h.URL); err != nil {
		t.Fatal(err)
	}
	if got := d.config().HubURL; got != "" {
		t.Fatalf("hub_url still names the hub this node left: %q", got)
	}
	// Persisted, or a restart would bring it back.
	cfg, err := LoadConfig(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HubURL != "" {
		t.Fatalf("hub_url was cleared in memory but not on disk: %q", cfg.HubURL)
	}
	d.mu.Lock()
	stillPolling := d.relayStop != nil
	d.mu.Unlock()
	if stillPolling {
		t.Fatal("the relay loop is still polling a hub this node has left")
	}
}

// Leaving some OTHER hub must not unregister this node from its own.
//
// The documented way to move house is to register with the new hub and
// then leave the old one; in that order hub_url already names the new hub.
func TestLeavingAnotherHubKeepsTheCurrentOne(t *testing.T) {
	oldHub := newFakeHub(t)
	defer oldHub.Close()
	newHub := newFakeHub(t)
	defer newHub.Close()

	d := newTestDaemon(t, oldHub.URL, true)
	if err := d.HubRegister(context.Background(), oldHub.URL, "n", nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.HubRegister(context.Background(), newHub.URL, "n", nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.LeaveHub(context.Background(), oldHub.URL); err != nil {
		t.Fatal(err)
	}
	if got := d.config().HubURL; got != newHub.URL {
		t.Fatalf("leaving the old hub disturbed the new one: %q", got)
	}
}

// A requester's chain must show what it asked for, not only what it got.
//
// The prose path recorded this; the capability path never did. After a
// capability call completed, the requester's chain held only
// anet.result.accepted — no provider, no request CID, no capability id.
func TestACapabilityDelegationIsOnTheRequestersChain(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	d := newTestDaemon(t, h.URL, true)
	if err := d.HubRegister(context.Background(), h.URL, "req", nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	other := newTestDaemon(t, h.URL, true)
	if err := other.HubRegister(context.Background(), h.URL, "prov", nil, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	_, beforeRecs := d.ledger.Evidence(EvidenceQuery{EventType: EvDelegationSent, Limit: 100})
	before := len(beforeRecs)
	if _, err := d.DelegateCapability(context.Background(), other.AID(), "text.digest",
		map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	_, after := d.ledger.Evidence(EvidenceQuery{EventType: EvDelegationSent, Limit: 100})
	if len(after) != before+1 {
		t.Fatalf("no %s record for a capability delegation (%d → %d)", EvDelegationSent, before, len(after))
	}
	p := after[len(after)-1].Payload
	for _, k := range []string{"interaction_id", "provider_aid", "request_cid", "capability"} {
		if v, ok := p[k]; !ok || v == "" {
			t.Errorf("the record does not say %q: %+v", k, p)
		}
	}
	if p["capability"] != "text.digest" {
		t.Errorf("the record names the wrong capability: %v", p["capability"])
	}
	if p["provider_aid"] != other.AID() {
		t.Errorf("the record names the wrong provider: %v", p["provider_aid"])
	}
}

// A node vouches for its own key history.
//
// The peers table records only key histories seen on the inbound
// delegation path, so a node never had its own — and org.verify returned
// "issuer KEL unresolvable" for every credential the verifying node had
// itself issued, which is exactly the single-node organisation the
// fixture produces.
func TestANodeResolvesItsOwnKeyHistory(t *testing.T) {
	d := newTestDaemon(t, "", true)
	host := moduleHost{d}
	kel, ok := host.ResolveKEL(d.AID())
	if !ok || len(kel) == 0 {
		t.Fatal("a node could not resolve its own key history")
	}
	if _, ok := host.ResolveKEL("bafyrei-somebody-else"); ok {
		t.Fatal("a stranger's key history was invented")
	}
	if _, ok := host.ResolveKEL(""); ok {
		t.Fatal("an empty AID resolved to something")
	}
}

// Restarting a node re-publishes what it currently serves.
//
// The capability list is folded in at hub-register time and never
// afterwards, while modules are wired at start — so a restart, which is
// how an operator applies a module change, left the hub advertising the
// previous set. The removal direction is the worse one: the hub goes on
// offering a capability the node has dropped.
func TestStartupRepublishesTheCurrentCapabilities(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	d := newTestDaemon(t, h.URL, true)
	if err := d.HubRegister(context.Background(), h.URL, "n", []string{"stale.cap"}, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	hv, _ := hubsByURL.Load(h.URL)
	fake := hv.(*fakeHub)
	// Drift: the hub's idea of what this node serves is now wrong. That is
	// what a module change plus a restart produces, and asserting on the
	// caps the FIRST registration left would pass whether or not the
	// restart republished anything.
	fake.mu.Lock()
	fake.agents[d.AID()].view.Caps = []string{"gone.cap"}
	fake.mu.Unlock()

	// A fresh Daemon over the same data dir is what a restart is.
	d2, err := New(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()

	var got []string
	for i := 0; i < 200; i++ {
		fake.mu.Lock()
		if a, ok := fake.agents[d.AID()]; ok {
			got = append([]string(nil), a.view.Caps...)
		}
		fake.mu.Unlock()
		if len(got) == 1 && got[0] == "stale.cap" {
			return // the restart put the hub straight
		}
		time.Sleep(25 * time.Millisecond)
	}
	b, _ := json.Marshal(got)
	t.Fatalf("the restart did not correct the hub's capability list, it still says %s", b)
}

// A capability this node does not serve gets an answer, not silence.
//
// The requester named a capability id, so it is not a task an agent could
// interpret differently. Returning nothing handed it to the auto-reply
// path, and a node with no auto-reply never replied: the interaction sat
// at queued forever with no error, no effect status, and nothing on either
// chain. The requester could not tell "does not serve it" from "is down"
// from "still working" — which is the one thing the honest-effect-status
// rule exists to prevent.
func TestAnUnservedCapabilityAnswersUnavailable(t *testing.T) {
	h := newFakeHub(t)
	defer h.Close()
	req := newTestDaemon(t, h.URL, true)
	prov := newTestDaemon(t, h.URL, true)
	for _, d := range []*Daemon{req, prov} {
		if err := d.HubRegister(context.Background(), h.URL, "n", nil, nil, nil, ""); err != nil {
			t.Fatal(err)
		}
	}

	ix, err := req.DelegateCapability(context.Background(), prov.AID(), "nobody.serves.this", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Drive the provider's inbound path.
	for i := 0; i < 40; i++ {
		_ = prov.pollOnce(context.Background())
		_ = req.pollOnce(context.Background())
		res, _ := req.Results(context.Background())
		for _, r := range res {
			if r.InteractionID != ix {
				continue
			}
			if !strings.Contains(r.Result, "UNAVAILABLE") {
				t.Fatalf("expected UNAVAILABLE, got: %s", r.Result)
			}
			if !strings.Contains(r.Result, "nobody.serves.this") {
				t.Fatalf("the answer should name the capability: %s", r.Result)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the requester never got an answer for a capability the provider does not serve")
}
