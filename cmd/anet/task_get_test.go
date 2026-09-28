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

// `anet task get <id>` is where a cut task says the whole of it is read
// (a2ashape.TaskReadWithin, MCP get_task): without --full it asks the
// control plane for the task held to the MCP bound, with --full for all of
// it, whichever side of the id the flag is on.
func TestTaskGetAsksForTheBoundOrTheWholeTask(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		if r.URL.Path != "/tasks/get" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"ix_1"}`))
	}))
	t.Cleanup(srv.Close)
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	for _, tc := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"get", "ix_1"}, map[string]any{"task_id": "ix_1", "max_task_bytes": float64(cliTaskBytes)}},
		{[]string{"get", "ix_1", "--full"}, map[string]any{"task_id": "ix_1"}},
		{[]string{"get", "--full", "ix_1"}, map[string]any{"task_id": "ix_1"}},
		{[]string{"get", "ix_1", "--history", "2"}, map[string]any{"task_id": "ix_1", "history_length": 2.0,
			"max_task_bytes": float64(cliTaskBytes)}},
	} {
		if err := checkFlags("task", tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		n := len(bodies)
		if err := taskCommand(c, tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if len(bodies) != n+1 || !reflect.DeepEqual(bodies[n], tc.want) {
			t.Fatalf("%v: sent %v, want %v", tc.args, bodies[len(bodies)-1], tc.want)
		}
	}
	n := len(bodies)
	for _, bad := range [][]string{{}, {"get"}, {"list"}, {"get", "a", "b"}, {"get", "ix_1", "--history", "x"}} {
		if err := taskCommand(c, bad); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
	if len(bodies) != n {
		t.Fatal("a malformed command reached the daemon")
	}
	if checkFlags("task", []string{"get", "ix_1", "--al"}) == nil {
		t.Fatal("an unknown flag was accepted")
	}
}
