//go:build linux

package daemon

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
	"strings"
	"testing"
	"time"
)

// bindSources returns the source path of every bind flag in a bwrap argv.
func bindSources(argv []string) []string {
	var out []string
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--" {
			break
		}
		switch argv[i] {
		case "--bind", "--ro-bind", "--ro-bind-try", "--bind-try", "--dev-bind", "--dev-bind-try":
			out = append(out, argv[i+1])
			i += 2
		}
	}
	return out
}

func hasSeq(argv []string, seq ...string) bool { return seqIndex(argv, seq...) >= 0 }

// seqIndex is the index where seq starts in argv, or -1.
func seqIndex(argv []string, seq ...string) int {
	for i := 0; i+len(seq) <= len(argv); i++ {
		ok := true
		for j := range seq {
			if argv[i+j] != seq[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

func countArg(argv []string, a string) int {
	n := 0
	for _, x := range argv {
		if x == a {
			n++
		}
	}
	return n
}

// goldenBwrapSpec is the fixed input of G-BWRAP: a merged-/usr host, an anet binary in /opt, a user
// whose cache is under the home directory, a claude-style runtime under the home directory.
func goldenBwrapSpec(f bwrapFeatures) bwrapSpec {
	return bwrapSpec{
		Init:       "/opt/anet/bin/anet",
		EgressSock: "/home/u/.cache/anet/run/Golden01-0123456789/egress.sock",
		RunDir:     "/home/u/.cache/anet/run/Golden01-0123456789",
		Home:       "/home/u",
		WorkDir:    "/home/u/.cache/anet/work-sandbox/EaidForGolden001/ix-golden",
		RuntimeRO:  []string{"/home/u/.local/bin", "/home/u/.local/share/claude/versions/1.0.0", "/usr/lib/node_modules/x"},
		NeverBind:  []string{"/home/u/.anet", "/home/u/.claude", "/home/u/.cache/anet/work", "/run/user/1000", "/tmp/anet-1000"},
		System: []sandboxSystemEntry{{Path: "/usr"}, {Path: "/bin", Link: "usr/bin"}, {Path: "/sbin", Link: "usr/sbin"},
			{Path: "/lib", Link: "usr/lib"}, {Path: "/lib64", Link: "usr/lib64"}},
		Features: f,
	}
}

// G-BWRAP: the complete command line for the fixed input, with and without --disable-userns and --size,
// each with and without the systemd-run wrapper, is byte-for-byte the vector in testdata/bwrap.
func TestBwrapGolden(t *testing.T) {
	onOff := map[bool]string{true: "on", false: "off"}
	for _, userns := range []bool{true, false} {
		for _, size := range []bool{true, false} {
			args, err := buildBwrapArgs(goldenBwrapSpec(bwrapFeatures{DisableUserns: userns, Size: size}))
			if err != nil {
				t.Fatal(err)
			}
			for _, systemd := range []string{"", "/usr/bin/systemd-run"} {
				path, argv := sandboxCommandLine("/usr/bin/bwrap", systemd, args)
				name := fmt.Sprintf("disable-userns-%s_size-%s", onOff[userns], onOff[size])
				if systemd != "" {
					name += "_systemd-run"
				}
				goldenCompare(t, filepath.Join("testdata", "bwrap", name+".argv"),
					[]byte(strings.Join(append([]string{path}, argv...), "\n")+"\n"))
			}
		}
	}
}

// SB-1: the namespaces, the dropped capabilities, the filter on fd 5, init at /run/anet/init and the
// egress socket under the /run tmpfs, $HOME, /tmp and /var/tmp from the run directory, the work dir last,
// no resolver directory, and init (never the agent) at the end.
func TestBwrapArgsHaveTheSandboxShape(t *testing.T) {
	for _, f := range []bwrapFeatures{{true, true}, {false, false}, {true, false}, {false, true}} {
		spec := goldenBwrapSpec(f)
		argv, err := buildBwrapArgs(spec)
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"--unshare-user", "--unshare-net", "--unshare-ipc", "--unshare-pid",
			"--unshare-uts", "--unshare-cgroup-try", "--new-session", "--die-with-parent"} {
			if countArg(argv, flag) != 1 {
				t.Errorf("%+v: %s appears %d times", f, flag, countArg(argv, flag))
			}
		}
		if !hasSeq(argv, "--cap-drop", "ALL") || !hasSeq(argv, "--seccomp", "5") {
			t.Errorf("%+v: no --cap-drop ALL or --seccomp 5: %v", f, argv)
		}
		if hasSeq(argv, "--disable-userns") != f.DisableUserns {
			t.Errorf("%+v: --disable-userns present = %v", f, hasSeq(argv, "--disable-userns"))
		}
		if f.Size {
			if !hasSeq(argv, "--size", "67108864", "--tmpfs", "/dev/shm") || !hasSeq(argv, "--size", "16777216", "--tmpfs", "/run") {
				t.Errorf("%+v: tmpfs sizes missing: %v", f, argv)
			}
		} else if countArg(argv, "--size") != 0 || !hasSeq(argv, "--tmpfs", "/dev/shm") || !hasSeq(argv, "--tmpfs", "/run") {
			t.Errorf("%+v: --size used without support, or a tmpfs missing: %v", f, argv)
		}
		initAt := seqIndex(argv, "--ro-bind", spec.Init, "/run/anet/init")
		egressAt := seqIndex(argv, "--bind", spec.EgressSock, "/run/anet/egress.sock")
		runAt := seqIndex(argv, "--tmpfs", "/run")
		homeAt := seqIndex(argv, "--bind", filepath.Join(spec.RunDir, "home"), spec.Home)
		workAt := seqIndex(argv, "--bind", spec.WorkDir, spec.WorkDir)
		if initAt < 0 || egressAt < 0 || homeAt < 0 || workAt < 0 {
			t.Fatalf("%+v: a bind is missing: %v", f, argv)
		}
		if !(runAt < initAt && runAt < egressAt) {
			t.Errorf("%+v: --tmpfs /run must come before the binds under /run: %v", f, argv)
		}
		if !(homeAt < workAt) {
			t.Errorf("%+v: the home bind must come before the work dir: %v", f, argv)
		}
		for _, r := range spec.RuntimeRO {
			if at := seqIndex(argv, "--ro-bind", r, r); r != "/usr/lib/node_modules/x" && (at < homeAt || at > workAt) {
				t.Errorf("%+v: runtime %s bound at %d, not between the home (%d) and the work dir (%d)", f, r, at, homeAt, workAt)
			}
		}
		if hasSeq(argv, "--ro-bind", "/usr/lib/node_modules/x", "/usr/lib/node_modules/x") {
			t.Errorf("%+v: a runtime dir under /usr was bound again", f)
		}
		for _, m := range [][2]string{{"tmp", "/tmp"}, {"vartmp", "/var/tmp"}} {
			if !hasSeq(argv, "--bind", filepath.Join(spec.RunDir, m[0]), m[1]) {
				t.Errorf("%+v: %s is not the run directory's %s/", f, m[1], m[0])
			}
		}
		if countArg(argv, "--bind") != 5 {
			t.Errorf("%+v: %d writable binds, want 5 (egress, tmp, vartmp, home, work)", f, countArg(argv, "--bind"))
		}
		joined := strings.Join(argv, "\n")
		for _, banned := range []string{"/run/systemd/resolve", "/etc/resolv.conf"} {
			if strings.Contains(joined, banned) {
				t.Errorf("%+v: %s is bound", f, banned)
			}
		}
		if hasSeq(argv, "--tmpfs", "/tmp") || hasSeq(argv, "--ro-bind", "/etc", "/etc") || hasSeq(argv, "--bind", spec.Home, spec.Home) {
			t.Errorf("%+v: /tmp as tmpfs, /etc whole, or the real home bound: %v", f, argv)
		}
		tail := argv[len(argv)-5:]
		if strings.Join(tail, " ") != "-- /run/anet/init __sandbox-init --spec-fd 3" {
			t.Errorf("%+v: the command must be init and nothing else: %v", f, tail)
		}
		for _, src := range bindSources(argv) {
			if n, bad := bindConflict(src, spec.NeverBind); bad {
				t.Errorf("%+v: bind %s exposes never-bind path %s", f, src, n)
			}
		}
	}
	spec := goldenBwrapSpec(bwrapFeatures{})
	spec.Probe, spec.EgressSock = true, ""
	argv, err := buildBwrapArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(argv[len(argv)-5:], " ") != "-- /run/anet/init __sandbox-probe --spec-fd 3" {
		t.Errorf("the probe command: %v", argv[len(argv)-5:])
	}
	if strings.Contains(strings.Join(argv, " "), "egress.sock") {
		t.Errorf("no egress socket was given, yet one is bound: %v", argv)
	}
}

// Every bind source is checked against the never-bind list, the init binary and the egress socket
// included; relative paths and a home of / are refused.
func TestBwrapArgsRefuseUnsafeBinds(t *testing.T) {
	base := goldenBwrapSpec(bwrapFeatures{true, true})
	for name, mutate := range map[string]func(*bwrapSpec){
		"runtime dir containing the data dir": func(s *bwrapSpec) { s.RuntimeRO = []string{"/home/u"} },
		"runtime dir inside the data dir":     func(s *bwrapSpec) { s.RuntimeRO = []string{"/home/u/.anet/bin"} },
		"work dir inside the data dir":        func(s *bwrapSpec) { s.WorkDir = "/home/u/.anet/work" },
		"work dir under the direct work root": func(s *bwrapSpec) { s.WorkDir = "/home/u/.cache/anet/work/aid/ix" },
		"init inside the data dir":            func(s *bwrapSpec) { s.Init = "/home/u/.anet/anet" },
		"egress socket in the runtime dir":    func(s *bwrapSpec) { s.EgressSock = "/run/user/1000/egress.sock" },
		"run dir inside a never-bind path":    func(s *bwrapSpec) { s.RunDir = "/tmp/anet-1000/run" },
		"relative work dir":                   func(s *bwrapSpec) { s.WorkDir = "work" },
		"relative init":                       func(s *bwrapSpec) { s.Init = "anet" },
		"relative egress socket":              func(s *bwrapSpec) { s.EgressSock = "egress.sock" },
		"home of /":                           func(s *bwrapSpec) { s.Home = "/" },
		"no home":                             func(s *bwrapSpec) { s.Home = "" },
	} {
		s := base
		s.RuntimeRO = append([]string{}, base.RuntimeRO...)
		mutate(&s)
		if _, err := buildBwrapArgs(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// SB-1, last item: the agent's program, arguments and environment are not on any command line anet
// starts. They are in the spec on fd 3, which carries the agent's argv and environment, its working
// directory, no stdin and the limits; fd 5 carries the host's filter. The run directory has home/, tmp/
// and vartmp/ and is gone after Close.
func TestSandboxCommandKeepsTheAgentOffTheCommandLine(t *testing.T) {
	agent := writeStub(t, "#!/bin/sh\necho hi\n")
	work := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	plan := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: "EaidForCommandTest0001", Home: home,
		Features: bwrapFeatures{DisableUserns: true, Size: true}}
	const prompt = "CANARY-PROMPT-7f3a: the peer's words"
	const key = "CANARY-KEY-91bd"
	l, err := plan.command(work, []string{agent, "-p", prompt}, []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=" + key, "HOME=/elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != "/usr/bin/bwrap" || l.Env == nil || len(l.Env) != 0 {
		t.Fatalf("launch: path %q env %v", l.Path, l.Env)
	}
	if strings.Contains(strings.Join(l.Args, " "), "CANARY") {
		t.Errorf("the bwrap command line carries the conversation or the key: %v", l.Args)
	}
	for _, a := range l.Args {
		if a == agent || a == "-p" {
			t.Errorf("the bwrap command line carries the agent's command: %v", l.Args)
		}
	}
	if strings.Join(l.Args[len(l.Args)-5:], " ") != "-- /run/anet/init __sandbox-init --spec-fd 3" {
		t.Errorf("the command line does not end with init: %v", l.Args)
	}
	if len(l.Files) != 3 || l.Files[0] == nil || l.Files[1] != nil || l.Files[2] == nil {
		t.Fatalf("descriptors: %v", l.Files)
	}
	raw, err := io.ReadAll(l.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	var spec sandboxInitSpec
	if err := decodeSandboxSpec(raw, &spec); err != nil || spec.check() != nil {
		t.Fatalf("spec %s: %v", raw, err)
	}
	realAgent, _ := filepath.EvalSymlinks(agent)
	if !(spec.Argv[0] == agent || spec.Argv[0] == realAgent) || spec.Argv[2] != prompt || spec.Cwd != work ||
		spec.Stdin != "null" || spec.Bridge != nil {
		t.Errorf("spec: %+v", spec)
	}
	if *spec.Rlimits != (sandboxRlimitsT{Fsize: 256 << 20, Nofile: 4096, Core: 0}) {
		t.Errorf("limits: %+v", *spec.Rlimits)
	}
	env := strings.Join(spec.Env, "\n")
	if !strings.Contains(env, "ANTHROPIC_API_KEY="+key) || !strings.Contains(env, "HOME="+home) || strings.Contains(env, "HOME=/elsewhere") {
		t.Errorf("spec env: %v", spec.Env)
	}
	prog, _ := io.ReadAll(l.Files[2])
	if want, _ := seccompProgramForHost(); !bytes.Equal(prog, want) {
		t.Errorf("fd 5 carries %d bytes, not the host's filter", len(prog))
	}
	runDir := ""
	for i := 0; i+2 < len(l.Args); i++ {
		if l.Args[i] == "--bind" && l.Args[i+2] == "/tmp" {
			runDir = filepath.Dir(l.Args[i+1])
		}
	}
	if filepath.Dir(runDir) != sandboxRunRoot() || !strings.HasPrefix(filepath.Base(runDir), "Test0001-") {
		t.Errorf("run directory %q is not <cache>/anet/run/<aid8>-<rand>", runDir)
	}
	for _, sub := range []string{"", "home", "tmp", "vartmp"} {
		fi, err := os.Stat(filepath.Join(runDir, sub))
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("run directory %s/%s: %v %v", runDir, sub, fi, err)
		}
	}
	// A read-only directory the agent left behind does not keep the run directory alive.
	ro := filepath.Join(runDir, "tmp", "modcache")
	_ = os.MkdirAll(filepath.Join(ro, "pkg"), 0o700)
	_ = os.WriteFile(filepath.Join(ro, "pkg", "f"), []byte("x"), 0o400)
	_ = os.Chmod(filepath.Join(ro, "pkg"), 0o500)
	_ = os.Chmod(ro, 0o500)
	l.Close()
	if _, err := os.Stat(runDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the run directory survived Close: %v", err)
	}
}

// A spec over the 1 MiB init accepts is refused before anything starts, and leaves no run directory.
func TestSandboxCommandRefusesAnOversizedSpec(t *testing.T) {
	agent := writeStub(t, "#!/bin/sh\necho hi\n")
	plan := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: "EaidOversized0000001", Home: t.TempDir()}
	_, err := plan.command(t.TempDir(), []string{agent, strings.Repeat("x", sandboxSpecMax)}, nil)
	if err == nil || errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("an oversized spec: %v", err)
	}
	ents, _ := os.ReadDir(sandboxRunRoot())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "d0000001-") {
			t.Errorf("run directory %s left behind", e.Name())
		}
	}
}

// SB-3: the never-bind list covers the resolved data dir (not only ~/.anet), the runtime dirs, the
// direct-run work root, the configured and the default ANetLink sockets and the agent credential dirs;
// a sandbox work dir under the direct-run root is refused.
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
	var pe *sandboxProbeError
	if !errors.Is(err, errSandboxUnavailable) || !errors.As(err, &pe) || pe.Code != sandboxCodeNoBwrap {
		t.Fatalf("without bwrap the plan must be unavailable with no_bwrap, got %v", err)
	}
	never := d.sandboxNeverBind()
	home, _ := os.UserHomeDir()
	realData, _ := filepath.EvalSymlinks(d.layout.Root)
	for _, want := range []string{realData, legacyRuntimeDir(), xdg, sock, filepath.Join(xdg, "anetlink", "c1.sock"),
		execWorkRoot(), filepath.Join(home, ".claude"), filepath.Join(home, ".codex")} {
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
	direct, err := execInteractionDir(d.layout.Root, "EaidNeverBind0000001", "ix")
	if err != nil {
		t.Fatal(err)
	}
	sandboxed, err := execSandboxInteractionDir(d.layout.Root, "EaidNeverBind0000001", "ix")
	if err != nil {
		t.Fatal(err)
	}
	spec := bwrapSpec{Init: "/usr/bin/true", RunDir: t.TempDir(), Home: "/home/x", NeverBind: never}
	spec.WorkDir = direct
	if _, err := buildBwrapArgs(spec); err == nil {
		t.Error("a work dir under the direct-run root was bound into the sandbox")
	}
	spec.WorkDir = sandboxed
	if _, err := buildBwrapArgs(spec); err != nil {
		t.Errorf("the sandbox work dir was refused: %v", err)
	}
	if pathWithin(sandboxed, execWorkRoot()) || pathWithin(direct, execSandboxWorkRoot()) {
		t.Errorf("the two work roots overlap: %s %s", direct, sandboxed)
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

// --- the probe (SB-2) ---

func TestBwrapHelpParsing(t *testing.T) {
	newer := "usage: bwrap [OPTIONS...] [--] COMMAND [ARGS...]\n    --disable-userns             Disable further use of user namespaces inside sandbox\n" +
		"    --size BYTES                 Set size of next argument (only for --tmpfs)\n"
	older := "usage: bwrap [OPTIONS...] [--] COMMAND [ARGS...]\n    --unshare-user  Create new user namespace\n" +
		"    --tmpfs DEST    Mount new tmpfs on DEST\n    --sizeable-flag-that-is-not-size\n"
	if f := parseBwrapHelp(newer); !f.DisableUserns || !f.Size {
		t.Errorf("0.8 help: %+v", f)
	}
	if f := parseBwrapHelp(older); f.DisableUserns || f.Size {
		t.Errorf("older help: %+v", f)
	}
}

// fakeBwrap writes a stand-in bwrap: --help prints the given flags; a run saves its arguments, fd 3 (the
// probe spec) and fd 5 (the filter) under dir, counts the run, then runs body.
func fakeBwrap(t *testing.T, dir, helpFlags, body string) string {
	t.Helper()
	p := filepath.Join(dir, "bwrap")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --help ]; then echo 'usage: bwrap'; for f in " + helpFlags + "; do echo \"    $f   x\"; done; exit 0; fi\n" +
		"echo run >> " + dir + "/runs\n" +
		"printf '%s\\n' \"$@\" > " + dir + "/args\n" +
		"cat <&3 > " + dir + "/spec\n" +
		"cat <&5 > " + dir + "/prog\n" +
		body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func reportJSON(r sandboxCheckReport) string {
	b, _ := json.Marshal(r)
	return "echo '" + string(b) + "'"
}

// The probe's outcome becomes the local reason code of UX §1.4 H: a bwrap that cannot load the filter is
// seccomp_failed; one that cannot make a user namespace is userns_blocked under Ubuntu's AppArmor
// restriction and userns_unavailable elsewhere; a probe whose items 1-3 fail is netns_failed and whose
// items 4-5 fail is seccomp_failed. The probe runs with the same namespace flags and the same filter as a
// real run.
func TestSandboxProbeFailureCodes(t *testing.T) {
	all := sandboxCheckReport{LoUp: true, HostTCPRefused: true, AbstractRefused: true, KeyctlEPERM: true, UsernsRefused: true}
	noLo, noKeyctl, noUserns := all, all, all
	noLo.LoUp, noKeyctl.KeyctlEPERM, noUserns.UsernsRefused = false, false, false
	restricted := filepath.Join(t.TempDir(), "restrict")
	defer func(p string) { apparmorRestrictUsernsPath = p }(apparmorRestrictUsernsPath)
	for _, tc := range []struct {
		name, sysctl, body, want string
	}{
		{"apparmor", "1", "echo 'bwrap: setting up uid map: Permission denied' >&2; exit 1", sandboxCodeUsernsBlocked},
		{"apparmor-loopback", "1", "echo 'bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted' >&2; exit 1", sandboxCodeUsernsBlocked},
		{"no-userns", "0", "echo 'bwrap: No permissions to create new namespace' >&2; exit 1", sandboxCodeUsernsUnavailable},
		{"no-sysctl", "", "echo 'bwrap: Creating new namespace failed: No space left on device' >&2; exit 1", sandboxCodeUsernsUnavailable},
		{"seccomp-load", "1", "echo 'bwrap: prctl(PR_SET_SECCOMP): Invalid argument' >&2; exit 1", sandboxCodeSeccompFailed},
		{"sigsys", "0", "exit 159", sandboxCodeSeccompFailed},
		{"probe-died", "0", "echo 'anet: the sandbox self-check spec is not usable.' >&2; exit 125", sandboxCodeNetnsFailed},
		{"lo-down", "0", reportJSON(noLo), sandboxCodeNetnsFailed},
		{"keyctl", "0", reportJSON(noKeyctl), sandboxCodeSeccompFailed},
		{"userns", "0", reportJSON(noUserns), sandboxCodeSeccompFailed},
		{"ok", "1", reportJSON(all), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			apparmorRestrictUsernsPath = filepath.Join(dir, "missing")
			if tc.sysctl != "" {
				apparmorRestrictUsernsPath = restricted
				_ = os.WriteFile(restricted, []byte(tc.sysctl+"\n"), 0o644)
			}
			bw := fakeBwrap(t, dir, "--disable-userns --size", tc.body)
			pr := runBwrapProbe(bw, detectBwrapFeatures(bw))
			var pe *sandboxProbeError
			switch {
			case tc.want == "" && pr.Err != nil:
				t.Fatalf("a passing probe was refused: %v", pr.Err)
			case tc.want != "" && (!errors.As(pr.Err, &pe) || pe.Code != tc.want || !errors.Is(pr.Err, errSandboxUnavailable)):
				t.Fatalf("got %v, want %s", pr.Err, tc.want)
			}
			if !pr.Features.DisableUserns || !pr.Features.Size {
				t.Errorf("features: %+v", pr.Features)
			}
			args, _ := os.ReadFile(filepath.Join(dir, "args"))
			argv := strings.Split(strings.TrimSpace(string(args)), "\n")
			for _, flag := range []string{"--unshare-user", "--unshare-net", "--unshare-ipc", "--unshare-pid", "--unshare-uts",
				"--unshare-cgroup-try", "--new-session", "--die-with-parent", "--disable-userns"} {
				if countArg(argv, flag) != 1 {
					t.Errorf("the probe ran without %s: %v", flag, argv)
				}
			}
			if !hasSeq(argv, "--seccomp", "5") || !hasSeq(argv, "--cap-drop", "ALL") ||
				strings.Join(argv[len(argv)-5:], " ") != "-- /run/anet/init __sandbox-probe --spec-fd 3" {
				t.Errorf("probe arguments: %v", argv)
			}
			prog, _ := os.ReadFile(filepath.Join(dir, "prog"))
			if want, _ := seccompProgramForHost(); !bytes.Equal(prog, want) {
				t.Errorf("the probe ran with another filter (%d bytes)", len(prog))
			}
			specRaw, _ := os.ReadFile(filepath.Join(dir, "spec"))
			var spec sandboxCheckSpec
			if err := json.Unmarshal(specRaw, &spec); err != nil || spec.V != 1 || !strings.HasPrefix(spec.TCP, "127.0.0.1:") || spec.Abstract == "" {
				t.Errorf("probe spec %s: %v", specRaw, err)
			}
		})
	}
}

// A bwrap without --disable-userns and --size is probed and used without them.
func TestSandboxProbeFollowsTheFlagsBwrapHas(t *testing.T) {
	dir := t.TempDir()
	bw := fakeBwrap(t, dir, "--unshare-user", reportJSON(sandboxCheckReport{LoUp: true, HostTCPRefused: true,
		AbstractRefused: true, KeyctlEPERM: true, UsernsRefused: true}))
	pr := runBwrapProbe(bw, detectBwrapFeatures(bw))
	if pr.Err != nil || pr.Features.DisableUserns || pr.Features.Size {
		t.Fatalf("probe %+v", pr)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if strings.Contains(string(args), "--disable-userns") || strings.Contains(string(args), "--size") {
		t.Errorf("flags this bwrap lacks were passed: %s", args)
	}
}

// A probe result is reused for five minutes, then the probe runs again; prepare reports its code.
func TestSandboxProbeIsCachedForFiveMinutes(t *testing.T) {
	dir := t.TempDir()
	bw := fakeBwrap(t, dir, "--disable-userns --size", "echo 'bwrap: setting up uid map: Permission denied' >&2; exit 1")
	bwrapProbeCache.Lock()
	bwrapProbeCache.last = nil
	bwrapProbeCache.Unlock()
	defer func() {
		bwrapProbeCache.Lock()
		bwrapProbeCache.last = nil
		bwrapProbeCache.Unlock()
		bwrapLookPath = exec.LookPath
	}()
	bwrapLookPath = func(string) (string, error) { return bw, nil }
	runs := func() int {
		b, _ := os.ReadFile(filepath.Join(dir, "runs"))
		return strings.Count(string(b), "run")
	}
	for i := 0; i < 3; i++ {
		err := (&sandboxPlan{}).prepare()
		var pe *sandboxProbeError
		if !errors.As(err, &pe) || (pe.Code != sandboxCodeUsernsBlocked && pe.Code != sandboxCodeUsernsUnavailable) {
			t.Fatalf("prepare: %v", err)
		}
	}
	if runs() != 1 {
		t.Fatalf("the probe ran %d times within five minutes", runs())
	}
	bwrapProbeCache.Lock()
	bwrapProbeCache.last.At = time.Now().Add(-bwrapProbeTTL - time.Second)
	bwrapProbeCache.Unlock()
	_ = (&sandboxPlan{}).prepare()
	if runs() != 2 {
		t.Fatalf("an expired result was reused (%d runs)", runs())
	}
}

// realBwrapProbe runs the probe with the host's bubblewrap. A host whose kernel or policy gives bwrap no
// user namespace skips (the CI sandbox job and the lab hosts treat a skip as a failure); a sandbox that
// bwrap builds but that fails a check is a failure here.
func realBwrapProbe(t *testing.T, feats func(bwrapFeatures) bwrapFeatures) bwrapProbe {
	t.Helper()
	path, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not installed")
	}
	pr := runBwrapProbe(path, feats(detectBwrapFeatures(path)))
	var pe *sandboxProbeError
	if errors.As(pr.Err, &pe) && (pe.Code == sandboxCodeUsernsBlocked || pe.Code == sandboxCodeUsernsUnavailable) {
		t.Skipf("bwrap cannot create namespaces here: %v", pr.Err)
	}
	return pr
}

// SB-2 on the real thing: inside the sandbox lo is up, the host's loopback port and abstract socket are
// out of reach, keyctl answers EPERM and no user namespace can be made (M-SB-1, M-SB-2, M-SB-3).
func TestSandboxProbeFiveItems(t *testing.T) {
	pr := realBwrapProbe(t, func(f bwrapFeatures) bwrapFeatures { return f })
	t.Logf("bwrap features %+v; report %+v", pr.Features, pr.Report)
	if pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	r := pr.Report
	for name, ok := range map[string]bool{"1 lo up": r.LoUp, "2 host loopback port refused": r.HostTCPRefused,
		"3 abstract socket refused": r.AbstractRefused, "4 keyctl EPERM": r.KeyctlEPERM, "5 user namespace refused": r.UsernsRefused} {
		if !ok {
			t.Errorf("item %s does not hold: %v", name, r.Detail)
		}
	}
}

// Item 5 holds on the filter alone: the probe with --disable-userns left out (as with a bwrap older than
// 0.8) still cannot create a user namespace (M-SB-3).
func TestSandboxProbeSeccompAloneRefusesUserNamespaces(t *testing.T) {
	pr := realBwrapProbe(t, func(f bwrapFeatures) bwrapFeatures { f.DisableUserns = false; return f })
	if pr.Err != nil || !pr.Report.UsernsRefused {
		t.Fatalf("without --disable-userns: %v %+v", pr.Err, pr.Report)
	}
}

// Integration: with a working bubblewrap, a sandboxed agent (the test binary acting as the probe)
// cannot read the control or A2A token, cannot connect to a Unix socket in the data dir or to the
// system bus, does not see the data dir or the host's /tmp, and can write its work dir, which is the
// interaction's sandbox work dir. Skipped where bwrap is missing or cannot create namespaces.
func TestSandboxIntegrationConfinesTheAgent(t *testing.T) {
	pr := realBwrapProbe(t, func(f bwrapFeatures) bwrapFeatures { return f })
	if pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
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
	marker, err := os.CreateTemp("", "anet-host-tmp-marker-")
	if err != nil {
		t.Fatal(err)
	}
	marker.Close()
	defer os.Remove(marker.Name())
	req, _ := json.Marshal(sandboxProbeRequest{
		ControlToken: filepath.Join(data, "control_token.txt"),
		A2AToken:     filepath.Join(data, "a2a_token.txt"),
		Socket:       sock, DBus: dbus, DataDir: data, HostTmpMarker: marker.Name(),
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
	if got.HostTmpVisible {
		t.Errorf("the host's /tmp was visible: %+v", got)
	}
	if !got.WorkDirWritable {
		t.Errorf("the work dir was not writable: %+v", got)
	}
	wantWork, _ := execSandboxInteractionDir(d.layout.Root, d.AID(), "ix-probe")
	if got.Cwd != wantWork {
		t.Errorf("the agent ran in %q, want the interaction's sandbox work dir %q", got.Cwd, wantWork)
	}
	if _, err := os.Stat(filepath.Join(wantWork, "probe-wrote-this")); err != nil {
		t.Errorf("the write inside the sandbox did not reach the host work dir: %v", err)
	}
	ents, _ := os.ReadDir(sandboxRunRoot())
	if len(ents) != 0 {
		t.Errorf("run directories left behind: %d", len(ents))
	}
}
