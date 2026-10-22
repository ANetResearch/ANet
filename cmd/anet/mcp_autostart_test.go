//go:build !no_mcp

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/mcpserv"
)

// throwawayHome makes a new home directory the user's (HOME, USERPROFILE),
// with no ANET_HOME and no ANET_DATA_DIR, and returns it.
func throwawayHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANET_HOME", "")
	t.Setenv("ANET_DATA_DIR", "")
	t.Setenv("ANET_ID", "")
	rt := t.TempDir()
	if err := os.Chmod(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", rt)
	guardInProcess(t)
	return home
}

// setUpNotRunning is the default identity of a throwaway home (~/.anet) with
// a node set up (config.json with a free control address) and no daemon, and
// the function that serves its daemon in this process, as the starter of
// `anet mcp` would start it.
func setUpNotRunning(t *testing.T) (daemon.Layout, func() error) {
	t.Helper()
	home := throwawayHome(t)
	return setUpNotRunningAt(t, filepath.Join(home, ".anet"), nil)
}

// setUpNotRunningAt is setUpNotRunning for the data directory root, with
// extra members of config.json.
func setUpNotRunningAt(t *testing.T, root string, extra map[string]any) (daemon.Layout, func() error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := map[string]any{"control_addr": addr}
	for k, v := range extra {
		conf[k] = v
	}
	cfg, _ := json.Marshal(conf)
	if err := os.WriteFile(filepath.Join(root, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	layout := daemon.NewLayout(root)
	serve := func() error {
		d, err := daemon.New(layout)
		if err != nil {
			return err
		}
		const token = "mcp-autostart-token"
		if err := os.WriteFile(layout.ControlTokenPath(), []byte(token+"\n"), 0o600); err != nil {
			return err
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		srv := &http.Server{Handler: d.ControlHandler(token), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { _ = srv.Close(); d.Close() })
		return nil
	}
	return layout, serve
}

// stubStart replaces the starter of `anet mcp` for the test and counts its
// calls; start is what it does.
func stubStart(t *testing.T, start func(layout daemon.Layout, wait time.Duration, dir string) error) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := startNodeForMCP
	startNodeForMCP = func(layout daemon.Layout, wait time.Duration, dir string) error {
		n.Add(1)
		return start(layout, wait, dir)
	}
	t.Cleanup(func() { startNodeForMCP = old })
	return &n
}

// `anet mcp` brings up the node of its identity when the data directory is
// set up and no daemon runs: it starts the daemon (as `anet up`, in the
// user's home directory rather than the session's workspace), waits for it,
// and serves it. Nothing is written on stdout, which is the MCP session.
// [mut] no start → why is daemon_not_running and this is red.
func TestMCPStartsTheNodeOfItsIdentity(t *testing.T) {
	layout, serve := setUpNotRunning(t)
	home := os.Getenv("HOME")
	var gotWait time.Duration
	var gotDir string
	n := stubStart(t, func(l daemon.Layout, wait time.Duration, dir string) error {
		if l.Root != layout.Root {
			t.Errorf("started %s, want %s", l.Root, layout.Root)
		}
		gotWait, gotDir = wait, dir
		return serve()
	})
	var log bytes.Buffer
	var c *mcpControl
	var why *mcpserv.NodeUnavailable
	out := captureStdout(t, func() { c, why = mcpNode(layout, true, &log) })
	if out != "" {
		t.Errorf("anet mcp wrote %q on stdout while starting the node", out)
	}
	if why != nil {
		t.Fatalf("the node is unavailable after the start: %v", why)
	}
	if n.Load() != 1 || gotWait != mcpStartWait || gotDir != home {
		t.Errorf("start: %d calls, wait %v, dir %q (want 1, %v, %q)", n.Load(), gotWait, gotDir, mcpStartWait, home)
	}
	if !strings.Contains(log.String(), "starting it") {
		t.Errorf("progress line: %q", log.String())
	}
	var raw json.RawMessage
	if err := c.Call(context.Background(), "/status", map[string]any{}, &raw); err != nil || !strings.Contains(string(raw), "aid") {
		t.Errorf("/status through the started node: %v %s", err, raw)
	}
	// A node that already runs is used as it is.
	n2 := stubStart(t, func(daemon.Layout, time.Duration, string) error { return nil })
	if _, why := mcpNode(layout, true, io.Discard); why != nil || n2.Load() != 0 {
		t.Errorf("a running node: why %v, %d starts", why, n2.Load())
	}
}

// A data directory without a node is not set up by `anet mcp`: nothing is
// created or started, and the reason names the command that sets it up (with
// the identity's name for a named identity).
func TestMCPDoesNotSetUpANode(t *testing.T) {
	throwawayHome(t)
	n := stubStart(t, func(daemon.Layout, time.Duration, string) error {
		t.Error("anet mcp started a node in a data directory that is not set up")
		return nil
	})
	for _, c := range []struct {
		layout daemon.Layout
		next   string
	}{
		{daemon.NewLayout(filepath.Join(t.TempDir(), "data")), "anet up"},
		{daemon.IdentityLayout("work"), "anet --id work up"},
	} {
		_, why := mcpNode(c.layout, true, io.Discard)
		if why == nil || why.Reason != "daemon_not_running" || why.Next != c.next ||
			!strings.Contains(why.Message, "No anet node is set up for this identity yet") || strings.Contains(why.Error(), c.layout.Root) {
			t.Errorf("%s: %#v", c.layout.Root, why)
		}
		if c.layout.Initialized() {
			t.Errorf("%s was set up by anet mcp", c.layout.Root)
		}
		if _, err := os.Stat(c.layout.Root); err == nil {
			t.Errorf("%s was created by anet mcp", c.layout.Root)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d starts", n.Load())
	}
}

// A configuration that the daemon refuses is reported with the refusal's own
// reason and sentence; nothing is started.
func TestMCPReportsARefusedConfiguration(t *testing.T) {
	layout, _ := setUpNotRunning(t)
	if err := os.WriteFile(layout.ConfigPath(), []byte(`{"pricing": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	n := stubStart(t, func(daemon.Layout, time.Duration, string) error { return nil })
	_, why := mcpNode(layout, true, io.Discard)
	if why == nil || why.Reason != "config_key_removed" || !strings.Contains(why.Message, "pricing") || why.Next == "" {
		t.Errorf("refused configuration: %#v", why)
	}
	if n.Load() != 0 {
		t.Errorf("a refused configuration was started")
	}
}

// When the node does not start, `anet mcp` serves anyway: every tool is
// listed, guide works, node_status reports the reason and the command, every
// other tool answers with the same reason as a tool error, and the
// instructions say so. Once the node runs, the same session reaches it.
// [mut] node_status forwards the error → it is a tool error and this is red.
func TestMCPServesWithoutTheNode(t *testing.T) {
	layout, serve := setUpNotRunning(t)
	stubStart(t, func(daemon.Layout, time.Duration, string) error {
		return io.ErrUnexpectedEOF
	})
	c, why := mcpNode(layout, true, io.Discard)
	if why == nil || why.Reason != "daemon_not_running" || !strings.Contains(why.Detail, "did not answer within") ||
		strings.Contains(why.Error(), layout.Root) || strings.Contains(why.Error(), "unexpected EOF") {
		t.Fatalf("a failed start: %#v", why)
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := mcpserv.NewWith(c, "test", mcpserv.Options{Unavailable: why}).Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if ins := sess.InitializeResult().Instructions; !strings.HasPrefix(ins, "The anet node of this identity is not running") ||
		!strings.Contains(ins, "[daemon_not_running]") || !strings.Contains(ins, "anet up") {
		t.Errorf("instructions: %q", ins)
	}
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if len(names) != 15 || !strings.Contains(strings.Join(names, ","), "guide") {
		t.Errorf("tools without the node: %v", names)
	}
	call := func(name string, args map[string]any) (*mcp.CallToolResult, string) {
		t.Helper()
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b strings.Builder
		for _, ct := range res.Content {
			if tc, ok := ct.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return res, b.String()
	}
	if res, text := call("guide", map[string]any{"topic": "setup"}); res.IsError || !strings.HasPrefix(text, "# Setting up anet") {
		t.Errorf("guide without the node: %v %.60q", res.IsError, text)
	}
	res, text := call("node_status", nil)
	var status map[string]any
	_ = json.Unmarshal([]byte(text), &status)
	if res.IsError || status["running"] != false || status["reason"] != "daemon_not_running" || status["next"] != "anet up" ||
		!strings.Contains(text, "did not answer within") {
		t.Errorf("node_status without the node: error %v, %s", res.IsError, text)
	}
	for _, name := range []string{"get_task", "list_agents", "get_balance"} {
		args := map[string]any{}
		if name == "get_task" {
			args["task_id"] = "ix-1"
		}
		res, text := call(name, args)
		if !res.IsError || !strings.Contains(text, "[daemon_not_running]") || !strings.Contains(text, "→ anet up") {
			t.Errorf("%s without the node: error %v, %q", name, res.IsError, text)
		}
	}
	// The node comes up (the user or the agent ran anet up): the session reaches it.
	if err := serve(); err != nil {
		t.Fatal(err)
	}
	res, text = call("node_status", nil)
	if res.IsError || strings.Contains(text, `"running":false`) || !strings.Contains(text, "aid") {
		t.Errorf("node_status once the node runs: error %v, %.200s", res.IsError, text)
	}
}

// The whole command, as a host runs it: `anet mcp` in a data directory that
// is set up and has no daemon starts the daemon (this test binary, run as
// anet), answers the MCP session on stdout with JSON-RPC only, and leaves the
// daemon running after the session; `anet stop` ends it.
func TestMCPCommandStartsTheDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a daemon")
	}
	if runtime.GOOS != "linux" {
		t.Skip("finds the started daemon through /proc, to stop it by its process ID")
	}
	h := newCLIHome(t, "en")
	// The default identity of the home (~/.anet): the only kind of data
	// directory, with the named identities under it, that anet mcp starts.
	// Every command names it with --id default, so that it resolves strictly:
	// without an explicit identity the CLI falls back to the uid-wide pointer
	// (/run/user/<uid>/anet/daemon.json), which names the daemon of the real
	// user of this machine, and `anet stop` would stop that one.
	inHome := []string{"ANET_HOME=", "ANET_DATA_DIR="}
	data := filepath.Join(h.home, ".anet")
	if _, stderr, code := runCLI(t, h, []string{"--id", "default", "init"}, inHome...); code != 0 {
		t.Fatalf("anet init: %d %s", code, stderr)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "--id", "default", "mcp")
	cmd.Env = h.env(inHome...)
	guardCLIRun(t, cmd.Env, cmd.Args[1:])
	cmd.Dir = h.root
	detachFromTerminal(cmd)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"node_status","arguments":{}}}`,
	}, "\n") + "\n"
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The daemon that this test starts is stopped by its process ID, which is recorded once it
	// runs: the process of this test binary that holds the lock of this data directory. No
	// other process, and no `anet stop` that could resolve to another daemon, is involved.
	var daemonPID int
	t.Cleanup(func() {
		if daemonPID == 0 {
			daemonPID = daemonHolding(t, filepath.Join(data, "daemon.lock"))
		}
		if daemonPID != 0 {
			stopRecorded(t, daemonPID)
		}
	})
	if _, err := io.WriteString(stdin, in); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var got []string
	deadline := time.After(60 * time.Second)
wait:
	for len(got) < 2 {
		select {
		case l, ok := <-lines:
			if !ok {
				break wait
			}
			got = append(got, l)
		case <-deadline:
			break wait
		}
	}
	_ = stdin.Close()
	_ = cmd.Wait()
	if len(got) != 2 {
		t.Fatalf("stdout lines %q\nstderr %s", got, stderr.String())
	}
	for _, l := range got {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil || m["jsonrpc"] != "2.0" {
			t.Errorf("stdout carries a line that is not JSON-RPC: %q", l)
		}
	}
	if strings.Contains(got[1], `"isError":true`) || strings.Contains(got[1], `running\":false`) || !strings.Contains(got[1], "aid") {
		t.Errorf("node_status: %s\nstderr %s", got[1], stderr.String())
	}
	if !strings.Contains(stderr.String(), "starting it") {
		t.Errorf("stderr: %s", stderr.String())
	}
	// The daemon outlives the session.
	if !localDaemonUp(daemon.NewLayout(data)) {
		t.Error("the daemon did not outlive the MCP session")
	}
	if daemonPID = daemonHolding(t, filepath.Join(data, "daemon.lock")); daemonPID == 0 {
		t.Error("no process of this test holds the data directory of the started daemon")
	}
}

// `anet mcp` with an explicitly selected identity resolves strictly: an identity without its own
// daemon is reported as such, and no call reaches another daemon, even when the uid-wide pointer names
// a running one — neither at the start nor at a later call.
func TestMCPResolvesStrictlyForAnExplicitIdentity(t *testing.T) {
	throwawayHome(t)
	rt := t.TempDir()
	if err := os.Chmod(rt, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", rt)
	running, addr := servedDaemon(t)
	if err := os.MkdirAll(filepath.Join(rt, "anet"), 0o700); err != nil {
		t.Fatal(err)
	}
	ptr, _ := json.Marshal(map[string]string{"control_addr": addr, "data_dir": running.Root})
	if err := os.WriteFile(filepath.Join(rt, "anet", "daemon.json"), ptr, 0o600); err != nil {
		t.Fatal(err)
	}
	other := daemon.NewLayout(t.TempDir()) // selected explicitly, has no daemon
	c, why := mcpNode(other, true, io.Discard)
	if why == nil {
		t.Fatal("anet mcp for an identity without a daemon connected to another daemon")
	}
	var raw json.RawMessage
	err := c.Call(context.Background(), "/status", map[string]any{}, &raw)
	var u *mcpserv.NodeUnavailable
	if !errors.As(err, &u) || len(raw) != 0 {
		t.Fatalf("a call of anet mcp for an identity without a daemon: %v %s", err, raw)
	}
}

// `anet mcp` takes a port squatter for no daemon: it starts its own (stubbed here; the daemon moves to a
// free port), and neither the start nor a later call sends the token to the squatter (F18).
func TestMCPDoesNotSendTheTokenToAPortSquatter(t *testing.T) {
	home := throwawayHome(t)
	rt := t.TempDir()
	_ = os.Chmod(rt, 0o700)
	t.Setenv("XDG_RUNTIME_DIR", rt)
	sq := newSquatter(t)
	layout, token := victimLayoutAt(t, filepath.Join(home, ".anet"), sq.addr)
	starts := stubStart(t, func(daemon.Layout, time.Duration, string) error { return errors.New("not started in this test") })
	c, why := mcpNode(layout, true, io.Discard)
	if why == nil {
		t.Error("anet mcp started serving against a squatter")
	}
	if starts.Load() != 1 {
		t.Errorf("anet mcp did not start its own daemon: %d starts", starts.Load())
	}
	_ = c.Call(context.Background(), "/status", map[string]any{}, new(json.RawMessage))
	if got := sq.sent(); len(got) != 0 {
		t.Fatalf("the squatter received %q (token %s)", got, token)
	}
}

// `anet mcp` starts only an identity of the user's own anet home. A data
// directory named by ANET_DATA_DIR (or under an ANET_HOME) elsewhere, for
// example one of a workspace with a config.json of its choosing, is not
// started: the reason is daemon_not_running and the command is anet up, and
// nothing in the answer names the path. A named identity under ~/.anet/ids is
// started.
// [mut] mcpMayStart returns true → the raw directory is started and this is red.
func TestMCPStartsOnlyIdentitiesOfTheAnetHome(t *testing.T) {
	home := throwawayHome(t)
	var started []string
	stubStart(t, func(l daemon.Layout, _ time.Duration, _ string) error {
		started = append(started, l.Root)
		return errors.New("not started in this test")
	})
	workspace := filepath.Join(t.TempDir(), "project", ".anet-data")
	raw, _ := setUpNotRunningAt(t, workspace, map[string]any{"hub_url": "https://hub.example.invalid"})
	t.Setenv("ANET_DATA_DIR", workspace)
	if got := daemon.ResolveLayout(""); got.Root != workspace {
		t.Fatalf("ANET_DATA_DIR selects %s", got.Root)
	}
	_, why := mcpNode(raw, true, io.Discard)
	if why == nil || why.Reason != "daemon_not_running" || why.Next != "anet up" ||
		!strings.Contains(why.Message, "starts only identities in the anet home") || strings.Contains(why.Error(), workspace) {
		t.Errorf("a data directory outside the anet home: %#v", why)
	}
	t.Setenv("ANET_DATA_DIR", "")
	// An ANET_HOME elsewhere is not the user's anet home either.
	other := t.TempDir()
	t.Setenv("ANET_HOME", other)
	elsewhere, _ := setUpNotRunningAt(t, filepath.Join(other, "ids", "work"), nil)
	if _, why := mcpNode(elsewhere, true, io.Discard); why == nil || !strings.Contains(why.Message, "anet home") {
		t.Errorf("an identity of another ANET_HOME: %#v", why)
	}
	if len(started) != 0 {
		t.Fatalf("anet mcp started %v", started)
	}
	t.Setenv("ANET_HOME", "")
	// A named identity of the user's anet home is started.
	named, _ := setUpNotRunningAt(t, filepath.Join(home, ".anet", "ids", "work"), nil)
	_, why = mcpNode(named, true, io.Discard)
	if len(started) != 1 || started[0] != named.Root || why == nil || why.Next != "anet --id work up" {
		t.Errorf("a named identity of the anet home: started %v, %#v", started, why)
	}
	for _, c := range []struct {
		root string
		ok   bool
	}{
		{filepath.Join(home, ".anet"), true},
		{filepath.Join(home, ".anet", "ids", "work"), true},
		{filepath.Join(home, ".anet", "ids", "work", ".."), false},
		{filepath.Join(home, ".anet", "ids", "ids"), false},
		{filepath.Join(home, ".anet", "ids"), false},
		{filepath.Join(home, ".anet", "ids", "a", "b"), false},
		{filepath.Join(home, "project"), false},
		{"relative/.anet", false},
	} {
		if got := mcpMayStart(daemon.NewLayout(c.root)); got != c.ok {
			t.Errorf("mcpMayStart(%s) = %v", c.root, got)
		}
	}
}

// A node whose configuration enables the shell module (a controller may run
// commands on this machine) is not started by `anet mcp`; it is left to an
// explicit anet up.
// [mut] no shell check → the node is started and this is red.
func TestMCPDoesNotStartANodeWithTheShellModule(t *testing.T) {
	home := throwawayHome(t)
	n := stubStart(t, func(daemon.Layout, time.Duration, string) error {
		t.Error("anet mcp started a node with the shell module")
		return nil
	})
	layout, _ := setUpNotRunningAt(t, filepath.Join(home, ".anet"),
		map[string]any{"modules": map[string]any{"shell": map[string]any{"allow": []string{}}}})
	if !shellModuleConfigured(layout) {
		t.Fatal("the shell block is not seen")
	}
	_, why := mcpNode(layout, true, io.Discard)
	if why == nil || why.Reason != "daemon_not_running" || why.Next != "anet up" ||
		!strings.Contains(why.Message, "enables the shell module") {
		t.Errorf("a node with the shell module: %#v", why)
	}
	if n.Load() != 0 {
		t.Errorf("%d starts", n.Load())
	}
}

// What `anet mcp` tells the model names no path: not the data directory, not
// the file of a refusal, not the daemon's log. The paths and the log lines go
// to its standard error.
// [mut] the start error in Detail → the log line reaches the model and this is red.
func TestMCPTellsTheModelNoPathAndNoLog(t *testing.T) {
	layout, _ := setUpNotRunning(t)
	secret := "log line with " + layout.Root + "/secret"
	stubStart(t, func(daemon.Layout, time.Duration, string) error {
		return errors.New("the daemon exited immediately:\n  " + secret)
	})
	var log bytes.Buffer
	_, why := mcpNode(layout, true, &log)
	if why == nil {
		t.Fatal("no reason")
	}
	for _, text := range []string{why.Error(), why.Message, why.Detail, why.Next} {
		if strings.Contains(text, layout.Root) || strings.Contains(text, "secret") || strings.Contains(text, os.Getenv("HOME")) {
			t.Errorf("the model is told %q", text)
		}
	}
	if !strings.Contains(log.String(), secret) || !strings.Contains(log.String(), layout.Root) {
		t.Errorf("stderr lacks the detail: %q", log.String())
	}
	// A refusal that names a file names it by its name only.
	args := withoutPaths(map[string]any{"path": filepath.Join(layout.Root, "identity.json"), "key": "x",
		"paths": []string{filepath.Join(layout.Root, "a.json"), "b"}})
	if args["path"] != "identity.json" || args["key"] != "x" || strings.Join(args["paths"].([]string), ",") != "a.json,b" {
		t.Errorf("withoutPaths: %v", args)
	}
}
