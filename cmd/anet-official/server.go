package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// capability is one callable thing this backend serves, and everything a
// daemon's configuration needs to know to mount it.
type capability struct {
	ID    string
	Group string
	// Name, Description, Tags and Examples describe the capability as an
	// A2A skill; they go into the daemon's service configuration
	// (provider.Described) and so onto the node's card.
	Name        string
	Description string
	Tags        []string
	Examples    []string
	// MaxArgsBytes bounds the JSON arguments. The daemon's service module
	// sends exactly the bytes the kernel measured for max_args_bytes
	// (json.Marshal of the arguments), so the same number is the body limit
	// here and max_args_bytes in public_capabilities: a call the kernel
	// admits is never refused here for size, and a call refused here never
	// got past the kernel on a correctly configured node.
	MaxArgsBytes int
	// Timeout bounds one call here, and is the capability's timeout_ms in
	// the service configuration.
	Timeout time.Duration
	// Quota is the suggested public_capabilities entry. The kernel enforces
	// it; this backend only publishes the suggestion.
	Quota quota
	// Price is the suggested price in credit; zero is free. The daemon's
	// configuration decides, not this number.
	Price uint64
	// Handle computes the answer. It must be deterministic in its
	// arguments except where the description says otherwise (net.echo
	// reports when it received the call), must not touch the network or
	// the file system, and must stop when ctx is done.
	Handle func(ctx context.Context, e *env, args []byte) (any, error)
}

type quota struct {
	PerCallerPerMin int
	PerCallerPerDay int
	GlobalPerMin    int
	MaxInflight     int
}

// env is what handlers may use besides their arguments.
type env struct {
	version string
	now     func() time.Time
	corpus  *corpus
}

// argError is a refusal of the arguments: the caller sent something this
// capability does not take. It is answered with 400 and the message, which
// the daemon passes back to the caller as the reason for FAILED.
type argError struct{ msg string }

func (e *argError) Error() string { return e.msg }

func badArgs(format string, a ...any) error { return &argError{msg: fmt.Sprintf(format, a...)} }

// errBudget is returned by a handler that stopped because its work budget
// or its deadline ran out: the input was acceptable, and this call could
// not finish it, which a caller must be able to tell apart from a wrong
// answer. The server answers 422 when the step budget ran out (the same
// input will run out again) and 503 when the deadline passed first (it may
// be load, and a retry may succeed); the service module reports 503 as
// UNAVAILABLE.
var errBudget = errors.New("the computation exceeded this capability's budget")

// server serves the capabilities of the selected groups.
type server struct {
	env    *env
	caps   map[string]*capability // route path -> capability
	token  [sha256.Size]byte      // SHA-256 of the bearer token
	logger *log.Logger
	// inflight bounds concurrent calls across all routes. The kernel bounds
	// each public capability; this bounds the process, so a daemon
	// misconfigured with generous limits cannot make it hold unbounded
	// work.
	inflight chan struct{}
}

// maxInflight is the process-wide bound on concurrent calls.
const maxInflight = 64

func newServer(e *env, caps []*capability, token string, logger *log.Logger) *server {
	s := &server{env: e, caps: map[string]*capability{}, token: sha256.Sum256([]byte(token)),
		logger: logger, inflight: make(chan struct{}, maxInflight)}
	for _, c := range caps {
		s.caps[routeOf(c)] = c
	}
	return s
}

// routeOf is the path a capability is served at: one route group per
// capability group, so a backend started for one group has no route for
// another.
func routeOf(c *capability) string { return "/v1/" + c.Group + "/" + c.ID }

