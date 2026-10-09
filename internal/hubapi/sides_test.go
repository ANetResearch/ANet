package hubapi_test

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity/identitytest"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// testAIDs are three distinct, well-formed AIDs: this node, its hub and a
// peer.
func testAIDs(t testing.TB) (self, hub, peer string) {
	t.Helper()
	var out [3]string
	for i := range out {
		c, err := identitytest.Hub()
		if err != nil {
			t.Fatal(err)
		}
		out[i] = c.AID()
	}
	return out[0], out[1], out[2]
}

// TestEveryHubRouteHasOneSide is SI-13's table (sealed-sender §4.3.11,
// SSR-D26, M30): every route the daemon sends is on exactly the side the
// specification lists — the requests that name this node on the signed
// side, every other on the anonymous one, /relay/send by its access kind —
// and a route the table does not hold has no side, so nothing can be sent
// on it by default.
func TestEveryHubRouteHasOneSide(t *testing.T) {
	self, hub, peer := testAIDs(t)
	esc := url.PathEscape
	signed := [][2]string{
		{"POST", "/relay/session"},
		{"POST", "/relay/poll"},
		{"POST", "/relay/ack"},
		{"POST", "/register"},
		{"POST", "/profile"},
		{"POST", "/mailbox/revoke"}, // SS-8's withdrawals (SSR-D20)
		{"POST", "/reviews"},
		{"POST", "/x402/verify"},
		{"POST", "/x402/settle"},
		{"POST", "/x402/redeem"},
		{"POST", "/x402/witness"},
		{"POST", "/agents/kel:push"},
		{"GET", "/agents/" + esc(self) + "/keys"}, // SS-7's own key set (SSR-W10, SSR-W16)
		{"POST", "/agents/" + esc(self) + "/keys"},
		{"GET", "/agents/" + esc(self) + "/kel"},
		{"GET", "/agents/" + esc(self) + "/balance"},
		{"GET", "/agents/" + esc(self) + "/ledger?limit=500"},
		{"GET", "/agents/" + esc(self) + "/redemptions"},
		{"GET", "/agents/" + esc(self) + "/liability-proof?to_day=3"},
		{"POST", "/agents/" + esc(self) + "/visibility"},
		{"POST", "/agents/" + esc(self) + "/deregister"},
	}
	anonymous := [][2]string{
		{"POST", "/relay/status"},
		{"POST", "/directory:bucket"},
		{"GET", "/a2a/v1/agents?skill=x"},
		{"GET", "/agents"},
		{"GET", "/agents?cap=cas.put"},
		{"GET", "/hub/identity"},
		{"GET", "/hub/kels"},
		{"GET", "/x402/supported"},
		{"GET", "/x402/supply"},
		{"GET", "/x402/issuance?from=0"},
		{"GET", "/x402/issuance/head"},
		{"GET", "/agents/" + esc(hub) + "/kel"},
	}
	for _, r := range signed {
		if _, side, ok := hubapi.SideOf(r[0], r[1], self, hub, -1); !ok || side != hubapi.SideSigned {
			t.Errorf("%s %s: side %v (ok %v), want signed", r[0], r[1], side, ok)
		}
	}
	for _, r := range anonymous {
		if _, side, ok := hubapi.SideOf(r[0], r[1], self, hub, -1); !ok || side != hubapi.SideAnonymous {
			t.Errorf("%s %s: side %v (ok %v), want anonymous", r[0], r[1], side, ok)
		}
	}
	// A send goes by its access kind, and only by one of the two.
	for kind, want := range map[int]hubapi.Side{0: hubapi.SideSigned, 1: hubapi.SideAnonymous} {
		if _, side, ok := hubapi.SideOf("POST", "/relay/send", self, hub, kind); !ok || side != want {
			t.Errorf("/relay/send kind %d: %v, want %v", kind, side, want)
		}
	}
	for _, kind := range []int{-1, 2, 255} {
		if _, side, ok := hubapi.SideOf("POST", "/relay/send", self, hub, kind); ok || side != hubapi.SideNone {
			t.Errorf("/relay/send kind %d: %v, want no side", kind, side)
		}
	}
	// Nothing outside the table goes anywhere: another route, another
	// method, a peer's records on a signed route, a malformed target.
	for _, r := range [][2]string{
		{"POST", "/relay/stauts"},
		{"GET", "/relay/poll"},
		{"DELETE", "/agents/" + esc(self) + "/keys"},
		{"GET", "/agents/" + esc(peer) + "/balance"},
		{"POST", "/agents/" + esc(peer) + "/visibility"},
		{"GET", "/agents/" + esc(self) + "/keys/extra"},
		{"GET", "/agents/" + esc(self)},
		{"GET", "/hub/identity/"},
		{"GET", "hub/identity"},
		{"GET", "/agents/%zz/keys"},
		{"GET", "/agents/" + esc(self) + "%2Fkeys"},
		{"POST", "/fed/v3/forward"},
		{"POST", "/tasks/create"},
		// The lookups by another agent's AID are gone: no route of the
		// table takes a peer's AID (SSR-D14, SSR-D16).
		{"POST", "/agents/keys:lookup"},
		{"POST", "/agents/kel:lookup"},
		{"POST", "/a2a/v1/agents/card:lookup"},
		{"GET", "/agents/" + esc(peer) + "/keys"},
		{"GET", "/agents/" + esc(peer) + "/kel"},
		{"GET", "/a2a/v1/agents/" + esc(peer) + "/card"},
		{"GET", "/agents/" + esc(peer)},
		{"GET", "/agents/" + esc(peer) + "/jwks.json"},
	} {
		if _, side, ok := hubapi.SideOf(r[0], r[1], self, hub, -1); ok || side != hubapi.SideNone {
			t.Errorf("%s %s: side %v, want none (not in the table)", r[0], r[1], side)
		}
	}
	// Without this node's AID or the hub's, their routes have no side.
	if _, _, ok := hubapi.SideOf("GET", "/agents/"+esc(self)+"/balance", "", hub, -1); ok {
		t.Error("an own-record route matched without this node's AID")
	}
	if r, _, ok := hubapi.SideOf("GET", "/agents/"+esc(hub)+"/kel", self, "", -1); ok && strings.Contains(r.Path, "{hub}") {
		t.Error("the hub's KEL route matched without the hub's AID")
	}

	// The table itself: one side per route, the two kinds of /relay/send
	// only there, and no path segment that takes any AID.
	seen := map[string]bool{}
	for _, r := range hubapi.Routes {
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("%s is in the table twice", key)
		}
		seen[key] = true
		switch r.Side {
		case hubapi.SideSigned, hubapi.SideAnonymous:
		case hubapi.SideByAccess:
			if key != "POST /relay/send" {
				t.Errorf("%s goes by access kind; only /relay/send does", key)
			}
		default:
			t.Errorf("%s has side %v", key, r.Side)
		}
		for _, seg := range strings.Split(r.Path, "/") {
			if strings.HasPrefix(seg, "{") && seg != "{self}" && seg != "{hub}" {
				t.Errorf("%s: segment %s takes an AID that is neither this node's nor its hub's", key, seg)
			}
		}
	}
}

