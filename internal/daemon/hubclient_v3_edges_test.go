package daemon

// The edges of the wire-3 hub client that the main cases do not reach: a
// request that would put this node's name on the anonymous side, an answer
// stated under another wire, a capability home that is not an AID, and the
// dry run's reading of a wire header that is not a number.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// TestAnAnonymousRequestCarriesNoSignature (SI-13, SSR-D26): a request for
// the anonymous side that carries a relayauth signature or a first-contact
// session — the daemon's own, a capability send, a module's — is refused
// before anything is sent, however it came to carry it: on the anonymous
// connection it would name the node the side exists to hide.
func TestAnAnonymousRequestCarriesNoSignature(t *testing.T) {
	srv, req, _ := registeredPair(t)
	ctx := context.Background()
	seen := func(path string) int {
		n := 0
		for _, r := range hubRequests(t, srv.URL) {
			if r.path == path {
				n++
			}
		}
		return n
	}
	agents, sends, supported := seen("/agents"), seen(hubapi.RelaySendPath), seen("/x402/supported")
	before := counter(req, counterHubRouteUnmapped)

	named := func(method, path, header string) *http.Request {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, srv.URL+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set(header, "x")
		return r
	}
	for _, c := range []struct {
		r     *http.Request
		route string
		kind  int
	}{
		{named(http.MethodGet, "/agents", hubapi.HeaderAID), "/agents", -1},
		{named(http.MethodGet, "/agents", hubapi.HeaderSig), "/agents", -1},
		{named(http.MethodPost, hubapi.RelaySendPath, hubapi.HeaderSession), hubapi.RelaySendPath, 1},
		{named(http.MethodPost, hubapi.RelaySendPath, hubapi.HeaderMAC), hubapi.RelaySendPath, 1},
		{named(http.MethodPost, hubapi.RelaySendPath, hubapi.HeaderSeq), hubapi.RelaySendPath, 1},
	} {
		rep := req.hubExchange(c.r, srv.URL, c.route, c.kind)
		var refused *hubRouteError
		if rep.Status != 0 || !rep.Local || !errors.As(rep.Err, &refused) || refused.naming == "" {
			t.Errorf("%s %s with a naming header: status %d local %v: %v", c.r.Method, c.route, rep.Status, rep.Local, rep.Err)
		}
	}
	if resp, err := (paymentSeam{d: req}).HubDo(named(http.MethodGet, "/x402/supported", hubapi.HeaderAID)); err == nil {
		resp.Body.Close()
		t.Error("a module's anonymous request carrying X-ANet-AID was sent")
	}
	if n := counter(req, counterHubRouteUnmapped) - before; n != 6 {
		t.Errorf("%d refusals counted, want 6", n)
	}
	if seen("/agents") != agents || seen(hubapi.RelaySendPath) != sends || seen("/x402/supported") != supported {
		t.Fatal("a refused request reached the hub")
	}

	// The same requests without the header go, and a signed route keeps its
	// signature.
	if _, err := hubDirectory(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := req.hubSeamDo(ctx, srv.URL, "/x402/supported"); err != nil {
		t.Fatalf("a module's plain read: %v", err)
	}
	if seen("/x402/supported") != supported+1 {
		t.Fatal("a module's plain read did not reach the hub")
	}
	if err := req.pollOnce(ctx); err != nil {
		t.Fatalf("a signed poll: %v", err)
	}
}

// TestAnAnswerOfAnotherWireIsNoAnswer (CORE-V1): a 200 that states another
// wire ends the call wherever it comes — a capability send (handed, since
// the hub was reached, but not taken as queued) and a module's read alike;
// the module never sees the body.
func TestAnAnswerOfAnotherWireIsNoAnswer(t *testing.T) {
	_, req, prov := registeredPair(t)
	ctx := context.Background()
	heldCapFrom(t, prov, req)
	env, choice := underCapability(t, req, prov, sealFrom(t, req, prov, seal.TypeMessage, "ix-w", chatBody(t, "w", "")))

	hubID, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	kel, err := identity.MarshalKEL(hubID.KEL())
	if err != nil {
		t.Fatal(err)
	}
	newer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hub/identity" {
			w.Header().Set(hubapi.WireVersionHeader, "3")
			fakeHubJSON(w, http.StatusOK, hubapi.HubIdentity{AID: hubID.AID(), KEL: hubapi.B64(kel)})
			return
		}
		w.Header().Set(hubapi.WireVersionHeader, "4")
		switch r.URL.Path {
		case hubapi.RelaySendPath:
			fakeHubJSON(w, http.StatusOK, hubapi.RelaySendResponse{Status: hubapi.RelayQueued, RID: hubapi.B64(make([]byte, hubapi.RIDLen))})
		default:
			fakeHubJSON(w, http.StatusOK, map[string]any{})
		}
	}))
	t.Cleanup(newer.Close)
	req.mu.Lock()
	req.cfg.HubURL = newer.URL
	req.mu.Unlock()

	_, handed, err := req.relaySendV3(ctx, env, choice)
	if !errors.Is(err, errHubWire) || !handed {
		t.Errorf("a send answered 200 under wire 4: handed %v, %v; want errHubWire, handed", handed, err)
	}
	resp, err := req.hubSeamDo(ctx, newer.URL, "/x402/supported")
	if !errors.Is(err, errHubWire) {
		t.Errorf("a module's read answered 200 under wire 4: %d, %v; want errHubWire", resp, err)
	}
}

