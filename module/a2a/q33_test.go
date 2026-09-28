//go:build !no_a2a

package a2a

// 0017 Q33 (a2a-tck CARD-CACHE-001/002): the proxy cards and the agent list
// may be kept by the client — Cache-Control private (the routes need the
// bearer) with a max-age of five minutes, and an ETag answered 304 on
// If-None-Match. Every other response stays no-store.

import (
	"net/http"
	"testing"
)

func TestCardsAndTheAgentListAreCacheablePrivately(t *testing.T) {
	e := newEnv(t)
	bearer := map[string]string{"Authorization": "Bearer " + testToken}
	for _, path := range []string{
		agentsPath + "/" + agentA + "/.well-known/agent-card.json",
		agentsPath + "/" + agentA,
		agentsPath,
	} {
		resp, body := e.raw("GET", path, bearer, "")
		tag := resp.Header.Get("ETag")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "private, max-age=300" ||
			len(tag) < 3 || tag[0] != '"' || len(body) == 0 {
			t.Fatalf("GET %s: %d, Cache-Control %q, ETag %q", path, resp.StatusCode, resp.Header.Get("Cache-Control"), tag)
		}
		again, _ := e.raw("GET", path, bearer, "")
		if again.Header.Get("ETag") != tag {
			t.Fatalf("GET %s: the same bytes under another ETag: %q, %q", path, tag, again.Header.Get("ETag"))
		}
		for _, inm := range []string{tag, "W/" + tag, `"other", ` + tag, "*"} {
			nm, b := e.raw("GET", path, map[string]string{"Authorization": "Bearer " + testToken, "If-None-Match": inm}, "")
			if nm.StatusCode != http.StatusNotModified || len(b) != 0 || nm.Header.Get("ETag") != tag {
				t.Fatalf("GET %s If-None-Match %s: %d %q", path, inm, nm.StatusCode, b)
			}
		}
		if stale, _ := e.raw("GET", path, map[string]string{"Authorization": "Bearer " + testToken,
			"If-None-Match": `"other"`}, ""); stale.StatusCode != http.StatusOK {
			t.Fatalf("GET %s with another ETag: %d", path, stale.StatusCode)
		}
	}

	// The rest is not kept: a task, an error, a refused request.
	for _, c := range []struct {
		method, path, body string
		hdr                map[string]string
	}{
		{"POST", agentsPath + "/" + agentA + "/jsonrpc", rpcGet,
			map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}},
		{"GET", agentsPath + "/" + selfAID, "", bearer},
		{"GET", agentsPath + "/" + agentA, "", nil},
	} {
		resp, _ := e.raw(c.method, c.path, c.hdr, c.body)
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("ETag") != "" {
			t.Errorf("%s %s: %d, Cache-Control %q, ETag %q", c.method, c.path, resp.StatusCode,
				resp.Header.Get("Cache-Control"), resp.Header.Get("ETag"))
		}
	}
}
