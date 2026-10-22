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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/anethome"
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
// It brings the node up first (mcpNode): when no daemon answers for the
// selected identity and its data directory is set up, it starts the daemon as
// `anet up` does. The plugins of the coding agents start `anet mcp` with the
// session, and a node that is not running would otherwise leave the agent
// with no anet tools at all and nothing to tell the user.
//
// When the node cannot be brought up (no node set up for the identity, a
// configuration the daemon refuses, a daemon that does not answer), it serves
// anyway rather than exit: every tool is listed, guide works, node_status
// reports the reason and the command that fixes it, and the other tools
// answer with the same reason. It keeps looking for the daemon at each call,
// so the tools work as soon as the node runs, in the same session. An MCP
// server that fails to start leaves the model with nothing but a plugin's
// description, and some hosts remember the failure and do not start it again
// for a while.
//
// Everything it says goes to stderr: stdout is the MCP session.
//
// With an explicitly selected identity (--id, ANET_ID, ANET_HOME, ANET_DATA_DIR or a non-default
// `anet id use`) it resolves strictly: only that identity's own daemon, never the uid-wide pointer to
// whichever daemon started last (A2A-DESIGN §7.8). An MCP client configured for one identity must not
// end up driving another.
func runMCP(layout daemon.Layout, explicit bool) error {
	c, why := mcpNode(layout, explicit, os.Stderr)
	if why != nil {
		fmt.Fprintf(os.Stderr, "anet mcp: serving without the node: %s\n", why.Error())
	}
	srv := mcpserv.NewWith(c, daemon.Version, mcpserv.Options{Unavailable: why})
	err := srv.Run(context.Background(), &mcp.StdioTransport{})
	// A client that goes away is how this process is supposed to end. It
	// is spawned per session and dies with it, so reporting the
	// disconnection as a failure would put an error in the operator's log
	// every single time the tool worked.
	if err != nil && (errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF")) {
		return nil
	}
	return err
}

// mcpStartWait is how long `anet mcp` waits for a daemon it started (or one
// that another start is bringing up) to answer.
const mcpStartWait = 20 * time.Second

// startNodeForMCP starts the daemon of layout as `anet up` does, without
// printing on stdout, and returns once it answers or wait has passed. The
// daemon's working directory is dir. A variable so that the tests can start a
// daemon in the test process instead of this binary.
var startNodeForMCP = func(layout daemon.Layout, wait time.Duration, dir string) error {
	return startDaemonDetached(layout, wait, dir)
}

// mcpNode is the Control of `anet mcp` for layout, and why the node is
// unavailable when it is (nil when a daemon answers). Progress goes to logw.
//
// The steps, in order:
//
//  1. A daemon that answers (strictly this identity's when explicit) is used.
//  2. A data directory that holds no identity and no configuration is not set
//     up by `anet mcp`: it reports daemon_not_running.not_set_up with the
//     command that creates and starts the node. Creating an identity is the
//     installer's or the operator's step (anet up, anet id new), where the
//     node also joins a hub; `anet mcp` is started implicitly with every
//     session of a coding agent, and an identity name that is misspelt in a
//     plugin setting would otherwise create, and leave running, a new
//     identity.
//  3. A data directory that a daemon holds without answering is waited for
//     (it may be starting, for example by another session at the same moment),
//     and reported as daemon_not_answering when it does not answer in time.
//  4. Only an identity of the user's own anet home is started (mcpMayStart):
//     the default identity (~/.anet) and the named ones (~/.anet/ids/<name>).
//     A data directory that ANET_DATA_DIR or ANET_HOME names elsewhere is
//     reported as daemon_not_running.outside_home and left to an explicit
//     `anet up`. The environment of `anet mcp` is the host's, and a host may
//     pass on what a project sets; a data directory of a workspace, with a
//     config.json of the workspace's choosing, must not become a running node
//     just because a session started.
//  5. A configuration that enables the shell module (a controller may run
//     commands on this machine) is not started implicitly either:
//     daemon_not_running.shell_enabled, left to `anet up`.
//  6. A data directory or configuration that the daemon would refuse is
//     reported with the refusal's own reason and sentence; nothing is
//     started.
//  7. Otherwise the daemon is started (startNodeForMCP) and waited for.
//
// What the returned NodeUnavailable says goes to a model: it names the reason
// and the command, never a path (the data directory, a log) and never the
// daemon's log; those are written to logw, the standard error of `anet mcp`.
//
// The returned Control is used either way: while the node is unavailable it
// answers every call with the NodeUnavailable, and it looks for the daemon
// again at each call, so that `anet up` run by the user or the agent makes the
// tools work without a new session.
func mcpNode(layout daemon.Layout, explicit bool, logw io.Writer) (*mcpControl, *mcpserv.NodeUnavailable) {
	m := &mcpControl{connect: func() (*controlClient, error) { return connectMCP(layout, explicit) }}
	if cc, err := m.connect(); err == nil {
		m.cc = cc
		return m, nil
	}
	why := mcpWhyNot(layout, logw)
	if why == nil {
		if cc, err := m.connect(); err == nil {
			m.cc = cc
			return m, nil
		}
		why = notRunning(layout, "The node was started, but this session could not reach its control plane.")
	}
	m.why = why
	return m, why
}

