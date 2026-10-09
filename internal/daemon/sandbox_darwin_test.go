//go:build darwin

package daemon

// The macOS sandbox on the real thing (LEC §4.3.8, MS-4, MS-5, MS-7, SI-55, SI-56, SI-64): these run on a
// Mac with /usr/bin/sandbox-exec and skip elsewhere; the CI seatbelt job (macos-14, macos-15) runs them
// and treats a skip as a failure. Everything around Seatbelt is also tested on Linux through a stand-in
// (sandbox_seatbelt_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// noSandbox makes the Mac's sandbox unavailable for the test, and returns the local reason a refused run
// is recorded with: no sandbox-exec.
func noSandbox(t *testing.T) string {
	t.Helper()
	old := seatbeltExecPath
	seatbeltExecPath = filepath.Join(t.TempDir(), "no-sandbox-exec")
	t.Cleanup(func() { seatbeltExecPath = old })
	return seatbeltCodeNoSandboxExec
}

// realSeatbeltProbe runs the probe with the Mac's sandbox-exec, uncached. A Mac without it skips (the CI
// seatbelt job treats a skip as a failure).
func realSeatbeltProbe(t *testing.T) seatbeltProbe {
	t.Helper()
	if _, err := os.Stat(seatbeltExecPath); err != nil {
		t.Skipf("needs %s (a Mac): %v", seatbeltExecPath, err)
	}
	pr := runSeatbeltProbe(seatbeltExecPath)
	t.Logf("probe: err %v; report %+v", pr.Err, pr.Report)
	return pr
}

// MS-4 on the real thing: inside the sandbox the real home cannot be listed nor a file outside the run
// read, another port of the host's loopback cannot be reached, the egress endpoint can, /usr/bin/security
// does not start, and the work dir takes a file (M-MS-1, M-MS-3).
func TestSeatbeltProbeFiveItems(t *testing.T) {
	pr := realSeatbeltProbe(t)
	if pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	r := pr.Report
	for name, ok := range map[string]bool{"1 home and outside unreadable": r.HomeUnreadable, "2 host loopback port refused": r.HostTCPRefused,
		"3 egress endpoint reached": r.EgressReached, "4 security refused": r.SecurityRefused, "5 work dir writable": r.WorkWritable} {
		if !ok {
			t.Errorf("item %s does not hold: %v", name, r.Detail)
		}
	}
}

// prepared is a plan whose sandbox passed the probe on this Mac, for a daemon's runs.
func preparedSeatbelt(t *testing.T, d *Daemon) *sandboxPlan {
	t.Helper()
	if pr := realSeatbeltProbe(t); pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	plan := &sandboxPlan{AID: d.AID(), NeverBind: d.sandboxNeverBind()}
	if err := plan.prepare(); err != nil {
		t.Fatal(err)
	}
	return plan
}

// sandboxedProbe runs the test binary as the claude agent in the real sandbox, through runSandboxed, with
// req as its probe request.
func sandboxedProbe(t *testing.T, d *Daemon, cfg AutoReplyConfig, ix string, req sandboxProbeRequest, turns []chatTurn) (sandboxResult, sandboxProbeResult) {
	t.Helper()
	if pr := realSeatbeltProbe(t); pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	raw, _ := json.Marshal(req)
	cfg.Backend, cfg.Agent, cfg.Command, cfg.APITimeoutSeconds = "exec", "claude", selfExe(t), 50
	if turns == nil {
		turns = []chatTurn{{Role: "user", Text: "probe"}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := d.runSandboxed(ctx, sandboxRequest{Cfg: cfg,
		RC:    replyContext{Role: "inbound", InteractionID: ix, Goal: sandboxProbeMarker + string(raw) + sandboxProbeEnd},
		Turns: turns})
	if err != nil {
		t.Fatalf("sandboxed probe: %v", err)
	}
	var got sandboxProbeResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Reply)), &got); err != nil {
		t.Fatalf("probe reply %q: %v", res.Reply, err)
	}
	if got.Error != "" {
		t.Fatalf("probe: %s", got.Error)
	}
	t.Logf("probe result inside the sandbox: %+v", got)
	return res, got
}

