//go:build !no_mcp

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/mcpserv"
)

// servedRotatingDaemon is servedDaemon that also returns the daemon, so a test can rotate its local
// credentials. The control token is the one in the data directory's token file, as on a node.
func servedRotatingDaemon(t *testing.T) (daemon.Layout, *daemon.Daemon) {
	t.Helper()
	t.Setenv("ANET_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"control_addr": ln.Addr().String()})
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := daemon.NewLayout(root)
	d, err := daemon.New(layout)
	if err != nil {
		t.Fatal(err)
	}
	const token = "cli-test-token-before-rotation"
	if err := os.WriteFile(layout.ControlTokenPath(), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: d.ControlHandler(token), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); d.Close() })
	return layout, d
}

// A running `anet mcp` keeps working after the daemon replaces its control token (W3 EXE-5 2): the
// emergency rotation calls rotateLocalTokens (LEC AV-10), and from then on the old token is refused. The
// MCP process started with the old one; on the 401 it reads the token file again and repeats the call
// once, so the coding agent's session does not have to be restarted. [mut] no reread on 401 → the second
// node_status is a tool error, and this is red.
func TestMCPSurvivesTokenRotation(t *testing.T) {
	layout, d := servedRotatingDaemon(t)
	cc, err := newControlClient(layout, true)
	if err != nil {
		t.Fatal(err)
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := mcpserv.New(cc, "test").Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	nodeStatus := func(step string) {
		t.Helper()
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "node_status", Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if res.IsError {
			var text []string
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					text = append(text, tc.Text)
				}
			}
			t.Fatalf("%s: node_status failed: %s", step, strings.Join(text, " "))
		}
	}
	nodeStatus("before the rotation")
	old := cc.current()

	if err := d.RotateLocalTokens(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(layout.ControlTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	fresh := strings.TrimSpace(string(b))
	if fresh == old.token || fresh == "" {
		t.Fatalf("the rotation did not replace the token file (%q)", fresh)
	}
	// The rotation is in force: the token the MCP process started with is refused.
	if _, code, err := post(context.Background(), old, "/status", []byte("{}")); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("the old token after the rotation: %d %v, want 401", code, err)
	}

	nodeStatus("after the rotation")
	if got := cc.current(); got.token != fresh || got.base != old.base {
		t.Fatalf("the client sends %q to %s, want the new token to %s", got.token, got.base, old.base)
	}
	// And it stays with the new token: the next call needs no second try.
	nodeStatus("again")
}

// The second try happens once, and only on a 401: a token that the file still holds (nothing was
// rotated) is not sent again, and a refusal of another kind is the daemon's answer as it is.
func TestControlClientRetriesOnlyANewToken(t *testing.T) {
	calls := &tokenCalls{n: map[string]int{}}
	srv := newTokenServer(t, "current", calls)
	cc := &controlClient{c: &client{base: srv, token: "stale", timeout: 5 * time.Second},
		resolve: func() (string, string, error) { return srv, "stale", nil }}
	if _, code, err := cc.fetch(context.Background(), "/status", map[string]any{}); err != nil || code != 401 ||
		calls.get("stale") != 1 {
		t.Fatalf("the same token again: %d %v, calls %v", code, err, calls)
	}
	cc.resolve = func() (string, string, error) { return srv, "current", nil }
	if _, code, err := cc.fetch(context.Background(), "/status", map[string]any{}); err != nil || code != 200 ||
		calls.get("stale") != 2 || calls.get("current") != 1 {
		t.Fatalf("a new token: %d %v, calls %v", code, err, calls)
	}
	if _, code, err := cc.fetch(context.Background(), "/forbidden", map[string]any{}); err != nil || code != 403 ||
		calls.get("current") != 2 {
		t.Fatalf("a 403 is not retried: %d %v, calls %v", code, err, calls)
	}
}

// tokenCalls counts the calls each token made.
type tokenCalls struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *tokenCalls) add(tok string) { c.mu.Lock(); c.n[tok]++; c.mu.Unlock() }

func (c *tokenCalls) get(tok string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.n[tok] }

func (c *tokenCalls) String() string { c.mu.Lock(); defer c.mu.Unlock(); return fmt.Sprint(c.n) }

// newTokenServer is a control plane that accepts one token and counts the calls each token made.
func newTokenServer(t *testing.T, accept string, calls *tokenCalls) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		calls.add(tok)
		switch {
		case tok != accept:
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/forbidden":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"no","reason":"denied"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}