// Headers the daemon's service module sets (module/service). Repeated here
// rather than imported: this binary is the far side of an HTTP contract and
// must not link the daemon to learn it.
const (
	headerCaller = "X-ANet-Caller"
	headerCall   = "X-ANet-Call"
	headerVia    = "X-ANet-Via"
)

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only a loopback Host is answered. A page in a browser on this
	// machine can make the browser send requests here; a Host check means a
	// DNS name rebound to 127.0.0.1 does not get an answer, whatever else
	// the request carries.
	if !loopbackHost(r.Host) {
		writeError(w, http.StatusMisdirectedRequest, "misdirected", "this service answers only on a loopback address")
		return
	}
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.env.version, "corpus_cid": s.env.corpus.CID})
		return
	}
	c, ok := s.caps[r.URL.Path]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no capability at "+r.URL.Path)
		return
	}
	if !s.authorized(r) {
		// No detail: whether the header was missing, malformed or wrong is
		// of use only to someone guessing. Logged, because on a host where
		// only the daemon holds the token, a refusal is something else on
		// the host knocking.
		s.logCall(r, c, http.StatusUnauthorized, 0, 0, s.env.now())
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or wrong bearer token")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "content_type", "arguments must be application/json")
		return
	}
	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	default:
		writeError(w, http.StatusServiceUnavailable, "busy", "too many calls in progress")
		return
	}

	started := s.env.now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(c.MaxArgsBytes)))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.logCall(r, c, http.StatusRequestEntityTooLarge, 0, 0, started)
			writeError(w, http.StatusRequestEntityTooLarge, "args_too_large",
				fmt.Sprintf("arguments exceed %d bytes", c.MaxArgsBytes))
			return
		}
		writeError(w, http.StatusBadRequest, "read", "reading the arguments failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), c.Timeout)
	defer cancel()
	res, err := runHandler(ctx, c, s.env, body)
	status := http.StatusOK
	var out []byte
	switch {
	case err == nil:
		out, err = marshalResult(res)
		if err != nil {
			status = http.StatusInternalServerError
			out = errorBody("internal", "encoding the result failed")
		}
	case errors.As(err, new(*argError)):
		status, out = http.StatusBadRequest, errorBody("bad_args", err.Error())
	case ctx.Err() != nil:
		status, out = http.StatusServiceUnavailable, errorBody("timeout",
			fmt.Sprintf("the call did not finish within %s", c.Timeout))
	case errors.Is(err, errBudget):
		status, out = http.StatusUnprocessableEntity, errorBody("too_complex", errBudget.Error())
	default:
		status, out = http.StatusInternalServerError, errorBody("internal", "the capability failed")
		s.logger.Printf("%s: internal error: %v", c.ID, err)
	}
	s.logCall(r, c, status, len(body), len(out), started)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// runHandler runs a handler, turning a panic into an error: one bad input
// must not take the other capabilities of this process down with it.
func runHandler(ctx context.Context, c *capability, e *env, body []byte) (res any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic in %s: %v", c.ID, p)
		}
	}()
	return c.Handle(ctx, e, body)
}

// authorized compares the bearer token in constant time. Both sides are
// hashed first, so the comparison takes the same time whatever the length
// of the presented token.
func (s *server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(h[len(prefix):]))
	return subtle.ConstantTimeCompare(got[:], s.token[:]) == 1
}

// logCall writes one line per call: who, which capability, how it ended,
// how many bytes each way and how long it took. Never the arguments or the
// result — this process keeps no copy of what it was asked or answered
// (deploy/official/README.md, content policy).
func (s *server) logCall(r *http.Request, c *capability, status, in, out int, started time.Time) {
	caller := r.Header.Get(headerCaller)
	if caller == "" {
		caller = "-"
	}
	call := r.Header.Get(headerCall)
	if call == "" {
		call = "-"
	}
	via := r.Header.Get(headerVia)
	if via == "" {
		via = "-"
	}
	s.logger.Printf("call cap=%s status=%d caller=%s call=%s via=%s in=%d out=%d ms=%d",
		c.ID, status, safeLogField(caller), safeLogField(call), safeLogField(via), in, out,
		s.env.now().Sub(started).Milliseconds())
}

// safeLogField keeps a header value on one log line and bounded.
func safeLogField(v string) string {
	if len(v) > 128 {
		v = v[:128]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x21 || r == 0x7f {
			return '_'
		}
		return r
	}, v)
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// marshalResult encodes a result without HTML escaping: the results carry
// documentation and diffs, where "<" is text, and nothing here is rendered
// by a browser.
func marshalResult(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func errorBody(code, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": msg}})
	return b
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(errorBody(code, msg))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "encoding failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// decodeArgs decodes a capability's JSON arguments into dst, refusing
// members dst does not declare: a misspelt option silently ignored is a
// caller who believes they asked for something they did not get.
func decodeArgs(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badArgs("arguments: %v", err)
	}
	if dec.More() {
		return badArgs("arguments: trailing data after the JSON object")
	}
	return nil
}