// leftRunDirs are the run directories of aid still on disk.
func leftRunDirs(aid string) []string {
	ents, _ := os.ReadDir(sandboxRunRoot())
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), runDirPrefix(aid)+"-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// Integration (SI-55, SI-59, MS-2): in the real sandbox the agent (the test binary as the probe) cannot
// read the control or A2A token, cannot connect to a Unix socket in the data dir, cannot list the data dir
// or read a file in the host's temp dir, and can write its work dir, the interaction's sandbox work dir;
// nothing of the run is left after it.
func TestSeatbeltIntegrationConfinesTheAgent(t *testing.T) {
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
	marker, err := os.CreateTemp("", "anet-host-tmp-marker-")
	if err != nil {
		t.Fatal(err)
	}
	marker.Close()
	defer os.Remove(marker.Name())
	res, got := sandboxedProbe(t, d, AutoReplyConfig{APIKey: "k"}, "ix-probe", sandboxProbeRequest{
		ControlToken: filepath.Join(data, "control_token.txt"), A2AToken: filepath.Join(data, "a2a_token.txt"),
		Socket: sock, DataDir: data, HostTmpMarker: marker.Name()}, nil)
	if !res.Sandboxed {
		t.Fatal("the result must say it ran sandboxed")
	}
	if got.ControlTokenRead || got.A2ATokenRead || got.SocketConnected || got.DataDirListed || got.HostTmpRead {
		t.Errorf("the sandbox let the agent through: %+v", got)
	}
	if !got.WorkDirWritable {
		t.Error("the work dir was not writable")
	}
	wantWork, _ := execSandboxInteractionDir(d.layout.Root, d.AID(), "ix-probe")
	realWork, _ := filepath.EvalSymlinks(wantWork)
	if got.Cwd != wantWork && got.Cwd != realWork {
		t.Errorf("the agent ran in %q, want the interaction's sandbox work dir %q", got.Cwd, wantWork)
	}
	if _, err := os.Stat(filepath.Join(wantWork, "probe-wrote-this")); err != nil {
		t.Errorf("the write inside the sandbox did not reach the work dir: %v", err)
	}
	if left := leftRunDirs(d.AID()); len(left) != 0 {
		t.Errorf("run directories left behind: %v", left)
	}
}

// Integration (SI-55, SI-57, EG-1, EG-3): in the real sandbox the agent reaches its model only through the
// node: the injection route, keyed, arrives at the model service with the operator's key, the pinned model
// and the bounded output; the service's own address is out of reach; CONNECT is answered by policy with
// the run's key and 407 without; the run's counts come back.
func TestSeatbeltIntegrationReachesTheModelOnlyThroughTheNode(t *testing.T) {
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamAnthropic, f)
	d := newBareDaemon(t)
	res, got := sandboxedProbe(t, d, AutoReplyConfig{APIKey: testOperatorKey}, "ix-egress",
		sandboxProbeRequest{Egress: true, HostURL: f.srv.URL + "/v1/models", OperatorKey: testOperatorKey}, nil)
	if !got.KeyIsPlaceholder || got.EnvHasOperatorKey {
		t.Errorf("the agent's key: placeholder %v, operator's key in its environment %v", got.KeyIsPlaceholder, got.EnvHasOperatorKey)
	}
	if got.RouteStatus != 200 {
		t.Errorf("the injection route answered %d: %s", got.RouteStatus, got.RouteBody)
	}
	if got.HostReached {
		t.Error("the model service's own address was reachable from the sandbox")
	}
	if got.ConnectStatus != 403 || got.ConnectNoKeyStatus != 407 {
		t.Errorf("CONNECT answered %d with the key, %d without", got.ConnectStatus, got.ConnectNoKeyStatus)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("the model service received %d requests", len(reqs))
	}
	if err := si57Check(up, reqs[0]); err != nil {
		t.Errorf("SI-57: %v", err)
	}
	if res.Egress == nil || res.Egress.ModelRequests != 1 || res.Egress.Connect.Refused["egress_host_not_allowed"] != 1 ||
		res.Egress.Connect.Refused["egress_unauthorized"] != 1 {
		t.Errorf("the run's egress counts: %+v", res.Egress)
	}
}