// FuzzHubRouteSide: whatever the method, target and kind, SideOf neither
// panics nor puts a request on a side without a route of the table, and
// never on the signed side for a target that names neither this node nor a
// signed route.
func FuzzHubRouteSide(f *testing.F) {
	self, hub, peer := testAIDs(f)
	for _, s := range []string{"/relay/send", "/relay/poll", "/agents/" + self + "/keys", "/agents/" + hub + "/kel",
		"/agents/" + peer + "/keys", "/agents/" + peer + "/kel?x=1", "/a2a/v1/agents/" + peer + "/card",
		"/agents/%2F/keys", "//", "", "?", "/agents/" + self + "/", "/x402/settle"} {
		for _, m := range []string{"GET", "POST", "PUT"} {
			f.Add(m, s, 0)
			f.Add(m, s, 1)
			f.Add(m, s, -1)
		}
	}
	f.Fuzz(func(t *testing.T, method, target string, kind int) {
		r, side, ok := hubapi.SideOf(method, target, self, hub, kind)
		if !ok {
			if side != hubapi.SideNone {
				t.Fatalf("%s %q: no route but side %v", method, target, side)
			}
			return
		}
		if side != hubapi.SideSigned && side != hubapi.SideAnonymous {
			t.Fatalf("%s %q: side %v", method, target, side)
		}
		if r.Method != method {
			t.Fatalf("%s %q matched %s %s", method, target, r.Method, r.Path)
		}
		again, ok2 := hubapi.RouteOf(method, target, self, hub)
		if !ok2 || again != r {
			t.Fatalf("%s %q: RouteOf disagrees", method, target)
		}
		// A route that names this node matches only this node's AID (in
		// the path as it reads unescaped).
		path, _, _ := strings.Cut(target, "?")
		plain, err := url.PathUnescape(path)
		if err != nil {
			t.Fatalf("%s %q matched although its path does not unescape", method, target)
		}
		if strings.Contains(r.Path, "{self}") && !strings.Contains(plain, self) {
			t.Fatalf("%s %q matched %s without this node's AID", method, target, r.Path)
		}
		if strings.Contains(r.Path, "{hub}") && !bytes.Contains([]byte(plain), []byte(hub)) {
			t.Fatalf("%s %q matched %s without the hub's AID", method, target, r.Path)
		}
	})
}
