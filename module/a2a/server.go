//go:build !no_a2a

package a2a

// server.go is the HTTP face of the local A2A interface: the routes of
// A2A-DESIGN §11.2 behind the checks of §11.4.
//
//	GET  /a2a/v1/agents                                   known remote agents
//	GET  /a2a/v1/agents/{aid}/.well-known/agent-card.json the proxy card (§11.3)
//	GET  /a2a/v1/agents/{aid}                             the same card
//	POST /a2a/v1/agents/{aid}/jsonrpc                     A2A JSON-RPC binding
//	POST /a2a/v1/agents/{aid}/jsonrpc/                    the same
//	     /a2a/v1/agents/{aid}/rest/...                    A2A HTTP+JSON binding
//
// The bare /a2a/v1/agents/{aid} serves the card too because that is the URL
// clients are configured with (Hermes' a2a_agents), and a2a-go's card
// resolver fetches a base URL with a path as it is, without appending the
// well-known suffix.
//
// Every request, on every route, passes the same three checks in this
// order: the Host names this listener on a loopback name (else 421), there
// is no Origin (else 403: a browser page is never a client of this
// interface), and the bearer is this interface's token (else 401). The
// control token is not accepted here, nor this token on the control plane
// (SI-7).

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/ANetResearch/ANet/internal/loopguard"
	"github.com/ANetResearch/ANet/internal/x402a2a"
	"github.com/ANetResearch/ANet/module"
)

// maxBody is the largest request body accepted (A2A-DESIGN §11.4): a
// message may carry files inline, up to the hub's envelope size.
const maxBody = 96 << 20

// agentsPath is the root of every route.
const agentsPath = "/a2a/v1/agents"

type serverConfig struct {
	token string
	// host and port are the listener's own address, which the URLs in the
	// cards and the agent list name: an address bound on [::1] is not
	// reached at 127.0.0.1. An empty host is 127.0.0.1.
	host   string
	port   string
	self   string
	signer module.ProxyCardSigner // nil: cards are served unsigned
}

// listenerURLHost is host:port for a URL that reaches the listener.
func listenerURLHost(host, port string) string {
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

type server struct {
	cfg   serverConfig
	seam  module.TaskSeam
	cards *cardCache
	mux   *http.ServeMux
	// h is the handler behind both bindings, which precheckStream asks
	// before a stream opens.
	h *handler
}

func newServer(seam module.TaskSeam, cfg serverConfig) *server {
	s := &server{cfg: cfg, seam: seam}
	s.cards = newCardCache(seam, cardBuilder{host: cfg.host, port: cfg.port}, cfg.signer)
	h := &handler{seam: seam}
	s.h = h
	opts := []a2asrv.TransportOption{
		a2asrv.WithTransportKeepAlive(15 * time.Second),
		a2asrv.WithTransportPanicHandler(func(r any) error {
			log.Printf("anet: a2a: handler panic: %v", r)
			return a2a.NewError(a2a.ErrInternalError, "internal error")
		}),
	}
	rpc := a2asrv.NewJSONRPCHandler(h, opts...)
	rest := a2asrv.NewTenantRESTHandler(agentsPath+"/{*}/rest", h, opts...)

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+agentsPath, s.listAgents)
	mux.HandleFunc("GET "+agentsPath+"/{aid}", s.serveCard)
	mux.HandleFunc("GET "+agentsPath+"/{aid}/.well-known/agent-card.json", s.serveCard)
	mux.Handle("POST "+agentsPath+"/{aid}/jsonrpc", s.binding(rpc, true))
	// The same endpoint with a trailing slash, served as it is: a client
	// that takes the interface URL as a base and posts to "/" (a2a-tck's
	// JSON-RPC client) sends it there. Not a redirect: a client that
	// follows a 301/302 turns the POST into a GET.
	mux.Handle("POST "+agentsPath+"/{aid}/jsonrpc/{$}", s.binding(rpc, true))
	mux.Handle(agentsPath+"/{aid}/rest/", s.binding(rest, false))
	s.mux = mux
	return s
}

