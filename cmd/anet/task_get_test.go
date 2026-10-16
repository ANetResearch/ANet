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
	for _, bad := range [][]string{{}, {"get"}, {"watch"}, {"get", "a", "b"}, {"get", "ix_1", "--history", "x"},
		{"list", "x"}, {"list", "--role", "boss"}, {"cancel"}, {"cancel", "a", "b"}} {
		if err := taskCommand(c, bad); !isUsage(err) {
			t.Errorf("%v: %v, want a usage error", bad, err)
		}
	}
	if len(bodies) != n {
		t.Fatal("a malformed command reached the daemon")
	}
	if checkFlags("task", []string{"get", "ix_1", "--al"}) == nil {
		t.Fatal("an unknown flag was accepted")
	}
}

// `anet task list` is the control plane's /tasks/list with the roles of MCP list_tasks — requester
// for the tasks this node sent (outbound), provider for those sent to it (inbound) — and its state
// and peer filters as given; `anet task cancel <id>` is /tasks/cancel (ux §4.2). [mut] the roles
// swapped, or cancel sent elsewhere → red.
func TestTaskListAndCancelAskTheirRoutes(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		paths, bodies = append(paths, r.URL.Path), append(bodies, m)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"ix_1","status":{"state":"TASK_STATE_CANCELED"},"tasks":[]}`))
	}))
	t.Cleanup(srv.Close)
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	for _, tc := range []struct {
		args []string
		path string
		want map[string]any
	}{
		{[]string{"list"}, "/tasks/list", map[string]any{"max_task_bytes": float64(cliTaskBytes)}},
		{[]string{"list", "--role", "requester"}, "/tasks/list", map[string]any{"role": "outbound", "max_task_bytes": float64(cliTaskBytes)}},
		{[]string{"list", "--role", "provider", "--state", "input-required", "--peer", wholeAID}, "/tasks/list",
			map[string]any{"role": "inbound", "state": "input-required", "peer": wholeAID, "max_task_bytes": float64(cliTaskBytes)}},
		{[]string{"cancel", "ix_1"}, "/tasks/cancel", map[string]any{"task_id": "ix_1", "max_task_bytes": float64(cliTaskBytes)}},
	} {
		n := len(bodies)
		_, _ = captureCLI(t, func() {
			if err := taskCommand(c, tc.args); err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
		})
		mu.Lock()
		if len(bodies) != n+1 || paths[n] != tc.path || !reflect.DeepEqual(bodies[n], tc.want) {
			t.Errorf("%v: sent %v %v, want %s %v", tc.args, paths[len(paths)-1], bodies[len(bodies)-1], tc.path, tc.want)
		}
		mu.Unlock()
	}
}