// mcpWhyNot brings the node of layout up (steps 2 to 7 of mcpNode) and returns
// nil when it answers, or why it does not. What it reports with a path or the
// daemon's log goes to logw only.
func mcpWhyNot(layout daemon.Layout, logw io.Writer) *mcpserv.NodeUnavailable {
	if !layout.Initialized() {
		fmt.Fprintf(logw, "anet mcp: no node is set up in the data directory %s\n", layout.Root)
		return &mcpserv.NodeUnavailable{
			Reason:  "daemon_not_running",
			Message: a2ashape.Sentence("daemon_not_running.not_set_up", a2ashape.English, nil),
			Next:    upCommand(layout),
		}
	}
	if daemon.DataLockHeld(layout) {
		if waitForDaemon(layout, mcpStartWait) {
			return nil
		}
		fmt.Fprintf(logw, "anet mcp: a daemon holds the data directory %s and did not answer within %s\n", layout.Root, mcpStartWait)
		return &mcpserv.NodeUnavailable{
			Reason:  "daemon_not_answering",
			Message: a2ashape.Sentence("daemon_not_answering.mcp", a2ashape.English, a2ashape.Args{"dur": mcpStartWait}),
			Next:    "anet status",
		}
	}
	if !mcpMayStart(layout) {
		fmt.Fprintf(logw, "anet mcp: the data directory %s is not an identity of the anet home %s; "+
			"anet mcp does not start it (run anet up with the same ANET_DATA_DIR or ANET_HOME)\n", layout.Root, userAnetHome())
		return &mcpserv.NodeUnavailable{
			Reason:  "daemon_not_running",
			Message: a2ashape.Sentence("daemon_not_running.outside_home", a2ashape.English, nil),
			Next:    upCommand(layout),
		}
	}
	if shellModuleConfigured(layout) {
		fmt.Fprintf(logw, "anet mcp: the configuration in %s enables the shell module; anet mcp does not start such a node (run anet up)\n", layout.Root)
		return &mcpserv.NodeUnavailable{
			Reason:  "daemon_not_running",
			Message: a2ashape.Sentence("daemon_not_running.shell_enabled", a2ashape.English, nil),
			Next:    upCommand(layout),
		}
	}
	if why := startRefusal(layout, logw); why != nil {
		return why
	}
	fmt.Fprintf(logw, "anet mcp: the node is not running; starting it (data directory %s)\n", layout.Root)
	if err := startNodeForMCP(layout, mcpStartWait, daemonWorkDir(layout)); err != nil {
		fmt.Fprintf(logw, "anet mcp: the node did not start: %v\n", err)
		return notRunning(layout, fmt.Sprintf("The node was started and did not answer within %s, or it exited; "+
			"run %s on a terminal to see the reason.", mcpStartWait, upCommand(layout)))
	}
	return nil
}

// userAnetHome is the anet home of the user's own account, ~/.anet, whatever
// ANET_HOME says; "" when the home directory is unknown.
func userAnetHome() string {
	h, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(h) {
		return ""
	}
	return filepath.Join(h, ".anet")
}