// Integration (SI-56, AV-1, EG-3): in the real sandbox the agent reads its conversation — over 200 KiB —
// on stdin; its environment holds no model key of the operator's or the daemon's, no proxy password and
// no local token; its command line holds no conversation, no interaction id and no key, while its
// instructions say it runs in the sandbox.
func TestSeatbeltIntegrationHoldsNoSecret(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", canaryEnvKey)
	t.Setenv("HTTPS_PROXY", "http://user:"+canaryProxy+"@proxy.example:3128")
	f := newFakeModelService(t)
	_ = testUpstream(t, egressUpstreamAnthropic, f)
	d := newBareDaemon(t)
	ctlTok, err := newControlToken(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("p", 200<<10)
	_, got := sandboxedProbe(t, d, AutoReplyConfig{APIKey: canaryKey}, canaryIX,
		sandboxProbeRequest{Egress: true, HostURL: f.srv.URL + "/v1/models", OperatorKey: canaryKey},
		[]chatTurn{{Role: "user", Text: canaryTurn + pad[:60<<10]}, {Role: "user", Text: pad[:60<<10]},
			{Role: "user", Text: pad[:60<<10]}, {Role: "user", Text: pad[:30<<10]}})
	if got.StdinBytes < 200<<10 {
		t.Errorf("the agent read %d bytes on stdin, want the whole conversation", got.StdinBytes)
	}
	env := strings.Join(got.Environ, "\n")
	for _, c := range []string{canaryKey, canaryEnvKey, canaryProxy, ctlTok} {
		if strings.Contains(env, c) {
			t.Errorf("the sandboxed agent's environment holds %q", c)
		}
	}
	key := strings.Split(strings.TrimPrefix(envOf(got.Environ)["HTTPS_PROXY"], "http://anet:"), "@")[0]
	argv := strings.Join(got.Argv, " ")
	for _, c := range []string{canaryTurn, canaryIX, sandboxProbeMarker, canaryKey, key} {
		if c != "" && strings.Contains(argv, c) {
			t.Errorf("the sandboxed agent's command line holds %q: %s", c, argv)
		}
	}
	if !got.KeyIsPlaceholder || len(key) != 64 {
		t.Errorf("placeholder %v, run key %q", got.KeyIsPlaceholder, key)
	}
	if !strings.Contains(argv, "--append-system-prompt") || !strings.Contains(argv, "## Sandbox") {
		t.Errorf("the sandboxed agent was not told it runs in the sandbox: %s", argv)
	}
}

// buildMachLookup builds the test-only cgo helper (testdata/seatbelt/machlookup) into dir. It needs the go
// command and a C compiler, which a Mac with the developer tools (and the CI runners) has.
func buildMachLookup(t *testing.T, dir string) string {
	t.Helper()
	goCmd, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("needs the go command to build the Mach lookup helper: %v", err)
	}
	bin := filepath.Join(dir, "machlookup")
	cmd := exec.Command(goCmd, "build", "-o", bin, "./testdata/seatbelt/machlookup")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if testRealHome != "" {
		cmd.Env = append(cmd.Env, "HOME="+testRealHome) // the go command's caches are in the real one
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("needs cgo and a C compiler to build the Mach lookup helper: %v\n%s", err, out)
	}
	return bin
}

// seatbeltRun runs argv under a run's profile with sandbox-exec, the program's directory being the
// runtime, and returns its exit status and output.
func seatbeltRun(t *testing.T, argv ...string) (int, string) {
	t.Helper()
	run, work := seatbeltRunDirs(t)
	return seatbeltRunIn(t, run, work, nil, argv...)
}

// seatbeltRunDirs makes a run directory (with home/ and tmp/) and a work dir, by their real paths.
func seatbeltRunDirs(t *testing.T) (run, work string) {
	t.Helper()
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	run, work = filepath.Join(tmp, "run"), filepath.Join(tmp, "work")
	for _, d := range []string{run, filepath.Join(run, "home"), filepath.Join(run, "tmp"), work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return run, work
}

// seatbeltRunIn is seatbeltRun with the run directory and work dir given, and runtime directories besides
// the program's own.
func seatbeltRunIn(t *testing.T, run, work string, runtime []string, argv ...string) (int, string) {
	t.Helper()
	initExe, _ := sandboxInitExe()
	rt, _ := filepath.EvalSymlinks(filepath.Dir(argv[0]))
	profile, err := buildSeatbeltProfile(seatbeltSpec{Init: initExe, RunDir: run, WorkDir: work,
		Runtime: append([]string{rt}, runtime...)})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(run, seatbeltProfileName)
	if err := os.WriteFile(p, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(seatbeltExecPath, append([]string{"-f", p, "--"}, argv...)...)
	cmd.Env, cmd.Dir = []string{"HOME=" + filepath.Join(run, "home")}, work
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &ee):
		return ee.ExitCode(), string(out)
	}
	return -1, err.Error()
}

