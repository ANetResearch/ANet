package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-0123456789abcdef"

// testEnv is an env with a fixed clock and the compiled-in corpus.
func testEnv(t *testing.T) *env {
	t.Helper()
	c, err := loadCorpus()
	if err != nil {
		t.Fatal(err)
	}
	return &env{version: "test", now: func() time.Time { return time.UnixMilli(1_790_000_000_000) }, corpus: c}
}

// testServer serves the given groups with testToken and a captured log.
func testServer(t *testing.T, groups string) (*server, *bytes.Buffer) {
	t.Helper()
	caps, err := selectGroups(groups)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	return newServer(testEnv(t), caps, testToken, log.New(&logs, "", 0)), &logs
}

type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func withHost(h string) reqOpt      { return func(r *http.Request) { r.Host = h } }
func withMethod(m string) reqOpt    { return func(r *http.Request) { r.Method = m } }

// do sends one request the way the service module does, with opts
// changing it.
func do(t *testing.T, s *server, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8613"+path, strings.NewReader(body))
	r.Host = "127.0.0.1:8613"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+testToken)
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// call invokes a capability through the server and decodes a 200 answer.
func call(t *testing.T, s *server, capID, body string) map[string]any {
	t.Helper()
	var c *capability
	for _, x := range allCapabilities() {
		if x.ID == capID {
			c = x
		}
	}
	if c == nil {
		t.Fatalf("no capability %s", capID)
	}
	w := do(t, s, routeOf(c), body)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: HTTP %d %s", capID, body, w.Code, w.Body.String())
	}
	var out map[string]any
	dec := json.NewDecoder(w.Body)
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("%s: reply is not a JSON object: %v", capID, err)
	}
	return out
}

// The daemon is the only door: a request without the token, with the
// wrong one, to a non-loopback Host, or in the wrong shape is refused
// before any capability runs.
func TestOnlyTheDaemonGetsIn(t *testing.T) {
	s, _ := testServer(t, "echo")
	const path = "/v1/echo/net.echo"
	cases := []struct {
		name string
		opts []reqOpt
		want int
	}{
		{"ok", nil, http.StatusOK},
		{"no token", []reqOpt{withHeader("Authorization", "")}, http.StatusUnauthorized},
		{"wrong token", []reqOpt{withHeader("Authorization", "Bearer "+testToken+"x")}, http.StatusUnauthorized},
		{"token prefix", []reqOpt{withHeader("Authorization", "Bearer "+testToken[:10])}, http.StatusUnauthorized},
		{"not bearer", []reqOpt{withHeader("Authorization", "Basic "+testToken)}, http.StatusUnauthorized},
		{"lowercase bearer", []reqOpt{withHeader("Authorization", "bearer "+testToken)}, http.StatusOK},
		{"rebound host", []reqOpt{withHost("evil.example:8613")}, http.StatusMisdirectedRequest},
		{"public host", []reqOpt{withHost("10.0.0.1:8613")}, http.StatusMisdirectedRequest},
		{"localhost", []reqOpt{withHost("localhost:8613")}, http.StatusOK},
		{"ipv6 loopback", []reqOpt{withHost("[::1]:8613")}, http.StatusOK},
		{"GET", []reqOpt{withMethod(http.MethodGet)}, http.StatusMethodNotAllowed},
		{"form post", []reqOpt{withHeader("Content-Type", "text/plain")}, http.StatusUnsupportedMediaType},
	}
	for _, c := range cases {
		if w := do(t, s, path, `{"a":1}`, c.opts...); w.Code != c.want {
			t.Errorf("%s: HTTP %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
	}
	// Without the token every path answers alike, so the routes of an
	// instance cannot be probed.
	for _, p := range []string{path, "/v1/tools/text.digest", "/nope"} {
		if w := do(t, s, p, `{}`, withHeader("Authorization", "")); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: HTTP %d, want 401", p, w.Code)
		}
	}
}

// A backend started for one group has no route for another, so a token
// opens only its own identity's capabilities.
func TestGroupsLimitTheRoutes(t *testing.T) {
	s, _ := testServer(t, "echo")
	if w := do(t, s, "/v1/tools/text.digest", `{"text":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("tools route on an echo backend: HTTP %d", w.Code)
	}
	if w := do(t, s, "/v1/echo/text.digest", `{"text":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("a capability under another group's prefix: HTTP %d", w.Code)
	}
	for _, bad := range []string{"", "nope", "echo,nope", " , "} {
		if _, err := selectGroups(bad); err == nil {
			t.Errorf("groups %q must be refused", bad)
		}
	}
	all, err := selectGroups("echo,tools,docs,paid")
	if err != nil || len(all) != len(allCapabilities()) {
		t.Errorf("all groups: %d capabilities, %v", len(all), err)
	}
}

// Arguments over the capability's limit are refused with 413 before they
// are read in full; arguments at the limit are served.
func TestArgumentLimit(t *testing.T) {
	s, _ := testServer(t, "echo")
	pad := func(n int) string { // a JSON object of exactly n bytes
		return `{"p":"` + strings.Repeat("x", n-8) + `"}`
	}
	if w := do(t, s, "/v1/echo/net.echo", pad(4096)); w.Code != http.StatusOK {
		t.Errorf("4096 bytes: HTTP %d %s", w.Code, w.Body.String())
	}
	if w := do(t, s, "/v1/echo/net.echo", pad(4097)); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("4097 bytes: HTTP %d", w.Code)
	}
}

// Bad arguments are the caller's error (400, with the reason); a panic in
// one capability is the backend's (500) and does not take the process
// down.
func TestErrorsAreClassified(t *testing.T) {
	s, _ := testServer(t, "tools")
	w := do(t, s, "/v1/tools/text.digest", `{"text":"x","algorithm":"sha256"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "algorithm") {
		t.Errorf("a misspelt option must be refused, naming it: HTTP %d %s", w.Code, w.Body.String())
	}
	boom := &capability{ID: "x.boom", Group: "tools", MaxArgsBytes: 64, Timeout: time.Second,
		Handle: func(context.Context, *env, []byte) (any, error) { panic("boom") }}
	s.caps[routeOf(boom)] = boom
	if w := do(t, s, routeOf(boom), `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("panic: HTTP %d", w.Code)
	}
	slow := &capability{ID: "x.slow", Group: "tools", MaxArgsBytes: 64, Timeout: 10 * time.Millisecond,
		Handle: func(ctx context.Context, _ *env, _ []byte) (any, error) { <-ctx.Done(); return nil, ctx.Err() }}
	s.caps[routeOf(slow)] = slow
	if w := do(t, s, routeOf(slow), `{}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("deadline: HTTP %d, want 503 (not a wrong answer, an unfinished one)", w.Code)
	}
	heavy := &capability{ID: "x.heavy", Group: "tools", MaxArgsBytes: 64, Timeout: time.Second,
		Handle: func(context.Context, *env, []byte) (any, error) { return nil, errBudget }}
	s.caps[routeOf(heavy)] = heavy
	if w := do(t, s, routeOf(heavy), `{}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("step budget: HTTP %d, want 422 (the same input will run out again)", w.Code)
	}
	// A result the service module could not read whole is refused here,
	// with the reason, rather than cut into something that is not JSON.
	huge := &capability{ID: "x.huge", Group: "tools", MaxArgsBytes: 64, Timeout: time.Second,
		Handle: func(context.Context, *env, []byte) (any, error) {
			return map[string]string{"x": strings.Repeat("y", maxResultBytes)}, nil
		}}
	s.caps[routeOf(huge)] = huge
	if w := do(t, s, routeOf(huge), `{}`); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "result_too_large") {
		t.Errorf("oversized result: HTTP %d %.200s", w.Code, w.Body.String())
	}
}

