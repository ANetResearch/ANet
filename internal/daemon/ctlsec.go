package daemon

// ctlsec.go is the access control of the local control plane (A2A-DESIGN §7.1–§7.3, SI-7).
//
// Every control-plane request passes three layers, outermost first:
//
//  1. hostGuard. The Host header must name this listener by a loopback name (127.0.0.1, localhost or
//     [::1]) with the listener's own port; anything else is answered 421. An Origin header, when present,
//     must be one of the same three loopback origins; anything else is 403. The Host check is what
//     defeats DNS rebinding: after an attacker's name is re-pointed at 127.0.0.1 the browser still sends
//     that name in Host. Before this check existed, a rebinding page could load /console and read the
//     control token that was then embedded in it (found in the 2026-09 A2A security survey of the
//     control plane).
//  2. authGate. A route is reached with the bearer token (the CLI, MCP, scripts) or with a console
//     session (cookie + X-Anet-CSRF header, see session.go). A session may call only the routes listed in
//     sessionRoutes. The list is an allowlist, so a route added later is refused for a session until
//     someone adds it here deliberately; TestEveryRouteIsAllowlistedOrRefusedForASession enforces that.
//  3. The route handler.
//
// The control address itself must be loopback (checkLoopbackControlAddr, applied at start). There is no
// remote-access switch: a remote operator uses SSH port forwarding to the same port number, which keeps
// the Host header inside the allowlist.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// apiCSP is sent on every control-plane response except the console page, which sets its own policy.
// JSON and attachment bytes are never meant to render as a document; if a browser does render one, the
// sandbox directive gives it an opaque origin with no script execution.
const apiCSP = "default-src 'none'; sandbox; frame-ancestors 'none'"

// maxSessionJSONBody caps a JSON request body sent under a console session. The console sends short
// JSON bodies (a goal, a chat message, a review); file bytes travel as multipart, which keeps the
// multipart ceiling (maxUploadBody).
const maxSessionJSONBody = 16 << 20

// sessionRule is what a console session may send to one route.
type sessionRule struct {
	// noCSRF marks a GET route that the page loads through <img>/<a>, which cannot carry a header. Such a
	// route must be read-only; the SameSite=Strict cookie and the Sec-Fetch-Site check keep other sites
	// from using it.
	noCSRF bool
	// jsonFields, when non-nil, is the complete set of keys a JSON body may carry: exactly the fields
	// console.html sends. Anything else (local file paths in "attachments", "pay", "capability", …) is
	// refused, so the session cannot reach the parts of the handler the page does not use.
	jsonFields []string
	// multipart accepts multipart/form-data bodies (console uploads of file bytes).
	multipart bool
}

// sessionRoutes is the complete set of control routes a browser console session may call, keyed by the
// registered ServeMux pattern (A2A-DESIGN §7.3). It matches what console.html actually calls, plus the
// read-only routes the design lists. Everything else is bearer-only, including /pull, /autoreply*,
// policy and peer writes, /hub-register, /hub-leave, /visibility, /p2p-advertise, /shutdown,
// /x402-authorize, /redeem, /reconcile and /console/ticket.
var sessionRoutes = map[string]sessionRule{
	"GET /status":          {},
	"POST /status":         {},
	"POST /threads":        {},
	"POST /thread":         {},
	"POST /inbox":          {},
	"POST /results":        {},
	"POST /evidence":       {},
	"POST /balance":        {},
	"POST /identities":     {},
	"POST /find":           {},
	"GET /attachment":      {noCSRF: true},
	"POST /delegate":       {jsonFields: []string{"provider", "goal"}, multipart: true},
	"POST /message":        {jsonFields: []string{"interaction_id", "body"}, multipart: true},
	"POST /end":            {jsonFields: []string{"interaction_id"}},
	"POST /review":         {jsonFields: []string{"interaction_id", "rating", "comment"}},
	"POST /console/switch": {jsonFields: []string{"aid"}},
}