// mcpMayStart reports whether `anet mcp` may start the daemon of layout
// itself: only for the default identity of the user's anet home (~/.anet)
// and a named identity under it (~/.anet/ids/<name>). See step 4 of mcpNode.
func mcpMayStart(layout daemon.Layout) bool {
	home := userAnetHome()
	if home == "" {
		return false
	}
	root, err := filepath.Abs(layout.Root)
	if err != nil {
		return false
	}
	root = filepath.Clean(root)
	if samePath(root, home) {
		return true
	}
	return samePath(filepath.Dir(root), filepath.Join(home, anethome.IDsDir)) && anethome.ValidName(filepath.Base(root))
}

// samePath compares two clean absolute paths, without case on Windows.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// shellModuleConfigured reports whether config.json of layout has a block for
// the shell module, which lets a controller run commands on this machine.
func shellModuleConfigured(layout daemon.Layout) bool {
	b, err := os.ReadFile(layout.ConfigPath())
	if err != nil {
		return false
	}
	var c struct {
		Modules map[string]json.RawMessage `json:"modules"`
	}
	if json.Unmarshal(b, &c) != nil {
		// Not readable as JSON: startRefusal, which comes next, reports it,
		// and nothing is started.
		return false
	}
	v, ok := c.Modules["shell"]
	return ok && strings.TrimSpace(string(v)) != "null"
}

// startRefusal is the refusal of the daemon to start with this data directory
// or its config.json (the checks of refuseConfig), or nil. The sentences name
// a file by its name only; the full refusal is written to logw.
func startRefusal(layout daemon.Layout, logw io.Writer) *mcpserv.NodeUnavailable {
	err := daemon.CheckDataDirForStart(layout)
	if err == nil {
		err = daemon.CheckConfigForStart(layout)
	}
	if err == nil {
		return nil
	}
	fmt.Fprintf(logw, "anet mcp: the node cannot start with the data directory %s: %v\n", layout.Root, err)
	var cr *daemon.ConfigRefusal
	if errors.As(err, &cr) && len(cr.Refusals) > 0 {
		var msgs []string
		next := ""
		for _, r := range cr.Refusals {
			args := withoutPaths(r.Args)
			msgs = append(msgs, a2ashape.Sentence(r.Key, a2ashape.English, args))
			if next == "" {
				next = a2ashape.Next(r.Key, a2ashape.English, args)
			}
		}
		if next == "" {
			next = "anet doctor"
		}
		return &mcpserv.NodeUnavailable{Reason: a2ashape.ReasonCode(cr.Refusals[0].Key), Message: strings.Join(msgs, " "), Next: next}
	}
	return notRunning(layout, "The node cannot start with its data directory; run "+upCommand(layout)+" on a terminal to see the reason.")
}

// withoutPaths is args with every absolute path replaced by its last element
// (config.json rather than the whole path), for a sentence that goes to a
// model.
func withoutPaths(args a2ashape.Args) a2ashape.Args {
	if args == nil {
		return nil
	}
	out := make(a2ashape.Args, len(args))
	for k, v := range args {
		switch x := v.(type) {
		case string:
			if filepath.IsAbs(x) {
				v = filepath.Base(x)
			}
		case []string:
			ys := make([]string, len(x))
			for i, y := range x {
				if filepath.IsAbs(y) {
					y = filepath.Base(y)
				}
				ys[i] = y
			}
			v = ys
		}
		out[k] = v
	}
	return out
}

// notRunning is daemon_not_running for `anet mcp`, with detail.
func notRunning(layout daemon.Layout, detail string) *mcpserv.NodeUnavailable {
	return &mcpserv.NodeUnavailable{
		Reason:  "daemon_not_running",
		Message: a2ashape.Sentence("daemon_not_running.mcp", a2ashape.English, nil),
		Next:    upCommand(layout),
		Detail:  detail,
	}
}

// upCommand is the command that starts (and, on a data directory that is not
// set up, creates) the node of layout: `anet up`, with the identity's name for
// a named identity.
func upCommand(layout daemon.Layout) string {
	if name := daemon.IdentityNameForDir(layout.Root); name != "" && name != "default" {
		return "anet --id " + name + " up"
	}
	return "anet up"
}

