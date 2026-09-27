//go:build linux

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bindSources returns the source path of every bind flag in a bwrap argv.
func bindSources(argv []string) []string {
	var out []string
	for i := 0; i+2 < len(argv); i++ {
		switch argv[i] {
		case "--bind", "--ro-bind", "--ro-bind-try", "--bind-try", "--dev-bind", "--dev-bind-try":
			out = append(out, argv[i+1])
			i += 2
		}
		if argv[i] == "--" {
			break
		}
	}
	return out
}

func hasSeq(argv []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(argv); i++ {
		ok := true
		for j := range seq {
			if argv[i+j] != seq[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// The argv builder must produce the §6 shape: namespaces, tmpfs over /tmp, /var/tmp, /dev/shm, /run and
// the home directory, the work dir as the only writable bind, and no bind that exposes a never-bind
// path.
func TestBwrapArgsHaveTheSandboxShapeAndBindNothingProtected(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	data := filepath.Join(home, ".anet")
	work := filepath.Join(root, "cache", "anet", "work", "aid", "ix1")
	runtime := filepath.Join(root, "opt", "agent")
	for _, d := range []string{data, work, runtime} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	never := resolvedAll([]string{data, filepath.Join(root, "run-user"), filepath.Join(home, ".claude")})
	argv, err := buildBwrapArgs(bwrapSpec{WorkDir: work, Home: home, NeverBind: never, RuntimeRO: []string{runtime}},
		filepath.Join(runtime, "agent"), []string{"-p", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"--unshare-ipc", "--unshare-pid", "--unshare-uts", "--new-session", "--die-with-parent"} {
		if !hasSeq(argv, f) {
			t.Errorf("missing %s in %v", f, argv)
		}
	}
	for _, d := range []string{"/tmp", "/var/tmp", "/dev/shm", "/run", home} {
		if !hasSeq(argv, "--tmpfs", d) {
			t.Errorf("missing --tmpfs %s", d)
		}
	}
	if !hasSeq(argv, "--bind", work, work) || !hasSeq(argv, "--chdir", work) {
		t.Errorf("the work dir must be the writable bind and the working directory: %v", argv)
	}
	if !hasSeq(argv, "--ro-bind", runtime, runtime) {
		t.Errorf("the agent runtime must be bound read-only: %v", argv)
	}
	writable := 0
	for i := range argv {
		if argv[i] == "--bind" || argv[i] == "--bind-try" || argv[i] == "--dev-bind" {
			writable++
		}
	}
	if writable != 1 {
		t.Errorf("exactly one writable bind expected, got %d: %v", writable, argv)
	}
	for _, src := range bindSources(argv) {
		if n, bad := bindConflict(src, never); bad {
			t.Errorf("bind %s exposes never-bind path %s", src, n)
		}
	}
	if !hasSeq(argv, "--", filepath.Join(runtime, "agent"), "-p", "hello") {
		t.Errorf("the command must follow --: %v", argv)
	}
	// /etc is bound file by file, never as a whole.
	if hasSeq(argv, "--ro-bind", "/etc", "/etc") || hasSeq(argv, "--ro-bind", home, home) {
		t.Errorf("/etc or the home dir bound whole: %v", argv)
	}
}

// A runtime directory that contains the data dir (an agent binary placed in the home directory) makes
// the run unavailable instead of binding the home directory.
func TestBwrapArgsRefuseARuntimeDirContainingTheDataDir(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	data := filepath.Join(home, ".anet")
	work := filepath.Join(root, "work")
	for _, d := range []string{data, work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	never := resolvedAll([]string{data})
	_, err := buildBwrapArgs(bwrapSpec{WorkDir: work, Home: home, NeverBind: never, RuntimeRO: []string{home}},
		filepath.Join(home, "agent"), nil)
	if err == nil {
		t.Fatal("a runtime bind containing the data dir was accepted")
	}
	// Inside a never-bind path is refused too.
	_, err = buildBwrapArgs(bwrapSpec{WorkDir: work, Home: home, NeverBind: never, RuntimeRO: []string{filepath.Join(data, "bin")}},
		"/usr/bin/true", nil)
	if err == nil {
		t.Fatal("a runtime bind inside the data dir was accepted")
	}
	// A work dir inside the data dir is refused.
	_, err = buildBwrapArgs(bwrapSpec{WorkDir: filepath.Join(data, "work"), Home: home, NeverBind: never}, "/usr/bin/true", nil)
	if err == nil {
		t.Fatal("a work dir inside the data dir was accepted")
	}
}

// The never-bind list covers every §6 entry: the resolved data dir (not only ~/.anet), the runtime
// dirs, the ANetLink sockets and the agent credential dirs.
func TestSandboxPlanNeverBindListIsComplete(t *testing.T) {
	d := newBareDaemon(t)
	sock := filepath.Join(t.TempDir(), "c1.sock")
	d.mu.Lock()
	d.cfg.Modules = ModulesConfig{"anetlink": json.RawMessage(`{"socket":"` + sock + `"}`)}
	d.mu.Unlock()
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	bwrapLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	defer func() { bwrapLookPath = exec.LookPath }()
	// prepare fails without bwrap; collect the list the way newSandboxPlan does and check it.
	_, err := d.newSandboxPlan()
	if !errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("without bwrap the plan must be unavailable, got %v", err)
	}
	never := d.sandboxNeverBind()
	home, _ := os.UserHomeDir()
	realData, _ := filepath.EvalSymlinks(d.layout.Root)
	for _, want := range []string{realData, legacyRuntimeDir(), xdg, sock, filepath.Join(home, ".claude"), filepath.Join(home, ".codex")} {
		found := false
		for _, n := range never {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("never-bind list lacks %s: %v", want, never)
		}
	}
}

// Without bubblewrap the runner fails closed: errSandboxUnavailable, and the agent never runs.
func TestSandboxFailsClosedWithoutBwrap(t *testing.T) {
	d := newBareDaemon(t)
	marker := filepath.Join(t.TempDir(), "ran")
	stub := writeStub(t, "#!/bin/sh\ntouch "+marker+"\necho ran\n")
	bwrapLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	defer func() { bwrapLookPath = exec.LookPath }()
	_, err := d.runSandboxed(context.Background(), sandboxRequest{
		Cfg:   AutoReplyConfig{Backend: "exec", Agent: "claude", Command: stub, APIKey: "k"},
		RC:    replyContext{Role: "inbound", Goal: "g", InteractionID: "ix-nobwrap"},
		Turns: []chatTurn{{Role: "user", Text: "hi"}},
	})
	if !errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("want errSandboxUnavailable, got %v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the agent ran although the sandbox was unavailable")
	}
}

// Without auto_reply.api_key the sandboxed run is unavailable even where bubblewrap works: the agent's
// login state is hidden inside the sandbox and must not be bound in.
func TestSandboxRequiresAnAPIKey(t *testing.T) {
	d := newBareDaemon(t)
	marker := filepath.Join(t.TempDir(), "ran")
	stub := writeStub(t, "#!/bin/sh\ntouch "+marker+"\necho ran\n")
	_, err := d.runSandboxed(context.Background(), sandboxRequest{
		Cfg:   AutoReplyConfig{Backend: "exec", Agent: "claude", Command: stub},
		RC:    replyContext{Role: "inbound", Goal: "g", InteractionID: "ix-nokey"},
		Turns: []chatTurn{{Role: "user", Text: "hi"}},
	})
	if !errors.Is(err, errSandboxUnavailable) || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("want errSandboxUnavailable naming api_key, got %v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the agent ran without an API key")
	}
}

// Integration: with a working bubblewrap, a sandboxed agent (the test binary acting as the probe)
// cannot read the control or A2A token, cannot connect to a Unix socket in the data dir or to the
// system bus, does not see the data dir, and can write its work dir. Skipped where bwrap is missing or
// cannot create namespaces.
func TestSandboxIntegrationConfinesTheAgent(t *testing.T) {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not installed")
	}
	if err := probeBwrap(path); err != nil {
		t.Skipf("bwrap cannot create namespaces here: %v", err)
	}
	d := newBareDaemon(t)
	data := d.layout.Root
	for _, f := range []string{"control_token.txt", "a2a_token.txt"} {
		if err := os.WriteFile(filepath.Join(data, f), []byte("secret-"+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(data, "c1.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	dbus := "/run/dbus/system_bus_socket"
	if _, err := os.Stat(dbus); err != nil {
		dbus = ""
	}
	req, _ := json.Marshal(sandboxProbeRequest{
		ControlToken: filepath.Join(data, "control_token.txt"),
		A2AToken:     filepath.Join(data, "a2a_token.txt"),
		Socket:       sock, DBus: dbus, DataDir: data,
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := d.runSandboxed(ctx, sandboxRequest{
		Cfg: AutoReplyConfig{Backend: "exec", Agent: "claude", Command: self, APIKey: "k", APITimeoutSeconds: 50},
		RC: replyContext{Role: "inbound", InteractionID: "ix-probe",
			Goal: sandboxProbeMarker + string(req) + sandboxProbeEnd},
		Turns: []chatTurn{{Role: "user", Text: "probe"}},
	})
	if err != nil {
		t.Fatalf("sandboxed probe: %v", err)
	}
	if !res.Sandboxed {
		t.Fatal("the result must say it ran sandboxed")
	}
	var got sandboxProbeResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Reply)), &got); err != nil {
		t.Fatalf("probe reply %q: %v", res.Reply, err)
	}
	if got.Error != "" {
		t.Fatalf("probe: %s", got.Error)
	}
	t.Logf("probe result inside the sandbox: %+v (system bus probed: %v)", got, dbus != "")
	if got.ControlTokenRead || got.A2ATokenRead {
		t.Errorf("a token was readable inside the sandbox: %+v", got)
	}
	if got.SocketConnected {
		t.Errorf("a Unix socket in the data dir was reachable: %+v", got)
	}
	if got.DBusConnected {
		t.Errorf("the system bus was reachable: %+v", got)
	}
	if got.DataDirVisible {
		t.Errorf("the data dir was visible: %+v", got)
	}
	if !got.WorkDirWritable {
		t.Errorf("the work dir was not writable: %+v", got)
	}
	wantWork, _ := execInteractionDir(d.layout.Root, d.AID(), "ix-probe")
	if got.Cwd != wantWork {
		t.Errorf("the agent ran in %q, want the interaction work dir %q", got.Cwd, wantWork)
	}
	if _, err := os.Stat(filepath.Join(wantWork, "probe-wrote-this")); err != nil {
		t.Errorf("the write inside the sandbox did not reach the host work dir: %v", err)
	}
}