// publicRoutes are the only top-level routes reachable without the bearer token or a session. /console
// serves the page shell (it carries no credential), /ping answers liveness probes, the root redirects to
// /console, and /console/session is authenticated by the single-use ticket in its body.
var publicRoutes = map[string]bool{
	"GET /console":          true,
	"GET /ping":             true,
	"GET /{$}":              true,
	"POST /console/session": true,
}

// routeMux is an http.ServeMux that records every pattern registered on it. The session route test
// walks the recorded patterns, which is how a route added later (by any file) is covered without anyone
// remembering to list it.
type routeMux struct {
	*http.ServeMux
	patterns []string
}

func newRouteMux() *routeMux { return &routeMux{ServeMux: http.NewServeMux()} }

// HandleFunc registers h for pattern and records the pattern.
func (m *routeMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.HandleFunc(pattern, h)
}

// Handle registers h for pattern and records the pattern.
func (m *routeMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.Handle(pattern, h)
}

// controlPlane is the assembled control-plane handler. It keeps both route tables so tests can walk
// them.
type controlPlane struct {
	http.Handler
	top      *routeMux // public routes and the authentication gate
	api      *routeMux // every authenticated route
	sessions *consoleSessions
}

// secureControlPlane wraps the authenticated route table api with the console routes, the
// authentication gate and the Host/Origin guard.
func (d *Daemon) secureControlPlane(token string, api *routeMux) *controlPlane {
	cs := newConsoleSessions()
	api.HandleFunc("GET /attachment", d.attachmentHandler())
	api.HandleFunc("POST /console/ticket", d.hConsoleTicket(cs))
	api.HandleFunc("POST /console/switch", d.hConsoleSwitch)

	top := newRouteMux()
	top.HandleFunc("GET /console", d.consoleHandler())
	top.HandleFunc("GET /ping", d.pingHandler())
	top.HandleFunc("POST /console/session", d.hConsoleSession(cs))
	// A browser landing on the bare root (typed URL, bookmark) is sent to the console page, which tells
	// the operator to run `anet console` when it has no ticket. ({$} matches ONLY "/", so the gate keeps
	// the "/" catch-all.)
	top.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console", http.StatusFound)
	})
	top.Handle("/", d.authGate(token, cs, api))
	return &controlPlane{Handler: d.hostGuard(top), top: top, api: api, sessions: cs}
}

// ctlPortKey carries the listener port hostGuard validated against, for handlers that build URLs and
// cookie names.
type ctlPortKey struct{}

// requestPort returns the listener port recorded by hostGuard.
func requestPort(r *http.Request) string {
	p, _ := r.Context().Value(ctlPortKey{}).(string)
	return p
}

// listenerPort is the port of the listener that accepted r. A request constructed in-process (no
// connection) falls back to the configured control address.
func listenerPort(r *http.Request, fallbackAddr string) string {
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && a != nil {
		if _, p, err := net.SplitHostPort(a.String()); err == nil && p != "" {
			return p
		}
	}
	_, p, _ := net.SplitHostPort(fallbackAddr)
	return p
}

// loopbackName reports whether host (without port, brackets removed) is one of the three names the
// control plane answers to.
func loopbackName(host string) bool {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// allowedHost reports whether a Host header value names this listener: a loopback name and exactly the
// listener's port. A Host without a port is accepted only for a listener on port 80, which is what a
// browser sends for that port.
func allowedHost(host, port string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		h, p = host, "80"
	}
	return port != "" && p == port && loopbackName(h)
}

// allowedOrigin reports whether an Origin header value is a page served by this listener.
func allowedOrigin(origin, port string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return false
	}
	return allowedHost(u.Host, port)
}