// SI-64, J8 (the probe half), M-MS-2: under a run's profile the keychain cannot be reached — the security
// daemon's Mach service is refused, and /usr/bin/security does not start — while a lookup the profile
// allows succeeds, and the same helper outside the sandbox finds the security daemon.
func TestSeatbeltKeychainIsOutOfReach(t *testing.T) {
	if _, err := os.Stat(seatbeltExecPath); err != nil {
		t.Skipf("needs %s (a Mac): %v", seatbeltExecPath, err)
	}
	helper := buildMachLookup(t, t.TempDir())
	if out, err := exec.Command(helper, "com.apple.SecurityServer").CombinedOutput(); err != nil {
		t.Fatalf("outside the sandbox the helper finds no security daemon: %v %s", err, out)
	}
	for _, name := range []string{"com.apple.SecurityServer", "com.apple.securityd.xpc", "com.apple.secd", "com.apple.trustd.agent",
		"com.apple.pasteboard.1", "com.apple.coreservices.launchservicesd", "com.apple.tccd", "com.apple.windowserver.active"} {
		if code, out := seatbeltRun(t, helper, name); code != 3 {
			t.Errorf("%s from the sandbox: exit %d %s, want refused (3)", name, code, out)
		}
	}
	if code, out := seatbeltRun(t, helper, "com.apple.system.opendirectoryd.libinfo"); code != 0 {
		t.Errorf("an allowed service from the sandbox: exit %d %s", code, out)
	}
	if code, out := seatbeltRun(t, "/usr/bin/security", "list-keychains"); code == 0 || strings.Contains(out, ".keychain") {
		t.Errorf("/usr/bin/security ran in the sandbox: exit %d %s", code, out)
	}
	for _, prog := range []string{"/usr/bin/osascript", "/usr/bin/open"} {
		if code, _ := seatbeltRun(t, prog, "-h"); code == 0 {
			t.Errorf("%s ran in the sandbox", prog)
		}
	}
}

// pidAlive reports whether a process with pid exists.
func pidAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// MS-5: the agent stays in the group the daemon starts the run in, so that what it leaves in the
// background ends with the run: when the agent exits (init ends the rest of the group) and on a timeout
// (the daemon kills the group).
func TestSeatbeltEndsWhatTheAgentLeftInItsGroup(t *testing.T) {
	d := newBareDaemon(t)
	plan := preparedSeatbelt(t, d)
	work := t.TempDir()
	for name, tc := range map[string]struct {
		script  string
		timeout time.Duration
	}{
		"agent exits": {"sleep 60 >/dev/null 2>&1 & echo $!", 30 * time.Second},
		"timeout":     {"sleep 60 >/dev/null 2>&1 & echo $!; wait", 2 * time.Second},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
		out, _, _ := runCmd(ctx, execInvokeOpts{Sandbox: plan, WorkDir: work, Env: []string{"PATH=/usr/bin:/bin"}}, nil,
			"/bin/sh", "-c", tc.script)
		cancel()
		pid, err := strconv.Atoi(strings.TrimSpace(string(bytes.SplitN(out, []byte("\n"), 2)[0])))
		if err != nil {
			t.Fatalf("%s: no pid from the agent: %q", name, out)
		}
		deadline := time.Now().Add(5 * time.Second)
		for pidAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if pidAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("%s: the agent's background process %d outlived the run", name, pid)
		}
	}
}

// seatbeltReachArg starts the test binary as the helper of TestSeatbeltRefusesWhatItWouldAllowByDefault:
// in the sandbox, it tries what Seatbelt allows by default even under (deny default) and a run's profile
// refuses, and prints what it could do as one JSON line. Its arguments are a file of its runtime, a file
// outside the run and the work dir.
const seatbeltReachArg = "__seatbelt-reach-test"

// seatbeltReach is what the helper could do: "EPERM" is the sandbox's refusal, "done" success, anything
// else another error.
type seatbeltReach struct {
	ReadRuntime bool   `json:"read_runtime"` // control: the runtime's file reads
	GetFL       bool   `json:"getfl"`        // control: an ordinary fcntl works on it
	SelfInfo    bool   `json:"self_info"`    // control: process information on itself
	Compress    string `json:"compress"`     // F_MAKECOMPRESSED on the runtime file, opened for reading
	Transfer    string `json:"transfer"`     // F_TRANSFEREXTENTS from a work dir file into the runtime file
	Link        string `json:"link"`         // a hard link of the outside file into the work dir
	Clone       string `json:"clone"`        // a clone of the outside file into the work dir
	ParentInfo  string `json:"parent_info"`  // process information on the test process, outside the sandbox
}

func reachResult(err error) string {
	switch {
	case err == nil:
		return "done"
	case errors.Is(err, syscall.EPERM):
		return "EPERM"
	}
	return err.Error()
}

