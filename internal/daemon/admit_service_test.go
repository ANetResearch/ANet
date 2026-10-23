//go:build !no_service

package daemon

// Design v0.3.9 §A and §B: admission delegated to an internal capability of
// the service module (inbound.admit), and the service calls a module makes
// as a peer (module.ServiceCaller). The gate (row 3a of §5.2), the cache,
// every way of failing closed, the deny list winning, the internal
// capability that the network never sees, agent_tool enforcement and the
// headers the service is sent.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
	_ "github.com/ANetResearch/ANet/module/service"
)

// admitSvc is a service behind every capability of the test service
// module: org.admit (internal) answers reply, board.post (agent_tool) and
// org.join (plain) answer {"ok":true}. It records every request.
type admitSvc struct {
	mu    sync.Mutex
	reqs  []http.Header
	caps  []string
	reply string
	code  int
	// hold, when set, makes the handler wait for it (or the request's end).
	hold chan struct{}
}

func (s *admitSvc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Header.Clone())
	s.caps = append(s.caps, r.Header.Get("X-ANet-Capability"))
	reply, code, hold := s.reply, s.code, s.hold
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	if r.Header.Get("X-ANet-Capability") != "org.admit" {
		reply, code = `{"ok":true}`, 0
	}
	if code == 0 {
		code = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(reply))
}

func (s *admitSvc) set(code int, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code, s.reply = code, reply
}

// asked is how many requests reached capability.
func (s *admitSvc) asked(capability string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.caps {
		if c == capability {
			n++
		}
	}
	return n
}

func (s *admitSvc) lastHeader() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

// admitModules is the service module of the tests, every capability on url.
func admitModules(url string) map[string]any {
	return map[string]any{"service": map[string]any{"allow_tcp": true, "capabilities": []any{
		map[string]any{"id": "org.admit", "url": url, "internal": true},
		map[string]any{"id": "org.secret", "url": url, "internal": true},
		map[string]any{"id": "board.post", "url": url, "agent_tool": true, "description": "Post to the board",
			"input_schema": map[string]any{"type": "object"}},
		map[string]any{"id": "org.join", "url": url},
	}}}
}

// admitNode is a registered provider with the test service module and
// inbound.admit naming org.admit, and the service behind it.
func admitNode(t *testing.T, srvURL string, cacheSeconds int) (*Daemon, *admitSvc) {
	t.Helper()
	svc := &admitSvc{reply: `{"admit":true}`}
	backend := httptest.NewServer(svc)
	t.Cleanup(backend.Close)
	d := newTestDaemonCfg(t, srvURL, admitModules(backend.URL))
	if err := d.RegisterWithHub(context.Background(), srvURL, "org", nil); err != nil {
		t.Fatal(err)
	}
	d.stopRelayLoop()
	if err := d.SetInboundAdmit(&InboundAdmit{Capability: "org.admit", CacheSeconds: cacheSeconds}); err != nil {
		t.Fatal(err)
	}
	return d, svc
}