// waitForDaemon waits at most wait for the daemon of layout to answer.
func waitForDaemon(layout daemon.Layout, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if localDaemonUp(layout) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// daemonWorkDir is the working directory of a daemon that `anet mcp` starts:
// the user's home directory (the data directory when there is none), never
// the working directory of the coding agent's session, which is a workspace.
// The daemon outlives the session: it should neither keep that directory in
// use (on Windows a directory in use cannot be removed or renamed) nor resolve
// a relative path of its configuration against it.
func daemonWorkDir(layout daemon.Layout) string {
	if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
		return h
	}
	if abs, err := filepath.Abs(layout.Root); err == nil {
		return abs
	}
	return ""
}

// connectMCP finds the daemon of layout (strictly when explicit) and checks
// that it answers.
func connectMCP(layout daemon.Layout, explicit bool) (*controlClient, error) {
	cc, err := newControlClient(layout, explicit)
	if err != nil {
		return nil, err
	}
	// A quick liveness check: a short bound, so that a call made while the
	// node is down is answered with the reason at once.
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := cc.Call(ctx, "/status", map[string]any{}, new(json.RawMessage)); err != nil {
		return nil, err
	}
	return cc, nil
}

// mcpControl is the Control of `anet mcp`: the control client of the node
// once a daemon answers, and until then the reason it does not, with a new
// attempt to find the daemon at each call.
type mcpControl struct {
	mu      sync.Mutex
	cc      *controlClient
	why     *mcpserv.NodeUnavailable
	connect func() (*controlClient, error)
}

// client is the control client to send with, or why there is none.
func (m *mcpControl) client() (*controlClient, *mcpserv.NodeUnavailable) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cc != nil {
		return m.cc, nil
	}
	if m.connect != nil {
		if cc, err := m.connect(); err == nil {
			m.cc, m.why = cc, nil
			return cc, nil
		}
	}
	return nil, m.why
}

func (m *mcpControl) Call(ctx context.Context, path string, body, out any) error {
	cc, why := m.client()
	if cc == nil {
		return why
	}
	return cc.Call(ctx, path, body, out)
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
// that gives another token for the same address, sends the call once more
// with it. A 401 is answered before a route does anything, so the second
// attempt is the first one the daemon acts on.
//
// The address does not change mid-session. A 401 comes from a daemon of
// this user's at the address the session has used (the listener is checked
// before the token is sent), so the daemon that rotated is at that address;
// a lookup that names another one has found another daemon — without an
// explicit identity, ResolveControl falls back to whichever daemon started
// last — and following it would have the agent's tool calls, its tasks and
// payments among them, act as another identity from one call to the next.
type controlClient struct {
	mu sync.Mutex
	c  *client
	// resolve finds the daemon again: base and token, as at start; nil
	// keeps the token it has.
	resolve func() (base, token string, err error)
}

// newControlClient is the control client of `anet mcp` for layout: with an
// explicitly selected identity it resolves strictly, only that identity's
// own daemon (A2A-DESIGN §7.8), the first time and after a 401. It does not
// contact the daemon; the error is the resolver's.
func newControlClient(layout daemon.Layout, explicit bool) (*controlClient, error) {
	resolve := daemon.ResolveControl
	if explicit {
		resolve = daemon.ResolveControlStrict
	}
	base, token, err := resolve(layout)
	if err != nil {
		return nil, err
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
// token at the same address; see controlClient). Concurrent calls that were
// all refused share one lookup's result.
func (cc *controlClient) refreshed(used client) (client, bool) {
	if cc.resolve == nil {
		return used, false
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.c.token != used.token {
		// Another call found the new token already.
		return *cc.c, true
	}
	base, token, err := cc.resolve()
	if err != nil || base != used.base || token == used.token {
		return used, false
	}
	cc.c.token = token
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
			Error   string `json:"error"`
			Code    string `json:"code"`
			Reason  string `json:"reason"`
			HubCode string `json:"hub_code"`
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
		return &mcpserv.DaemonError{Status: code, Message: e.Error, Code: e.Code, Reason: e.Reason, HubCode: e.HubCode}
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