func init() {
	testMainHooks[seatbeltReachArg] = func(args []string) int {
		if len(args) != 3 {
			return 2
		}
		runtimeFile, outside, work := args[0], args[1], args[2]
		var r seatbeltReach
		if f, err := os.Open(runtimeFile); err == nil {
			_, err = io.ReadAll(f)
			r.ReadRuntime = err == nil
			_, err = unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
			r.GetFL = err == nil
			_, err = unix.FcntlInt(f.Fd(), 80, 0) // F_MAKECOMPRESSED
			r.Compress = reachResult(err)
			if donor, err := os.OpenFile(filepath.Join(work, "donor"), os.O_RDWR|os.O_CREATE, 0o600); err == nil {
				_, _ = donor.Write(bytes.Repeat([]byte("d"), 64<<10))
				_ = donor.Sync()
				_, err = unix.FcntlInt(donor.Fd(), 110, int(f.Fd())) // F_TRANSFEREXTENTS
				r.Transfer = reachResult(err)
				_ = donor.Close()
			} else {
				r.Transfer = "donor: " + err.Error()
			}
			_ = f.Close()
		}
		r.Link = reachResult(os.Link(outside, filepath.Join(work, "linked")))
		r.Clone = reachResult(unix.Clonefile(outside, filepath.Join(work, "cloned"), 0))
		buf := make([]byte, 256) // struct proc_bsdinfo is 136 bytes
		info := func(pid int) error {
			// proc_pidinfo(pid, PROC_PIDTBSDINFO): proc_info call 2, flavour 3
			if _, _, e := syscall.Syscall6(syscall.SYS_PROC_INFO, 2, uintptr(pid), 3, 0,
				uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); e != 0 {
				return e
			}
			return nil
		}
		r.SelfInfo = info(os.Getpid()) == nil
		r.ParentInfo = reachResult(info(os.Getppid()))
		b, _ := json.Marshal(r)
		fmt.Println(string(b))
		return 0
	}
}

// MS-2, SI-55: what Seatbelt allows by default even under (deny default) is refused by a run's profile.
// The agent's runtime (like the anet binary), which it may read, cannot be truncated nor handed another
// file's blocks through a descriptor opened for reading (F_MAKECOMPRESSED, F_TRANSFEREXTENTS: the kernel
// checks only the file's permission bits); a file outside the run gets neither a hard link nor a clone in
// the work dir; process information on a process outside the sandbox is refused. Reading the runtime, an
// ordinary fcntl and process information on itself still work, and outside the sandbox the same link
// succeeds.
func TestSeatbeltRefusesWhatItWouldAllowByDefault(t *testing.T) {
	if _, err := os.Stat(seatbeltExecPath); err != nil {
		t.Skipf("needs %s (a Mac): %v", seatbeltExecPath, err)
	}
	run, work := seatbeltRunDirs(t)
	base := filepath.Dir(run)
	rt, outside := filepath.Join(base, "runtime"), filepath.Join(base, "outside")
	for _, d := range []string{rt, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runtimeFile, outsideFile := filepath.Join(rt, "lib.dat"), filepath.Join(outside, "secret")
	want := bytes.Repeat([]byte("r"), 64<<10)
	if err := os.WriteFile(runtimeFile, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("outside the run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The setup is one in which a link works: the same volume, outside the sandbox.
	if err := os.Link(outsideFile, filepath.Join(outside, "host-link")); err != nil {
		t.Fatalf("outside the sandbox the link fails too: %v", err)
	}
	code, out := seatbeltRunIn(t, run, work, []string{rt}, selfExe(t), seatbeltReachArg, runtimeFile, outsideFile, work)
	var r seatbeltReach
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &r) != nil {
		t.Fatalf("the helper in the sandbox: exit %d\n%s", code, out)
	}
	t.Logf("in the sandbox: %+v", r)
	if !r.ReadRuntime || !r.GetFL || !r.SelfInfo {
		t.Errorf("the controls failed (read %v, F_GETFL %v, own process information %v): the profile is broken", r.ReadRuntime, r.GetFL, r.SelfInfo)
	}
	if r.Compress != "EPERM" || r.Transfer != "EPERM" {
		t.Errorf("fcntl through a descriptor opened for reading: F_MAKECOMPRESSED %s, F_TRANSFEREXTENTS %s, want EPERM", r.Compress, r.Transfer)
	}
	if r.Link == "done" || r.Clone == "done" {
		t.Errorf("the outside file got a name in the work dir: link %s, clone %s", r.Link, r.Clone)
	}
	for _, n := range []string{"linked", "cloned"} {
		if _, err := os.Lstat(filepath.Join(work, n)); err == nil {
			t.Errorf("%s exists in the work dir", n)
		}
	}
	if r.ParentInfo == "done" {
		t.Error("process information on a process outside the sandbox was given")
	}
	if got, err := os.ReadFile(runtimeFile); err != nil || !bytes.Equal(got, want) {
		t.Errorf("the runtime's file changed (%d bytes, %v)", len(got), err)
	}
}