// A member's text task is accepted under policy closed because the service
// admits it: trust peer, recorded as admitted, its turn Admitted, the
// service asked with the member's AID. A stranger the service does not admit
// is refused as before. When the service stops admitting the member, its
// next turn is not Admitted and its next message is dropped.
func TestAdmissionAcceptsAMemberAndStopsAtTheNextTurn(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov, svc := admitNode(t, srv.URL, 0)
	member := registered(t, srv.URL, "member")

	id, err := member.Delegate(ctx, prov.AID(), "summarise the board", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ix := mustIX(t, prov, id)
	if ix.Trust != interactions.TrustPeer {
		t.Fatalf("trust = %q, want peer", ix.Trust)
	}
	if got := svc.lastHeader().Get("X-ANet-Caller"); got != member.AID() {
		t.Errorf("the service was asked about %q, want the member %s", got, member.AID())
	}
	if n := svc.asked("org.admit"); n != 1 {
		t.Errorf("the service was asked %d times, want 1", n)
	}
	admittedEv := 0
	prov.ledger.scan(EvDelegationReceived, 0, func(_ int64, p map[string]any) {
		if p["interaction_id"] == id && p["admitted"] == true {
			admittedEv++
		}
	})
	if admittedEv != 1 {
		t.Errorf("anet.delegation.received with admitted=true: %d, want 1", admittedEv)
	}
	tu, ok := turnsOf(t, prov)[id]
	if !ok || !tu.Admitted || tu.Trusted {
		t.Fatalf("turn = %+v (owed %v), want Admitted and not Trusted", tu, ok)
	}
	st := prov.InboundStatus()
	if st.Admit == nil || st.Admit.Capability != "org.admit" || st.Admit.CacheSeconds != admitCacheDefaultSeconds ||
		st.AdmittedAutoReply != "off" {
		t.Errorf("InboundStatus admit = %+v, auto_reply_admitted %q", st.Admit, st.AdmittedAutoReply)
	}

	// A stranger the service does not admit.
	svc.set(200, `{"admit":false}`)
	stranger := registered(t, srv.URL, "stranger")
	sid, err := stranger.Delegate(ctx, prov.AID(), "let me in", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state, meta := lastStatusMeta(t, stranger, sid); state != interactions.StateRejected || meta[a2ashape.KeyReason] != reasonNotAccepting {
		t.Fatalf("stranger: %s %v, want rejected not_accepting", state, meta)
	}

	// The service removes the member; once the cache lapses (cleared here)
	// the next turn is decided afresh.
	prov.admitted.clear()
	if tu := turnsOf(t, prov)[id]; tu.Admitted {
		t.Error("the removed member's turn is still Admitted")
	}
	msg := sealFrom(t, member, prov, seal.TypeMessage, id, chatBody(t, "one more thing", "msg_after_removal"))
	if r := receive(t, prov, msg); r.class != rxDropped || r.reason != dropNotAllowed {
		t.Fatalf("message after removal: %+v", r)
	}
}

// The deny list wins: a denied peer is refused before the service is asked,
// whatever it would answer, and its turns are not owed.
func TestAdmissionDenyListWins(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov, svc := admitNode(t, srv.URL, 0)
	denied := registered(t, srv.URL, "denied")
	denyPeers(t, prov, denied.AID())
	id, err := denied.Delegate(ctx, prov.AID(), "work", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if state, meta := lastStatusMeta(t, denied, id); state != interactions.StateRejected || meta[a2ashape.KeyReason] != reasonNotAccepting {
		t.Fatalf("denied: %s %v, want rejected not_accepting (closed: as a stranger)", state, meta)
	}
	if n := svc.asked("org.admit"); n != 0 {
		t.Fatalf("the service was asked %d times about a denied peer", n)
	}
	if prov.admittedFor(denied.AID(), false, prov.readPeers()) {
		t.Error("admittedFor answers true for a denied peer")
	}
}

// Row 3a is asked only for a text task from a peer on no list, after the
// deny list, and admits ahead of the approval queue and the open policy.
func TestAdmissionRowOfThePolicy(t *testing.T) {
	ps := peerSets{allow: map[string]bool{"al": true}, trust: map[string]bool{"tr": true},
		deny: map[string]bool{"dn": true}}
	on := InboundConfig{Policy: PolicyClosed, Admit: &InboundAdmit{Capability: "org.admit"}}
	for _, c := range []struct {
		name, from, capID string
		in                InboundConfig
		answer            bool
		asked             bool
		action, trust     string
		admitted          bool
	}{
		{"stranger admitted", "st", "", on, true, true, actAccept, interactions.TrustPeer, true},
		{"stranger not admitted", "st", "", on, false, true, actRefuse, "", false},
		{"denied", "dn", "", on, true, false, actRefuse, "", false},
		{"allowed", "al", "", on, true, false, actAccept, interactions.TrustPeer, false},
		{"trusted", "tr", "", on, true, false, actAccept, interactions.TrustPeer, false},
		{"capability call", "st", "org.join", on, true, false, actRefuse, "", false},
		{"approve, admitted", "st", "", withPolicy(on, PolicyApprove), true, true, actAccept, interactions.TrustPeer, true},
		{"approve, not admitted", "st", "", withPolicy(on, PolicyApprove), false, true, actHold, "", false},
		{"open, admitted", "st", "", withPolicy(on, PolicyOpen), true, true, actAccept, interactions.TrustPeer, true},
		{"open, not admitted", "st", "", withPolicy(on, PolicyOpen), false, true, actAccept, interactions.TrustPublic, false},
		{"admit not set", "st", "", InboundConfig{Policy: PolicyClosed}, true, false, actRefuse, "", false},
	} {
		asked := false
		dec, _, _ := decidePolicy(c.in, ps, c.from, c.capID, func() bool { asked = true; return c.answer })
		if asked != c.asked || dec.action != c.action || dec.trust != c.trust || dec.admitted != c.admitted {
			t.Errorf("%s: asked %v, %s %q admitted %v; want asked %v, %s %q admitted %v", c.name,
				asked, dec.action, dec.trust, dec.admitted, c.asked, c.action, c.trust, c.admitted)
		}
	}
}

func withPolicy(in InboundConfig, p string) InboundConfig {
	in.Policy = p
	return in
}

// Anything but {"admit": true} within the deadline is "not admitted": an
// error status, a reply of another shape, a slow service, a service that is
// down. A failed question counts for admitErrorTTL, not the whole cache.
func TestAdmissionFailsClosed(t *testing.T) {
	oldTimeout, oldTTL := admitCallTimeout, admitErrorTTL
	admitCallTimeout, admitErrorTTL = 300*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { admitCallTimeout, admitErrorTTL = oldTimeout, oldTTL })
	srv := newFakeHub(t)
	prov, svc := admitNode(t, srv.URL, 300)
	const peer = "did:anet:peer00000000000a"
	for _, c := range []struct {
		code  int
		reply string
	}{
		{500, `{"admit":true}`},
		{403, `{"error":"not_member"}`},
		{200, `{"admit":"true"}`},
		{200, `{"admit":1}`},
		{200, `{"admitted":true}`},
		{200, `[true]`},
		{200, `true`},
		{200, ``},
	} {
		prov.admitted.clear()
		svc.set(c.code, c.reply)
		if prov.admittedPeer(peer) {
			t.Errorf("HTTP %d %q admitted the peer", c.code, c.reply)
		}
	}

	// A slow service: refused at the deadline.
	prov.admitted.clear()
	svc.set(200, `{"admit":true}`)
	hold := make(chan struct{})
	svc.mu.Lock()
	svc.hold = hold
	svc.mu.Unlock()
	start := time.Now()
	if prov.admittedPeer(peer) {
		t.Error("a service that did not answer in time admitted the peer")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("the question took %v; the deadline is %v", el, admitCallTimeout)
	}
	close(hold)
	svc.mu.Lock()
	svc.hold = nil
	svc.mu.Unlock()
	// The failure is kept for admitErrorTTL only, then the service is asked
	// again and its admission counts.
	n := svc.asked("org.admit")
	if prov.admittedPeer(peer) {
		t.Error("admitted within the error TTL")
	}
	if svc.asked("org.admit") != n {
		t.Error("the service was asked again within the error TTL")
	}
	time.Sleep(admitErrorTTL + 100*time.Millisecond)
	if !prov.admittedPeer(peer) {
		t.Error("not admitted once the service answers again")
	}

	// Without the service module's capability (inbound.admit names one the
	// module does not serve as internal) nothing is admitted.
	prov.mu.Lock()
	in := prov.cfg.inbound()
	in.Admit = &InboundAdmit{Capability: "org.join"}
	prov.cfg.Inbound = &in
	prov.mu.Unlock()
	prov.admitted.clear()
	before := svc.asked("org.join")
	if prov.admittedPeer(peer) {
		t.Error("a capability that is not internal admitted the peer")
	}
	if svc.asked("org.join") != before {
		t.Error("a capability that is not internal was asked")
	}
}

// A decision is kept per peer for cache_seconds, admitted or not, and a
// burst of questions about one peer asks the service once.
func TestAdmissionCache(t *testing.T) {
	srv := newFakeHub(t)
	prov, svc := admitNode(t, srv.URL, 1)
	const a, b = "did:anet:peer00000000000a", "did:anet:peer00000000000b"
	if !prov.admittedPeer(a) || !prov.admittedPeer(a) {
		t.Fatal("not admitted")
	}
	if n := svc.asked("org.admit"); n != 1 {
		t.Fatalf("asked %d times within the cache, want 1", n)
	}
	svc.set(200, `{"admit":false}`)
	if prov.admittedPeer(b) || prov.admittedPeer(b) {
		t.Fatal("b admitted")
	}
	if n := svc.asked("org.admit"); n != 2 {
		t.Fatalf("a negative answer is not cached: asked %d times, want 2", n)
	}
	if !prov.admittedPeer(a) {
		t.Error("a's admission was not kept for its cache time")
	}
	time.Sleep(1100 * time.Millisecond)
	if prov.admittedPeer(a) {
		t.Error("a still admitted after the cache lapsed and the service said no")
	}

	// One question for a burst.
	prov.admitted.clear()
	svc.set(200, `{"admit":true}`)
	hold := make(chan struct{})
	svc.mu.Lock()
	svc.hold = hold
	svc.mu.Unlock()
	before := svc.asked("org.admit")
	var wg sync.WaitGroup
	results := make([]bool, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = prov.admittedPeer(a)
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(hold)
	wg.Wait()
	svc.mu.Lock()
	svc.hold = nil
	svc.mu.Unlock()
	if n := svc.asked("org.admit") - before; n != 1 {
		t.Errorf("a burst of 8 asked the service %d times, want 1", n)
	}
	for i, r := range results {
		if !r {
			t.Errorf("question %d of the burst: not admitted", i)
		}
	}
}

// inbound.admit names an internal capability of the running service module,
// is not public, and keeps its cache within 1 to 300 seconds; a write that
// breaks one of these is config_invalid and changes nothing, and the daemon
// does not start with it.
func TestAdmitConfigIsChecked(t *testing.T) {
	srv := newFakeHub(t)
	prov, _ := admitNode(t, srv.URL, 0)
	for _, a := range []InboundAdmit{
		{Capability: "board.post"},
		{Capability: "org.join"},
		{Capability: "missing"},
		{Capability: ""},
		{Capability: "org admit"},
		{Capability: "org.admit", CacheSeconds: 301},
		{Capability: "org.admit", CacheSeconds: -1},
	} {
		err := prov.SetInboundAdmit(&a)
		var ref *a2ashape.Refusal
		if !errors.As(err, &ref) || ref.Key != "config_invalid" {
			t.Errorf("%+v: %v, want config_invalid", a, err)
		}
	}
	if got := prov.config().inbound().Admit; got == nil || got.Capability != "org.admit" {
		t.Fatalf("a refused write changed inbound.admit: %+v", got)
	}
	// Not public: neither listed after admit is set, nor admit set while listed.
	err := prov.SetPublicCapabilities([]PublicCapability{{ID: "org.admit"}})
	var ref *a2ashape.Refusal
	if !errors.As(err, &ref) || ref.Key != "config_invalid" {
		t.Errorf("listing the admission capability as public: %v, want config_invalid", err)
	}
	// What the daemon refuses to write it refuses to start with.
	c := prov.config()
	in := c.inbound()
	in.Admit = &InboundAdmit{Capability: "board.post"}
	c.Inbound = &in
	if err := prov.validate(c); err == nil {
		t.Error("validate accepts inbound.admit naming a capability that is not internal")
	}
	if ref := checkInboundAdmit(InboundConfig{Admit: &InboundAdmit{Capability: "org.admit", CacheSeconds: 400}}); ref == nil {
		t.Error("the start check accepts cache_seconds 400")
	}
	// Off, then the cache is gone with it.
	if err := prov.SetInboundAdmit(nil); err != nil {
		t.Fatal(err)
	}
	if prov.config().inbound().Admit != nil || prov.InboundStatus().Admit != nil {
		t.Error("inbound.admit is still set after off")
	}
	if prov.admittedPeer("did:anet:peer00000000000a") {
		t.Error("admitted with inbound.admit off")
	}
	changes := 0
	prov.ledger.scan(EvPolicyChanged, 0, func(_ int64, p map[string]any) {
		if p["field"] == "inbound.admit" {
			changes++
		}
	})
	if changes != 2 {
		t.Errorf("anet.policy.changed for inbound.admit: %d, want 2 (on, off)", changes)
	}
}

// An internal capability is on no card and in no registration, whatever the
// lists say, and a call from the network never reaches its service.
func TestAnInternalCapabilityIsNeverPublishedOrCalled(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov, svc := admitNode(t, srv.URL, 0)
	caller := registered(t, srv.URL, "caller")
	// Listed public behind the checks' back, as a hand edit would.
	setInbound(prov, PolicyClosed, PublicCapability{ID: "org.secret"}, PublicCapability{ID: "org.join"})
	for _, id := range prov.publicSkillIDs() {
		if id == "org.secret" {
			t.Error("the internal capability is a skill of the network card")
		}
	}
	for _, id := range prov.publicOnly([]string{"org.secret", "org.join"}) {
		if id == "org.secret" {
			t.Error("the internal capability is in the hub registration")
		}
	}
	id, err := caller.DelegateCapability(ctx, prov.AID(), "org.secret", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the provider's answer", func() bool {
		_ = caller.pollOnce(ctx)
		askHubNow(t, caller)
		return mustIX(t, caller, id).IsTerminal()
	})
	if st := mustIX(t, caller, id).State; st == interactions.StateCompleted {
		t.Errorf("a network call of the internal capability completed")
	}
	if n := svc.asked("org.secret"); n != 0 {
		t.Fatalf("a network call reached the internal capability's service %d times", n)
	}
}

// module.ServiceCaller on the auto-reply module's Host: the agent tools of
// the service module; a call goes to the service as the caller with
// X-ANet-Via; only agent_tool capabilities; a denied caller gets nothing.
func TestServiceCallerOnTheHost(t *testing.T) {
	srv := newFakeHub(t)
	prov, svc := admitNode(t, srv.URL, 0)
	h, _ := newModuleHost(prov, "autoreply")
	sc, ok := h.(module.ServiceCaller)
	if !ok {
		t.Fatal("the autoreply Host grants no ServiceCaller")
	}
	if other, _ := newModuleHost(prov, "a2a"); other != nil {
		if _, ok := other.(module.ServiceCaller); ok {
			t.Error("the a2a Host grants ServiceCaller")
		}
	}
	tools := sc.AgentTools()
	if len(tools) != 1 || tools[0].Capability != "board.post" || tools[0].Description != "Post to the board" ||
		string(tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("AgentTools = %+v", tools)
	}
	ctx := context.Background()
	const member = "did:anet:member000000000a"
	out, err := sc.CallServiceAs(ctx, member, "board.post", json.RawMessage(`{"text":"help wanted"}`), "agent")
	if err != nil || string(out) != `{"ok":true}` {
		t.Fatalf("CallServiceAs = %s, %v", out, err)
	}
	hd := svc.lastHeader()
	if hd.Get("X-ANet-Caller") != member || hd.Get("X-ANet-Via") != "agent" || hd.Get("X-ANet-Capability") != "board.post" {
		t.Errorf("headers: caller %q via %q capability %q", hd.Get("X-ANet-Caller"), hd.Get("X-ANet-Via"), hd.Get("X-ANet-Capability"))
	}
	svc.mu.Lock()
	n := len(svc.reqs)
	svc.mu.Unlock()
	for _, capID := range []string{"org.admit", "org.join", "missing"} {
		if _, err := sc.CallServiceAs(ctx, member, capID, nil, "agent"); !errors.Is(err, module.ErrNotAgentTool) {
			t.Errorf("CallServiceAs(%s) = %v, want ErrNotAgentTool", capID, err)
		}
	}
	for _, c := range []struct{ caller, via string }{{"", "agent"}, {"bad aid", "agent"}, {member, ""}, {member, "Agent\r\nX: y"}} {
		if _, err := sc.CallServiceAs(ctx, c.caller, "board.post", nil, c.via); err == nil {
			t.Errorf("caller %q via %q was accepted", c.caller, c.via)
		}
	}
	denyPeers(t, prov, member)
	_, err = sc.CallServiceAs(ctx, member, "board.post", nil, "agent")
	var se *module.ServiceError
	if !errors.As(err, &se) || se.Code != reasonDenied {
		t.Errorf("a denied caller: %v, want ServiceError denied", err)
	}
	svc.mu.Lock()
	got := len(svc.reqs)
	svc.mu.Unlock()
	if got != n {
		t.Errorf("%d refused calls reached the service", got-n)
	}
}

// A backend that takes untrusted peers' tasks is given an admitted peer's
// task, with anet.admitted in its metadata.
func TestInboundTasksCarryAdmitted(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov, _ := admitNode(t, srv.URL, 0)
	member := registered(t, srv.URL, "member")
	allowed := registered(t, srv.URL, "allowed")
	allowPeers(t, prov, allowed.AID())
	prov.untrustedBackend.Store(true)
	idM, err := member.Delegate(ctx, prov.AID(), "from a member", nil)
	if err != nil {
		t.Fatal(err)
	}
	idA, err := allowed.Delegate(ctx, prov.AID(), "from an allowed peer", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{idM: true, idA: false} {
		task, ok, err := prov.inboundTaskNow(id)
		if err != nil || !ok {
			t.Fatalf("%s: delivered %v, %v", id, ok, err)
		}
		if task.Metadata[a2ashape.KeyAdmitted] != want || task.Metadata[a2ashape.KeyTrusted] != false {
			t.Errorf("%s: anet.admitted %v anet.trusted %v, want %v false", id,
				task.Metadata[a2ashape.KeyAdmitted], task.Metadata[a2ashape.KeyTrusted], want)
		}
	}
}

// The status summary says, after the inbound line, which capability admits
// and how auto-reply runs an admitted peer's turn.
func TestStatusSaysAdmission(t *testing.T) {
	srv := newFakeHub(t)
	prov, _ := admitNode(t, srv.URL, 0)
	want := a2ashape.Sentence("inbound.admit.off", a2ashape.English, a2ashape.Args{"capability": "org.admit"})
	found := false
	for _, l := range prov.statusSummary(map[string]any{}, a2ashape.English) {
		if l.Topic == a2ashape.TopicInbound && strings.Contains(l.Text, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("the inbound line does not say %q: %+v", want, prov.statusSummary(map[string]any{}, a2ashape.English))
	}
	if err := prov.SetInboundAdmit(nil); err != nil {
		t.Fatal(err)
	}
	for _, l := range prov.statusSummary(map[string]any{}, a2ashape.English) {
		if strings.Contains(l.Text, "org.admit") {
			t.Errorf("with inbound.admit off the summary still names it: %q", l.Text)
		}
	}
}