// ServeHTTP applies the checks every route shares.
func (s *server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w := &responseWriter{ResponseWriter: rw}
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			log.Printf("anet: a2a: %s %s: panic: %v", r.Method, r.URL.Path, p)
			if !w.wrote {
				writeError(w, http.StatusInternalServerError, a2a.ErrInternalError, "internal error")
			}
		}
	}()
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Referrer-Policy", "no-referrer")
	// The control plane's Host rule, from the one place both take it
	// (internal/loopguard): a loopback name and exactly this listener's
	// port. A name bound to a loopback address by DNS is not enough, which
	// is what defeats DNS rebinding.
	if !loopguard.AllowedHost(r.Host, s.cfg.port) {
		writeError(w, http.StatusMisdirectedRequest, a2a.ErrInvalidRequest,
			"misdirected request: the local A2A interface answers only to 127.0.0.1, localhost or [::1] on port "+s.cfg.port)
		return
	}
	if r.Header.Get("Origin") != "" {
		// A browser sends Origin on every cross-origin request and on
		// every POST; no legitimate client of this interface is a page.
		writeError(w, http.StatusForbidden, a2a.ErrUnauthorized, "requests from web pages are not accepted")
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="anet-local-a2a"`)
		writeError(w, http.StatusUnauthorized, a2a.ErrUnauthenticated,
			"a bearer token is required: the local A2A token (a2a_token.txt of this node)")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// authorized compares the bearer with the token in constant time.
func (s *server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	scheme, cred, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || s.cfg.token == "" {
		return false
	}
	cred = strings.TrimSpace(cred)
	return subtle.ConstantTimeCompare([]byte(cred), []byte(s.cfg.token)) == 1
}

// aidRe is the form of an AID in a path: the character set of the base32
// CIDs identity derives, as ANetCore a2acard's kid rule has it.
var aidRe = regexp.MustCompile(`^[a-z0-9]{1,128}$`)

// agentAID reads and checks the path's AID. This node itself is not a
// remote agent: a client sends it no tasks through this interface.
func (s *server) agentAID(w http.ResponseWriter, r *http.Request) (string, bool) {
	aid := r.PathValue("aid")
	if !aidRe.MatchString(aid) || aid == s.cfg.self {
		writeError(w, http.StatusNotFound, a2a.ErrInvalidRequest, "no such agent")
		return "", false
	}
	return aid, true
}

// binding wraps an A2A binding handler: the AID from the path, the body
// checks, and the service parameters (A2A-Version, A2A-Extensions).
func (s *server) binding(next http.Handler, jsonrpc bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aid, ok := s.agentAID(w, r)
		if !ok {
			return
		}
		// JSON-RPC takes JSON and nothing else, so a form post or a
		// text/plain body — the requests a page can send without a
		// preflight — never reaches a handler. A REST request with a body
		// is held to the same rule.
		// Refused as ContentTypeNotSupportedError (A2A §5.4: HTTP 415,
		// JSON-RPC -32005), in the error form of the binding the client
		// speaks: a JSON-RPC client reads the error object, not a
		// google.rpc.Status.
		if jsonrpc || (r.Method == http.MethodPost && r.ContentLength != 0) {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				const msg = "the request body must be application/json"
				if jsonrpc {
					writeRPCError(w, http.StatusUnsupportedMediaType, a2a.ErrUnsupportedContentType, msg)
				} else {
					writeError(w, http.StatusUnsupportedMediaType, a2a.ErrUnsupportedContentType, msg)
				}
				return
			}
		}
		if r.ContentLength > maxBody {
			writeError(w, http.StatusRequestEntityTooLarge, a2a.ErrInvalidRequest,
				fmt.Sprintf("the request body is larger than %d bytes", maxBody))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)

		info := &reqInfo{aid: aid, requested: mergeExtensions(r.Header)}
		info.versionErr = checkVersion(r)
		if len(info.requested) > 0 {
			// Answered before the handler runs: a stream's headers go out
			// before its first event.
			info.active = activate(info.requested, s.cards.extensions(r.Context(), aid))
			if len(info.active) > 0 {
				w.Header().Set(a2a.SvcParamExtensions, strings.Join(info.active, ", "))
			}
		}
		r = r.WithContext(withInfo(r.Context(), info))
		// A streaming call that cannot start is answered with an ordinary
		// error, not a stream holding one (precheck.go, 0017 Q31).
		r, answered := s.precheckStream(w, r, aid, jsonrpc)
		if answered {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mergeExtensions reads the extensions a client activates, from both
// A2A-Extensions and the older X-A2A-Extensions, each possibly repeated and
// comma-separated, and rewrites the request to carry them once, under
// A2A-Extensions, one value per URI.
func mergeExtensions(h http.Header) []string {
	var out []string
	for _, key := range []string{a2a.SvcParamExtensions, "X-A2A-Extensions"} {
		for _, v := range h.Values(key) {
			for _, u := range strings.Split(v, ",") {
				if u = strings.TrimSpace(u); u != "" && !slices.Contains(out, u) {
					out = append(out, u)
				}
			}
		}
	}
	h.Del("X-A2A-Extensions")
	h.Del(a2a.SvcParamExtensions)
	for _, u := range out {
		h.Add(a2a.SvcParamExtensions, u)
	}
	return out
}

// checkVersion reads A2A-Version from the header or, failing that, from
// the query. Absent means 1.0: the specification's default is 0.3, which
// would refuse every client that does not send the header, so this is a
// documented deviation (A2A-DESIGN §2, §21 item 12). Any 1.x is accepted;
// an explicit other version is VersionNotSupported, answered by the
// handler so that each binding renders it in its own form.
func checkVersion(r *http.Request) error {
	v := strings.TrimSpace(r.Header.Get(a2a.SvcParamVersion))
	if v == "" {
		q := r.URL.Query()
		v = strings.TrimSpace(q.Get(a2a.SvcParamVersion))
		if v == "" {
			v = strings.TrimSpace(q.Get(strings.ToLower(a2a.SvcParamVersion)))
		}
		if v != "" {
			r.Header.Set(a2a.SvcParamVersion, v)
		}
	}
	if v == "" || versionRe.MatchString(v) {
		return nil
	}
	return a2a.NewError(a2a.ErrVersionNotSupported, "A2A version "+strconv.Quote(v)+" is not supported; this interface speaks 1.0")
}

var versionRe = regexp.MustCompile(`^1(\.[0-9]+){0,2}$`)

// activate is the extensions a request activates: those it asks for that
// the agent's proxy card declares, in the order asked. a2a-x402 is
// recognised by either URI x402a2a.Activated accepts — the v0.2 one the
// card declares, or the v0.1 one the official reference library
// (x402_a2a) sends (0017 Q18) — and is active as the v0.2 URI: that is
// what the kernel is told and what the response echoes. The v0.1 URI is
// only recognised, never declared or echoed.
func activate(requested, supported []string) []string {
	var out []string
	for _, u := range requested {
		switch {
		case slices.Contains(supported, u):
		case x402a2a.Activated([]string{u}) && slices.Contains(supported, x402a2a.ExtensionURI):
			u = x402a2a.ExtensionURI
		default:
			continue
		}
		if !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// reqInfo is what the HTTP layer tells the handler about one request.
type reqInfo struct {
	// aid is the remote agent in the path.
	aid string
	// requested are the extensions the client activated, active those of
	// them the agent's proxy card declares.
	requested, active []string
	// versionErr is set when the client asked for a version this interface
	// does not speak.
	versionErr error
}

type infoKey struct{}

func withInfo(ctx context.Context, i *reqInfo) context.Context {
	return context.WithValue(ctx, infoKey{}, i)
}

func infoFrom(ctx context.Context) *reqInfo {
	i, _ := ctx.Value(infoKey{}).(*reqInfo)
	return i
}

// listAgents is GET /a2a/v1/agents: the remote agents a client may
// address, each with the URL to configure and its card's URL.
func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	aq := module.AgentQuery{Skill: q.Get("skill"), Tag: q.Get("tag"), Query: q.Get("q"), Cursor: q.Get("cursor")}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, a2a.ErrInvalidParams, "limit must be a non-negative integer")
			return
		}
		aq.Limit = n
	}
	agents, err := s.seam.Agents(r.Context(), aq)
	if err != nil {
		sdk := toSDKError(err)
		writeError(w, httpStatusOf(sdk), sdk, sdk.Error())
		return
	}
	out := agentList{Agents: make([]agentEntry, 0, len(agents))}
	for _, a := range agents {
		if !aidRe.MatchString(a.AID) || a.AID == s.cfg.self {
			continue
		}
		base := s.baseURL(a.AID)
		e := agentEntry{AID: a.AID, URL: base, CardURL: base + "/.well-known/agent-card.json",
			Verification: a.Verification, VerificationError: a.VerificationError, Official: a.Official}
		if a.Verification == verified {
			// Only a verified agent is described: of any other, what a
			// hub says is not shown beside the official mark (0017 Q24),
			// and its proxy card is the one made from the AID.
			e.Name, e.HubVerification, e.HomeHub, e.LastSeen = a.Name, a.HubVerification, a.HomeHub, a.LastSeen
			e.Quiet, e.ReviewCount, e.AvgRating = a.Quiet, a.ReviewCount, a.AvgRating
		}
		out.Agents = append(out.Agents, e)
	}
	body, err := json.Marshal(out)
	if err != nil {
		writeError(w, http.StatusInternalServerError, a2a.ErrInternalError, "the agent list could not be encoded")
		return
	}
	writeCacheable(w, r, append(body, '\n'))
}

type agentList struct {
	Agents []agentEntry `json:"agents"`
}

// agentEntry is one remote agent as the list shows it: what the kernel
// knows of it, without the card bytes (the card URL serves those, as the
// proxy card), and where to reach it here.
type agentEntry struct {
	AID               string `json:"aid"`
	Name              string `json:"name,omitempty"`
	URL               string `json:"url"`
	CardURL           string `json:"cardUrl"`
	Verification      string `json:"verification,omitempty"`
	VerificationError string `json:"verificationError,omitempty"`
	HubVerification   string `json:"hubVerification,omitempty"`
	HomeHub           string `json:"homeHub,omitempty"`
	LastSeen          string `json:"lastSeen,omitempty"`
	Quiet             bool   `json:"quiet,omitempty"`
	ReviewCount       int    `json:"reviewCount,omitempty"`
	AvgRating         any    `json:"avgRating,omitempty"`
	// Official: the agent's AID is on this node's official manifest
	// (module.RemoteAgent.Official).
	Official bool `json:"anet.official,omitempty"`
}

func (s *server) baseURL(aid string) string {
	return (&url.URL{Scheme: "http", Host: listenerURLHost(s.cfg.host, s.cfg.port), Path: agentsPath + "/" + aid}).String()
}

// serveCard writes the proxy card's bytes exactly as signed.
func (s *server) serveCard(w http.ResponseWriter, r *http.Request) {
	aid, ok := s.agentAID(w, r)
	if !ok {
		return
	}
	body, err := s.cards.card(r.Context(), aid)
	if err != nil {
		log.Printf("anet: a2a: proxy card for %s: %v", aid, err)
		writeError(w, http.StatusInternalServerError, a2a.ErrInternalError, "the proxy card could not be made")
		return
	}
	writeCacheable(w, r, body)
}

// cardMaxAge is how long a client may keep a proxy card or the agent list
// without asking again (0017 Q33).
const cardMaxAge = 300

// writeCacheable writes body, a proxy card's bytes or the agent list, as a
// response a client may keep: Cache-Control private (the route needs the
// bearer, so no shared cache may keep it) with a max-age, and an ETag of
// the bytes, against which If-None-Match is answered 304 (0017 Q33;
// a2a-tck CARD-CACHE-001/002). Every other response of this interface is
// no-store (ServeHTTP).
func writeCacheable(w http.ResponseWriter, r *http.Request, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + base64.RawURLEncoding.EncodeToString(sum[:18]) + `"`
	h := w.Header()
	h.Set("Cache-Control", "private, max-age="+strconv.Itoa(cardMaxAge))
	h.Set("ETag", etag)
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// etagMatch reports whether an If-None-Match value names etag: "*", or the
// tag in its comma-separated list, compared weakly (RFC 9110 §13.1.2).
func etagMatch(ifNoneMatch, etag string) bool {
	for _, t := range strings.Split(ifNoneMatch, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}

// responseWriter is the ResponseWriter the handlers see. It records
// whether anything was written, so a failure after that point is not
// followed by a second response, and it forwards Flush: the A2A bindings'
// streams refuse a writer that cannot flush.
type responseWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *responseWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *responseWriter) Flush() {
	w.wrote = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the connection's own writer.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("anet: a2a: write response: %v", err)
	}
}

