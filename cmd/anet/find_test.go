package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// `anet find` asks the daemon's /agents/list (the v1 /find route is gone):
// the query as q, --cap (either spelling) as skill, the agents without a card
// included. /agents/list reads its body leniently, so a member spelled the
// old way ("query", "capability") would be dropped without a word and the
// search would list every agent; the body is pinned member by member.
func TestFindAsksTheAgentList(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, m)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"agents":[],"nextCursor":""}`))
	}))
	t.Cleanup(srv.Close)
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	base := func(extra map[string]any) map[string]any {
		m := map[string]any{"include_uncarded": true, "limit": float64(findLimit)}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, tc := range []struct {
		args []string
		want map[string]any
	}{
		{nil, base(nil)},
		{[]string{"  "}, base(nil)},
		{[]string{"text", "digest"}, base(map[string]any{"q": "text digest"})},
		{[]string{"--cap", "text.digest"}, base(map[string]any{"skill": "text.digest"})},
		{[]string{"--capability=text.digest"}, base(map[string]any{"skill": "text.digest"})},
		{[]string{"digest", "--cap", "text.digest"}, base(map[string]any{"q": "digest", "skill": "text.digest"})},
	} {
		if err := checkFlags("find", tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		n := len(bodies)
		if err := findCommand(c, tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if len(bodies) != n+1 || paths[n] != "/agents/list" || !reflect.DeepEqual(bodies[n], tc.want) {
			t.Fatalf("%v: sent %v to %v, want %v to /agents/list", tc.args, bodies[len(bodies)-1], paths[len(paths)-1], tc.want)
		}
	}
}
