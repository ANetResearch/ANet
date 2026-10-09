//go:build !no_mcp

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/mcpserv"
)

// runMCP serves the MCP northbound over stdio.
//
// Configure it in an MCP client as the command `anet mcp`. The client
// spawns it, it proxies to this machine's daemon, and it exits when the
// client goes away — the daemon keeps running, keeps the keys, and keeps
// the ledger.
//
// It refuses to start without a daemon rather than starting and failing
// every call. An MCP client that connects successfully and then errors on
// each tool teaches the model that the tools are broken; one that fails to
// connect sends the operator to look at the daemon, which is where the
// problem is.
//
// With an explicitly selected identity (--id, ANET_ID, ANET_HOME, ANET_DATA_DIR or a non-default
// `anet id use`) it resolves strictly: only that identity's own daemon, never the uid-wide pointer to
// whichever daemon started last (A2A-DESIGN §7.8). An MCP client configured for one identity must not
// end up driving another.
func runMCP(layout daemon.Layout, explicit bool) error {
	c, err := newControlClient(layout, explicit)
	if err != nil {
		return err
	}

	// A quick liveness check, for the reason above.
	if err := c.Call(context.Background(), "/status", map[string]any{}, new(json.RawMessage)); err != nil {
		return fmt.Errorf("anet mcp: the daemon is not answering: %w", err)
	}
	srv := mcpserv.New(c, daemon.Version)
	err = srv.Run(context.Background(), &mcp.StdioTransport{})
	// A client that goes away is how this process is supposed to end. It
	// is spawned per session and dies with it, so reporting the
	// disconnection as a failure would put an error in the operator's log
	// every single time the tool worked.
	if err != nil && (errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF")) {
		return nil
	}
	return err
}

// controlClient adapts the CLI's control-plane client to the narrow face
// the tool surface is allowed to reach.
//
// It outlives the control token it started with. `anet mcp` runs as long as
// the coding agent's session, and the daemon replaces its token on a
// restart and when it rotates its local credentials (rotateLocalTokens,
// after an emergency rotation, LEC AV-10): from then on the old token is
// answered 401, and a session that kept it would fail every tool call until
// it was restarted. On a 401 the client finds the daemon again the way
// `anet mcp` did at start — the token file of the same identity — and, when
// that gives another token or address, sends the call once more with it. A
// 401 is answered before a route does anything, so the second attempt is
// the first one the daemon acts on.
type controlClient struct {
	mu sync.Mutex
	c  *client
	// resolve finds the daemon again: base and token, as at start; nil
	// keeps the token it has.
	resolve func() (base, token string, err error)
}

// newControlClient is the control client of `anet mcp` for layout: with an
// explicitly selected identity it resolves strictly, only that identity's
// own daemon (A2A-DESIGN §7.8), the first time and after a 401.
func newControlClient(layout daemon.Layout, explicit bool) (*controlClient, error) {
	resolve := daemon.ResolveControl
	if explicit {
		resolve = daemon.ResolveControlStrict
	}
	base, token, err := resolve(layout)
	if err != nil {
		return nil, diagnoseNoDaemon("http://"+daemon.LocalControlAddr(layout), layout.Root, err)
	}
	return &controlClient{
		c:       &client{base: base, token: token, timeout: 15 * time.Minute, dataDir: layout.Root},
		resolve: func() (string, string, error) { return resolve(layout) },
	}, nil
}

// current is the client to send with now.
func (cc *controlClient) current() client {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return *cc.c
}

// refreshed finds the daemon again after a 401 that used was answered
// with: the client to send with, and whether it differs from used (another
// token or address). Concurrent calls that were all refused share one
// lookup's result.
func (cc *controlClient) refreshed(used client) (client, bool) {
	if cc.resolve == nil {
		return used, false
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.c.token != used.token || cc.c.base != used.base {
		// Another call found the new token already.
		return *cc.c, true
	}
	base, token, err := cc.resolve()
	if err != nil || (token == used.token && base == used.base) {
		return used, false
	}
	cc.c.base, cc.c.token = base, token
	return *cc.c, true
}

func (cc *controlClient) Call(ctx context.Context, path string, body, out any) error {
	raw, code, err := cc.fetch(ctx, path, body)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		// Hand the daemon's own message through, with the names it gave:
		// it is written for a human and reads correctly to a model too, and
		// a tool can tell "no such task" from a daemon that is down.
		var e struct {
			Error  string `json:"error"`
			Code   string `json:"code"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal(raw, &e) != nil {
			// Not the daemon's JSON: a route this daemon does not have
			// ("404 page not found" from an older build) or a proxy in the
			// way. Its first line says which better than a bare status.
			e.Error, _, _ = strings.Cut(strings.TrimSpace(string(raw)), "\n")
			if len(e.Error) > 200 {
				e.Error = strings.ToValidUTF8(e.Error[:200], "")
			}
		}
		return &mcpserv.DaemonError{Status: code, Message: e.Error, Code: e.Code, Reason: e.Reason}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// fetch is client.fetch bound to the tool call's context: a client that
// cancels a call (or goes away) ends the request, rather than leaving a
// wait_task blocked in the daemon until its own bound.
//
// A 401 is retried once with the token found again (see controlClient).
func (cc *controlClient) fetch(ctx context.Context, path string, body any) ([]byte, int, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, 0, err
		}
	}
	used := cc.current()
	out, code, err := post(ctx, used, path, buf.Bytes())
	if err == nil && code == http.StatusUnauthorized {
		if fresh, ok := cc.refreshed(used); ok {
			return post(ctx, fresh, path, buf.Bytes())
		}
	}
	return out, code, err
}

// post sends one control call with c's address and token.
func post(ctx context.Context, c client, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http().Do(req) // verified to be this user's daemon first (client.http)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return out, resp.StatusCode, err
}