// writeError answers a request refused before it reached a binding, in the
// google.rpc.Status form of the HTTP+JSON binding (A2A §11.6), which the
// REST client reads as the named A2A error and anything else reads as
// JSON with a message.
func writeError(w http.ResponseWriter, code int, kind error, msg string) {
	type errorInfo struct {
		Type   string `json:"@type"`
		Reason string `json:"reason"`
		Domain string `json:"domain"`
	}
	type status struct {
		Code    int         `json:"code"`
		Status  string      `json:"status"`
		Message string      `json:"message"`
		Details []errorInfo `json:"details"`
	}
	st := grpcStatus(code)
	if _, same, g := errorEntry(kind); same == code {
		// The binding's own status name for this error (a2a-go's table).
		st = g
	}
	writeJSON(w, code, map[string]status{"error": {
		Code: code, Status: st, Message: msg,
		Details: []errorInfo{{Type: "type.googleapis.com/google.rpc.ErrorInfo", Reason: a2a.ErrorReason(kind), Domain: a2a.ProtocolDomain}},
	}})
}

// writeRPCError answers a JSON-RPC request refused before it reached the
// binding, in the binding's own error form: a JSON-RPC response whose error
// carries the A2A code and, in data, the google.rpc.ErrorInfo a2a-go puts
// there. The id is null: the body was not read, so the request's id is not
// known. The HTTP status still says what happened, for a client that
// looks no further.
func writeRPCError(w http.ResponseWriter, code int, kind error, msg string) {
	rc, _, _ := errorEntry(kind)
	writeRPCResponse(w, code, nil, rc, msg, a2a.ErrorReason(kind))
}