// hostGuard enforces the Host allowlist and the Origin check on every request, and sets the headers
// common to every control-plane response.
func (d *Daemon) hostGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		port := listenerPort(r, d.config().ControlAddr)
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Content-Security-Policy", apiCSP)
		hdr.Set("Referrer-Policy", "no-referrer")
		if !allowedHost(r.Host, port) {
			writeJSON(w, http.StatusMisdirectedRequest, map[string]string{
				"error": "misdirected request: the control plane answers only to 127.0.0.1, localhost or [::1] on port " + port,
			})
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !allowedOrigin(o, port) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctlPortKey{}, port)))
	})
}

// limitBody caps the request body: the JSON ceiling for ordinary calls, the upload ceiling for
// multipart console uploads.
func limitBody(w http.ResponseWriter, r *http.Request) {
	limit := int64(maxControlBody)
	if isMultipart(r) {
		limit = maxUploadBody
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
}

func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
}

// sessionRefusal is the error body for a request a console session may not make. The "refused" field
// lets tests tell a gate refusal from a handler error.
func sessionRefusal(w http.ResponseWriter, why string) {
	writeJSON(w, http.StatusForbidden, map[string]string{"error": why, "refused": "session"})
}

// authGate authenticates a request by bearer token or console session and applies the session route
// allowlist.
func (d *Daemon) authGate(token string, cs *consoleSessions, api *routeMux) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			if subtle.ConstantTimeCompare([]byte(auth), want) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			limitBody(w, r)
			api.ServeHTTP(w, r)
			return
		}
		sess := cs.fromRequest(r)
		if sess == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, pattern := api.Handler(r)
		rule, ok := sessionRoutes[pattern]
		if !ok {
			sessionRefusal(w, "this route is not available to a console session; use the anet CLI")
			return
		}
		// A page on another port of this machine is same-site to the console, so SameSite=Strict still
		// sends the cookie with its requests. Sec-Fetch-Site distinguishes it (same-site, not
		// same-origin) when the browser sends the header.
		if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
			sessionRefusal(w, "console session requests must come from the console page")
			return
		}
		if !rule.noCSRF {
			if r.Header.Get("Origin") == "" {
				sessionRefusal(w, "console session requests must carry an Origin header")
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(sess.csrf)) != 1 {
				sessionRefusal(w, "missing or wrong "+csrfHeader+" header")
				return
			}
		}
		limitBody(w, r)
		if isMultipart(r) {
			if !rule.multipart {
				sessionRefusal(w, "multipart bodies are not accepted on this route for a console session")
				return
			}
		} else if rule.jsonFields != nil {
			if why := restrictJSONFields(r, rule.jsonFields); why != "" {
				sessionRefusal(w, why)
				return
			}
		}
		api.ServeHTTP(w, r)
	})
}

// restrictJSONFields reads a JSON object body, refuses any key outside allowed, and restores the body
// for the handler. It returns the refusal text, or "" when the body is acceptable. An empty body is
// left for the handler to reject.
func restrictJSONFields(r *http.Request, allowed []string) string {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxSessionJSONBody+1))
	if err != nil {
		return "unreadable request body"
	}
	if len(raw) > maxSessionJSONBody {
		return fmt.Sprintf("request body over %d bytes; send files as a multipart upload", maxSessionJSONBody)
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "request body is not a JSON object"
	}
	for k := range obj {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Sprintf("field %q is not available to a console session; use the anet CLI", k)
		}
	}
	return ""
}

// checkLoopbackControlAddr refuses a control address whose host is not one of the names hostGuard
// accepts (127.0.0.1, localhost, ::1). An empty host (":39811") listens on every interface and is
// refused. Other 127/8 addresses are loopback too but are refused, because a client using them would
// send a Host header the guard rejects.
func checkLoopbackControlAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("anet: control_addr %q: %w", addr, err)
	}
	if loopbackName(host) {
		return nil
	}
	return fmt.Errorf("anet: control_addr %q is not a loopback address; the control plane listens only on "+
		"127.0.0.1, localhost or [::1] (for remote access use SSH port forwarding to the same port)", addr)
}
