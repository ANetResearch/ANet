package hubapi

import (
	"net/url"
	"strings"
)

// Connection isolation (sealed-sender §4.3.11, SI-13).
//
// A daemon talks to its hub over two HTTP transports that never share a
// connection, a TLS session ticket or a cookie: the signed side carries
// every request that names this node — a signature, a session, its own AID
// in the path — and the anonymous side everything else. Were a send under
// a delivery credential (which names no sender) to ride a connection that
// had just carried this node's signed poll, the connection itself would
// name the sender, and the credential would have hidden nothing.
//
// Routes is the one table from route to side. The daemon sends a hub
// request only through it: a route the table does not hold is refused
// before anything is sent (hub-route-unmapped), so a new call cannot slip
// onto the wrong side by default.

// Side is the transport a hub request goes on.
type Side uint8

const (
	// SideNone: the route is not in the table; nothing is sent.
	SideNone Side = iota
	// SideSigned carries the requests that name this node.
	SideSigned
	// SideAnonymous carries the requests that do not.
	SideAnonymous
	// SideByAccess is /relay/send alone: a first contact (access kind 0)
	// goes on the signed side, a send under a capability (kind 1) on the
	// anonymous side.
	SideByAccess
)

func (s Side) String() string {
	switch s {
	case SideSigned:
		return "signed"
	case SideAnonymous:
		return "anonymous"
	case SideByAccess:
		return "by-access"
	}
	return "none"
}

// Route is one hub route a daemon may call. Path is matched segment by
// segment: "{self}" is this node's AID, "{hub}" the hub's own AID, "*" any
// one segment; every other segment is literal. No route takes another
// agent's AID: a node asks about another agent through a directory bucket
// only (sealed-sender SSR-D14, SSR-D16).
type Route struct {
	Method string
	Path   string
	Side   Side
}

// Routes is the table, in the order it is matched: a request takes the
// first route that matches.
var Routes = []Route{
	// The signed side (§4.3.11 item 2; core G22).
	{"POST", RelaySessionPath, SideSigned},
	{"POST", "/relay/poll", SideSigned},
	{"POST", "/relay/ack", SideSigned},
	{"POST", "/register", SideSigned},
	{"POST", "/profile", SideSigned},
	{"POST", MailboxRevokePath, SideSigned},
	{"POST", "/reviews", SideSigned},
	{"POST", "/x402/verify", SideSigned},
	{"POST", "/x402/settle", SideSigned},
	// A withdrawal pays this node's credit out; a witness attestation is
	// signed by this node.
	{"POST", "/x402/redeem", SideSigned},
	{"POST", "/x402/witness", SideSigned},
	{"POST", "/agents/kel:push", SideSigned},
	// This node's own records: keys (SSR-W10, SSR-W16), kel, balance,
	// ledger, redemptions, liability-proof, visibility, deregister.
	{"GET", "/agents/{self}/*", SideSigned},
	{"POST", "/agents/{self}/*", SideSigned},

	// Both: a first contact names its sender to its hub, a capability
	// does not.
	{"POST", RelaySendPath, SideByAccess},

	// The anonymous side (§4.3.11 item 3).
	{"POST", "/relay/status", SideAnonymous},
	{"GET", RegistryAgentsPath, SideAnonymous},
	{"GET", "/agents", SideAnonymous},
	{"GET", "/hub/identity", SideAnonymous},
	{"GET", "/hub/kels", SideAnonymous},
	{"GET", "/x402/supported", SideAnonymous},
	{"GET", "/x402/supply", SideAnonymous},
	{"GET", "/x402/issuance", SideAnonymous},
	{"GET", "/x402/issuance/head", SideAnonymous},
	{"GET", "/x402/witnesses", SideAnonymous},
	// The hub's own key history names the hub, not this node (SSR-D14).
	{"GET", "/agents/{hub}/kel", SideAnonymous},
	// A directory bucket: how this node learns another agent's KEL, key
	// set, card and notice without naming it (SSR-D14, SSR-D15).
	{"POST", DirectoryBucketPath, SideAnonymous},
}

// SideOf is the side of one hub request: method, and target as the
// request line carries it (escaped path, optionally a query). self is this
// node's AID, hub the hub's AID ("" when not known yet); kind is the
// access kind of a /relay/send (0 or 1) and is not read for any other
// route. ok is false for a route the table does not hold, and for a
// /relay/send of any other kind: the request must not be sent.
func SideOf(method, target, self, hub string, kind int) (Route, Side, bool) {
	r, found := RouteOf(method, target, self, hub)
	if !found {
		return Route{}, SideNone, false
	}
	switch r.Side {
	case SideSigned, SideAnonymous:
		return r, r.Side, true
	case SideByAccess:
		switch kind {
		case 0:
			return r, SideSigned, true
		case 1:
			return r, SideAnonymous, true
		}
	}
	return r, SideNone, false
}

// RouteOf is the route of the table that method and target match.
func RouteOf(method, target, self, hub string) (Route, bool) {
	path, _, _ := strings.Cut(target, "?")
	if path == "" || path[0] != '/' {
		return Route{}, false
	}
	segs := strings.Split(path[1:], "/")
	for i, s := range segs {
		u, err := url.PathUnescape(s)
		if err != nil || u == "" || strings.ContainsAny(u, "/\x00") {
			return Route{}, false
		}
		segs[i] = u
	}
	for _, r := range Routes {
		if r.Method == method && routeMatches(r.Path, segs, self, hub) {
			return r, true
		}
	}
	return Route{}, false
}

// routeMatches matches a table path against the request's unescaped
// segments.
func routeMatches(pattern string, segs []string, self, hub string) bool {
	ps := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	if len(ps) != len(segs) {
		return false
	}
	for i, p := range ps {
		s := segs[i]
		switch p {
		case "{self}":
			if self == "" || s != self {
				return false
			}
		case "{hub}":
			if hub == "" || s != hub {
				return false
			}
		case "*":
		default:
			if s != p {
				return false
			}
		}
	}
	return true
}
