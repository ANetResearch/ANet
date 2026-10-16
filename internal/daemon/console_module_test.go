package daemon

import (
	"net/http"
	"strings"
	"testing"
)

// MOD-8, TestWithoutConsoleHasNoSessionPath: with `without: [console]` the page and the session routes
// answer 409 module_absent, a session cookie is a 401 whatever it names, and the only public route is
// /ping. Bearer callers of the kernel routes are not affected.
func TestWithoutConsoleHasNoSessionPath(t *testing.T) {
	absent := newPlaneFor(t, startWith(t, map[string]any{"without": []string{"console"}}, nil), "tok")
	for _, route := range []string{"GET /", "GET /console", "POST /console/session", "POST /console/ticket", "POST /console/switch"} {
		method, path, _ := strings.Cut(route, " ")
		resp, raw := absent.req(t, method, path, "{}", absent.bearer)
		if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "module_absent") {
			t.Errorf("%s: %d %s", route, resp.StatusCode, raw)
		}
		if !strings.Contains(string(raw), "console") {
			t.Errorf("%s: the sentence does not name the module: %s", route, raw)
		}
	}
	// A session that exists anyway (planted straight into the store) is no session.
	id, csrf, _, err := absent.cp.sessions.createSession()
	if err != nil {
		t.Fatal(err)
	}
	withCookie := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "anet_s_" + absent.port, Value: id})
		r.Header.Set("X-Anet-CSRF", csrf)
		r.Header.Set("Origin", absent.origin())
	}
	for pat := range sessionRoutes {
		method, path := splitPattern(pat)
		if resp, raw := absent.req(t, method, path, "{}", withCookie); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a session cookie: %d %s", pat, resp.StatusCode, raw)
		}
	}
	if got := absent.d.publicRouteSet(); len(got) != 1 || !got["GET /ping"] {
		t.Errorf("public routes %v", got)
	}
	if len(absent.d.sessionRouteSet()) != 0 {
		t.Error("session routes not empty")
	}
	if resp, raw := absent.req(t, "GET", "/ping", "", nil); resp.StatusCode != 200 {
		t.Errorf("/ping: %d %s", resp.StatusCode, raw)
	}
	// Bearer callers keep the kernel routes.
	if resp, raw := absent.req(t, "GET", "/status", "", absent.bearer); resp.StatusCode != 200 {
		t.Errorf("/status with bearer: %d %s", resp.StatusCode, raw)
	}

	// The default: the console works as before.
	present := newPlaneFor(t, startWith(t, nil, nil), "tok")
	s := present.session(t)
	if resp, raw := present.req(t, "GET", "/status", "", s.as(present)); resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusConflict {
		t.Errorf("present: session /status %d %s", resp.StatusCode, raw)
	}
	if len(present.d.publicRouteSet()) != len(publicRoutes) || len(present.d.sessionRouteSet()) != len(sessionRoutes) {
		t.Error("present daemon's route sets changed")
	}
}