// TestACapabilityHomeThatIsNoAIDIsNotSent (SSR-D13, core §2.12.1): the home
// of a capability is whatever its issuer wrote into the grant; a home that
// is not a hub AID is not sent as X-ANet-Home (the hub would refuse the
// whole send, or the header would not be a header at all), and the send
// goes without one. The engine's choice for a message under a held
// capability takes the home of the held row (accessChoiceFor, A9).
func TestACapabilityHomeThatIsNoAIDIsNotSent(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	held := heldCapFrom(t, prov, req)
	env, choice := underCapability(t, req, prov, sealFrom(t, req, prov, seal.TypeMessage, "ix-h", chatBody(t, "h", "")))

	got, err := req.accessChoiceFor(prov.AID(), "ix-h", false, accessOrdinary, req.nowMS())
	if err != nil || !got.Send || got.Kind != seal.AccessKindCap || got.Cap == nil || got.Home != held.Home || got.Home == "" {
		t.Fatalf("the choice of a message under a held capability: %+v, %v; want its row's home %q", got, err, held.Home)
	}

	for _, home := range []string{"https://hub.example", "x\r\nX-Other: 1", "not an aid"} {
		c := choice
		c.Home = home
		if _, _, err := req.relaySendV3(ctx, env, c); err != nil {
			t.Fatalf("a send under a capability whose home is %q: %v", home, err)
		}
		sends := relaySends(hubRequests(t, srv.URL))
		if h := sends[len(sends)-1].header.Values(hubapi.HeaderHome); len(h) != 0 {
			t.Errorf("home %q went out as X-ANet-Home %q", home, h)
		}
	}
}

// TestDryRunReadsTheWireHeaderFirst (CORE-V1, LEC §2.3.2): the hub's
// X-ANet-Wire is what the dry run reads; the identity's wire member stands in
// only when the header is absent. A header that is not a number is no
// wire-3 hub, whatever the body says.
func TestDryRunReadsTheWireHeaderFirst(t *testing.T) {
	hubID, err := identitytest.Hub()
	if err != nil {
		t.Fatal(err)
	}
	kel, err := identity.MarshalKEL(hubID.KEL())
	if err != nil {
		t.Fatal(err)
	}
	three := hubapi.WireVersion
	odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(hubapi.WireVersionHeader, "three")
		fakeHubJSON(w, http.StatusOK, hubapi.HubIdentity{AID: hubID.AID(), KEL: hubapi.B64(kel), Wire: &three})
	}))
	t.Cleanup(odd.Close)
	d, _ := joinDaemon(t)
	code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(odd.URL, `,"dry_run":true`))
	if code != http.StatusConflict || out["reason"] != "wire_too_new" {
		t.Fatalf("a hub whose wire header is not a number: %d %v, want wire_too_new", code, out)
	}
}
