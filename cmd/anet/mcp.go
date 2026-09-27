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
	resolve := daemon.ResolveControl
	if explicit {
		resolve = daemon.ResolveControlStrict
	}
	base, token, err := resolve(layout)
	if err != nil {
		return diagnoseNoDaemon("http://"+daemon.LocalControlAddr(layout), layout.Root, err)
	}
	c := &controlClient{c: &client{base: base, token: token,
		timeout: 15 * time.Minute, dataDir: layout.Root}}

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
type controlClient struct{ c *client }

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
func (cc *controlClient) fetch(ctx context.Context, path string, body any) ([]byte, int, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cc.c.base+path, &buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cc.c.token)
	resp, err := (&http.Client{Timeout: cc.c.timeout}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return out, resp.StatusCode, err
}