// The call log names who called what and how it ended, and never what
// was asked or answered (deploy/official/README.md §5).
func TestTheLogHoldsNoContent(t *testing.T) {
	s, logs := testServer(t, "tools")
	const secret = "canary-7f3a9c-secret-text"
	do(t, s, "/v1/tools/text.digest", `{"text":"`+secret+`"}`,
		withHeader("X-ANet-Caller", "aid-caller\nforged line"), withHeader("X-ANet-Call", "ix-1"), withHeader("X-ANet-Via", "relay"))
	do(t, s, "/v1/tools/text.digest", `{"text":"`+secret+`"}`, withHeader("Authorization", "Bearer wrong-token-0123456789"))
	got := logs.String()
	if !strings.Contains(got, "status=401") {
		t.Errorf("a refused token must be logged: %s", got)
	}
	if strings.Contains(got, secret) || strings.Contains(got, "2cf24dba") {
		t.Fatalf("the log holds call content: %s", got)
	}
	if !strings.Contains(got, "cap=text.digest") || !strings.Contains(got, "call=ix-1") || !strings.Contains(got, "via=relay") {
		t.Errorf("the log line lacks the call's coordinates: %s", got)
	}
	if strings.Count(strings.TrimSpace(got), "\n") != 1 {
		t.Errorf("two calls, two lines; a header value must not add lines: %q", got)
	}
}

func TestHealthz(t *testing.T) {
	s, _ := testServer(t, "echo")
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8611/healthz", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), s.env.corpus.CID) {
		t.Errorf("healthz: HTTP %d %s", w.Code, w.Body.String())
	}
}

func TestListenAddressMustBeLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8611": "127.0.0.1:8611",
		"localhost:8611": "127.0.0.1:8611",
		"[::1]:8611":     "[::1]:8611",
		"127.0.0.2:8611": "127.0.0.2:8611",
	} {
		got, err := loopbackListenAddr(in)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"0.0.0.0:8611", ":8611", "10.0.0.1:8611", "[::]:8611", "example.com:8611", "127.0.0.1:99999", "127.0.0.1"} {
		if _, err := loopbackListenAddr(in); err == nil {
			t.Errorf("%s must be refused", in)
		}
	}
}

func TestTokenFileRules(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good", testToken+"\n", 0o600)
	if tok, err := readToken(good); err != nil || tok != testToken {
		t.Errorf("good token: %q %v", tok, err)
	}
	t.Setenv("ANET_TEST_CRED", dir)
	if tok, err := readToken("${ANET_TEST_CRED}/good"); err != nil || tok != testToken {
		t.Errorf("expanded path: %q %v", tok, err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"world-readable": write("wr", testToken, 0o604),
		"short":          write("short", "abc", 0o600),
		"inner space":    write("space", "0123456789 abcdef0123", 0o600),
		"relative":       "good",
		"missing":        filepath.Join(dir, "missing"),
		"directory":      sub,
		// Unset, "${CREDENTIALS_DIRECTORY}/token" would name /token.
		"unset variable": "${ANET_TEST_UNSET_CRED}/good",
	} {
		if _, err := readToken(p); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}
