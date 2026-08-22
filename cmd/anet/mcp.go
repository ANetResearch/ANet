//go:build !no_mcp

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
func runMCP(layout daemon.Layout) error {
	base, token, err := daemon.ResolveControl(layout)
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

func (cc *controlClient) Call(_ context.Context, path string, body, out any) error {
	raw, code, err := cc.c.fetch(path, body)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		// Hand the daemon's own message through: it is written for a human
		// and reads correctly to a model too.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("daemon returned %d", code)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