// writeRPCResponse writes a JSON-RPC error response: the A2A code, the
// message and, in data, the google.rpc.ErrorInfo a2a-go puts there.
func writeRPCResponse(w http.ResponseWriter, status int, id any, code int, msg, reason string) {
	type errorInfo struct {
		Type   string `json:"@type"`
		Reason string `json:"reason"`
		Domain string `json:"domain"`
	}
	type rpcError struct {
		Code    int         `json:"code"`
		Message string      `json:"message"`
		Data    []errorInfo `json:"data"`
	}
	writeJSON(w, status, struct {
		JSONRPC string   `json:"jsonrpc"`
		ID      any      `json:"id"`
		Error   rpcError `json:"error"`
	}{"2.0", id, rpcError{Code: code, Message: msg,
		Data: []errorInfo{{Type: "type.googleapis.com/google.rpc.ErrorInfo", Reason: reason, Domain: a2a.ProtocolDomain}}}})
}

func grpcStatus(code int) string {
	switch code {
	case http.StatusBadRequest, http.StatusUnsupportedMediaType, http.StatusRequestEntityTooLarge, http.StatusMisdirectedRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	}
	return "INTERNAL"
}

// httpStatusOf is the status a plain JSON route answers an A2A error with.
func httpStatusOf(err error) int {
	switch {
	case errors.Is(err, a2a.ErrInvalidParams), errors.Is(err, a2a.ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, a2a.ErrTaskNotFound):
		return http.StatusNotFound
	case errors.Is(err, a2a.ErrServerError):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
