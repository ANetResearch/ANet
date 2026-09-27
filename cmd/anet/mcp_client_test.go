//go:build !no_mcp

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/mcpserv"
)

// A refusal reaches the tools as a DaemonError carrying the daemon's
// sentence and the names it gave, so a tool can tell "no such task" from a
// daemon that is down; a success is the daemon's bytes, unchanged.
func TestControlClientKeepsTheDaemonsAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/tasks/get":
			_, _ = w.Write([]byte(`{"id":"ix-1","metadata":{"anet.state_seq":9007199254740993}}`))
		case "/tasks/pay":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"over the agent limit","reason":"agent_max"}`))
		case "/tasks/old":
			http.NotFound(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"task ix-9 not found","code":"TaskNotFoundError"}`))
		}
	}))
	defer srv.Close()
	cc := &controlClient{c: &client{base: srv.URL, token: "tok", timeout: 5 * time.Second}}
	ctx := context.Background()

	var raw json.RawMessage
	if err := cc.Call(ctx, "/tasks/get", map[string]any{"task_id": "ix-1"}, &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Errorf("the answer was re-encoded: %s", raw)
	}

	err := cc.Call(ctx, "/tasks/reply", map[string]any{}, &raw)
	var de *mcpserv.DaemonError
	if !errors.As(err, &de) || de.Status != 404 || de.Code != "TaskNotFoundError" || !strings.Contains(err.Error(), "task ix-9 not found") {
		t.Errorf("404: %#v", err)
	}
	err = cc.Call(ctx, "/tasks/pay", map[string]any{}, &raw)
	if !errors.As(err, &de) || de.Status != 403 || de.Reason != "agent_max" {
		t.Errorf("403: %#v", err)
	}
	// A route this daemon does not have answers in plain text; its words
	// still reach the tool rather than a bare status.
	err = cc.Call(ctx, "/tasks/old", map[string]any{}, &raw)
	if !errors.As(err, &de) || de.Status != 404 || !strings.Contains(err.Error(), "404 page not found") {
		t.Errorf("plain-text 404: %#v", err)
	}

	// A canceled tool call ends the request.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := cc.Call(cctx, "/tasks/get", map[string]any{}, &raw); err == nil {
		t.Error("a canceled call still went through")
	}
}
