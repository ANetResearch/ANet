//go:build unix

package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sandboxTestAgentArg starts the test binary as a stand-in agent (runSandboxTestAgent).
const sandboxTestAgentArg = "__sandbox-test-agent"

func init() {
	testMainHooks["__sandbox-init"] = SandboxInitMain
	testMainHooks["__sandbox-probe"] = SandboxProbeMain
	testMainHooks[sandboxTestAgentArg] = runSandboxTestAgent
}

// sandboxAgentReport is what the stand-in agent saw.
type sandboxAgentReport struct {
	Argv    []string             `json:"argv"`
	Env     []string             `json:"env"`
	Cwd     string               `json:"cwd"`
	Stdin   string               `json:"stdin"`
	Rlimits map[string][2]uint64 `json:"rlimits"`
}

// runSandboxTestAgent: "report" prints a sandboxAgentReport; "exit N" exits N; "kill-self" dies of
// SIGKILL; "bridge ADDR" sends "ping" to ADDR, closes its writing side and prints the answer.
func runSandboxTestAgent(args []string) int {
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "report":
		in, _ := io.ReadAll(os.Stdin)
		cwd, _ := os.Getwd()
		rep := sandboxAgentReport{Argv: os.Args, Env: os.Environ(), Cwd: cwd, Stdin: string(in), Rlimits: map[string][2]uint64{}}
		for name, res := range map[string]int{"core": syscall.RLIMIT_CORE, "fsize": syscall.RLIMIT_FSIZE, "nofile": syscall.RLIMIT_NOFILE} {
			var l syscall.Rlimit
			_ = syscall.Getrlimit(res, &l)
			rep.Rlimits[name] = [2]uint64{uint64(l.Cur), uint64(l.Max)}
		}
		b, _ := json.Marshal(rep)
		fmt.Println(string(b))
		return 0
	case "exit":
		n, _ := strconv.Atoi(args[1])
		return n
	case "kill-self":
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Minute)
		return 0
	case "bridge":
		c, err := net.Dial("tcp", args[1])
		if err != nil {
			fmt.Println("dial:", err)
			return 1
		}
		_, _ = c.Write([]byte("ping"))
		_ = c.(*net.TCPConn).CloseWrite()
		b, _ := io.ReadAll(c)
		fmt.Print(string(b))
		return 0
	}
	return 2
}

func selfExe(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// initRun is one run of `__sandbox-init` as bwrap would start it: spec on fd 3, conversation on fd 4,
// extra descriptors after them.
type initRun struct {
	args   []string // default --spec-fd 3
	spec   []byte   // nil: fd 3 not open
	stdin  []byte   // nil: fd 4 not open
	extra  []*os.File
	env    []string
	onRun  func(cmd *exec.Cmd, stdout *bufio.Reader)
	stdout string
	stderr string
	code   int
}

func (r *initRun) run(t *testing.T) {
	t.Helper()
	args := r.args
	if args == nil {
		args = []string{"--spec-fd", "3"}
	}
	cmd := exec.Command(os.Args[0], append([]string{"__sandbox-init"}, args...)...)
	cmd.Env = r.env
	if cmd.Env == nil {
		cmd.Env = []string{"INIT_ONLY=must-not-reach-the-agent", "LANG=C"}
	}
	var files []*os.File
	pipeOf := func(data []byte) *os.File {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = pw.Write(data); _ = pw.Close() }()
		t.Cleanup(func() { pr.Close() })
		return pr
	}
	if r.spec != nil {
		files = append(files, pipeOf(r.spec))
	} else {
		files = append(files, nil)
	}
	if r.stdin != nil {
		files = append(files, pipeOf(r.stdin))
	} else if len(r.extra) > 0 {
		files = append(files, nil)
	}
	files = append(files, r.extra...)
	cmd.ExtraFiles = files
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(out)
	if r.onRun != nil {
		r.onRun(cmd, br)
	}
	rest, _ := io.ReadAll(br)
	err = cmd.Wait()
	r.stdout += string(rest)
	r.stderr = stderr.String()
	r.code = 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		r.code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
}

func initSpecFor(t *testing.T, argv ...string) sandboxInitSpec {
	return newSandboxInitSpec(argv, []string{"A=1", "CANARY_ENV=spec-only"}, t.TempDir(), false, "")
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// SB-5: started without its spec descriptor, init is an internal command: exit 2 and one line on stderr,
// in the daemon's language.
func TestSandboxInitWithoutSpecIsAnInternalCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		lang string
		want string
	}{
		{[]string{}, "C", "internal command"},
		{[]string{"--spec-fd", "3"}, "C", "internal command"},     // fd 3 not open
		{[]string{"--spec-fd", "1"}, "C", "internal command"},     // below 3
		{[]string{"--spec-fd", "three"}, "C", "internal command"}, // not a number
		{[]string{}, "zh_CN.UTF-8", "内部命令"},
	} {
		r := &initRun{args: tc.args, env: []string{"LANG=" + tc.lang}}
		r.run(t)
		if r.code != 2 || strings.Count(r.stderr, "\n") != 1 || !strings.Contains(r.stderr, tc.want) || r.stdout != "" {
			t.Errorf("%v %s: exit %d, stdout %q, stderr %q", tc.args, tc.lang, r.code, r.stdout, r.stderr)
		}
	}
	// The same from the anet command's own entry point.
	if code := SandboxInitMain(nil); code != 2 {
		t.Errorf("SandboxInitMain(nil) = %d", code)
	}
	if code := SandboxProbeMain(nil); code != 2 {
		t.Errorf("SandboxProbeMain(nil) = %d", code)
	}
}

// SB-5: a spec over 1 MiB, of another version, with an unknown key, trailing data, a relative program or
// working directory, an unknown stdin, no limits, a malformed environment entry or a half bridge exits
// 125; the agent never starts and nothing of the spec is printed.
func TestSandboxInitRefusesAnUnusableSpec(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "agent-ran")
	good := initSpecFor(t, "/bin/sh", "-c", "touch "+marker)
	good.Env = append(good.Env, "CANARY_SECRET=spec-canary-55e1")
	b := func(mut func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal(mustJSONBytes(t, good), &m)
		mut(m)
		return mustJSONBytes(t, m)
	}
	big := good
	big.Env = append(append([]string{}, good.Env...), "PAD="+strings.Repeat("x", sandboxSpecMax))
	cases := map[string][]byte{
		"over 1 MiB":        mustJSONBytes(t, big),
		"version 2":         b(func(m map[string]any) { m["v"] = 2 }),
		"no version":        b(func(m map[string]any) { delete(m, "v") }),
		"unknown key":       b(func(m map[string]any) { m["extra"] = true }),
		"relative program":  b(func(m map[string]any) { m["argv"] = []string{"sh", "-c", "touch " + marker} }),
		"empty argv":        b(func(m map[string]any) { m["argv"] = []string{} }),
		"relative cwd":      b(func(m map[string]any) { m["cwd"] = "work" }),
		"stdin fd9":         b(func(m map[string]any) { m["stdin"] = "fd9" }),
		"no limits":         b(func(m map[string]any) { delete(m, "rlimits") }),
		"null env":          b(func(m map[string]any) { m["env"] = nil }),
		"env without =":     b(func(m map[string]any) { m["env"] = []string{"NOEQUALS"} }),
		"half bridge":       b(func(m map[string]any) { m["bridge"] = map[string]string{"listen": "127.0.0.1:3128"} }),
		"unknown limit key": b(func(m map[string]any) { m["rlimits"].(map[string]any)["as"] = 1 }),
		"trailing data":     append(mustJSONBytes(t, good), []byte(` {"v":1}`)...),
		"not JSON":          []byte("CANARY_SECRET=spec-canary-55e1"),
		"empty":             {},
	}
	for name, spec := range cases {
		r := &initRun{spec: spec}
		r.run(t)
		if r.code != 125 || strings.Count(r.stderr, "\n") != 1 {
			t.Errorf("%s: exit %d, stderr %q", name, r.code, r.stderr)
		}
		if strings.Contains(r.stderr+r.stdout, "canary") || strings.Contains(r.stderr+r.stdout, marker) {
			t.Errorf("%s: init printed the spec: %q", name, r.stderr)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the agent ran", name)
		}
	}
	// The unchanged spec does run the agent.
	r := &initRun{spec: mustJSONBytes(t, good)}
	r.run(t)
	if _, err := os.Stat(marker); r.code != 0 || err != nil {
		t.Fatalf("a good spec: exit %d, %v, stderr %q", r.code, err, r.stderr)
	}
}

// SB-5: the agent gets exactly the spec's argv, environment and working directory (nothing of init's own
// environment), its stdin from fd 4, and RLIMIT_CORE 0, RLIMIT_FSIZE 256 MiB and RLIMIT_NOFILE 4096 as
// both soft and hard limits; init itself writes nothing to stdout.
func TestSandboxInitRunsTheAgentAsTheSpecSays(t *testing.T) {
	self := selfExe(t)
	spec := initSpecFor(t, self, sandboxTestAgentArg, "report", "an argument with spaces")
	spec.Stdin = "fd4"
	cwd, _ := filepath.EvalSymlinks(spec.Cwd)
	spec.Cwd = cwd
	r := &initRun{spec: mustJSONBytes(t, spec), stdin: []byte("the conversation\nsecond line\n")}
	r.run(t)
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var rep sandboxAgentReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("stdout is not only the agent's output: %q", r.stdout)
	}
	if strings.Join(rep.Argv, "\x00") != strings.Join(spec.Argv, "\x00") {
		t.Errorf("argv %q, want %q", rep.Argv, spec.Argv)
	}
	if strings.Join(rep.Env, "\x00") != strings.Join(spec.Env, "\x00") {
		t.Errorf("env %q, want exactly %q", rep.Env, spec.Env)
	}
	if rep.Cwd != cwd {
		t.Errorf("cwd %q, want %q", rep.Cwd, cwd)
	}
	if rep.Stdin != "the conversation\nsecond line\n" {
		t.Errorf("stdin %q", rep.Stdin)
	}
	var hard syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &hard)
	wantNofile := uint64(4096)
	if uint64(hard.Max) < wantNofile {
		wantNofile = uint64(hard.Max)
	}
	for name, want := range map[string]uint64{"core": 0, "fsize": 256 << 20, "nofile": wantNofile} {
		if got := rep.Rlimits[name]; got != [2]uint64{want, want} {
			t.Errorf("RLIMIT_%s = %v, want soft and hard %d", strings.ToUpper(name), got, want)
		}
	}
}

// With "stdin": "null" the agent reads nothing, even when a descriptor 4 is open.
func TestSandboxInitStdinIsNullWithoutAConversation(t *testing.T) {
	spec := initSpecFor(t, selfExe(t), sandboxTestAgentArg, "report")
	r := &initRun{spec: mustJSONBytes(t, spec), stdin: []byte("not for the agent")}
	r.run(t)
	var rep sandboxAgentReport
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil || rep.Stdin != "" {
		t.Fatalf("stdin %q (%v, exit %d, %s)", rep.Stdin, err, r.code, r.stderr)
	}
}

// SB-5: init exits with the agent's status, 128+n when signal n ended it, and 127 when the program does
// not exist (printing neither the spec nor the program's path).
func TestSandboxInitPassesTheAgentsExitStatus(t *testing.T) {
	self := selfExe(t)
	for _, tc := range []struct {
		argv []string
		want int
	}{
		{[]string{self, sandboxTestAgentArg, "exit", "0"}, 0},
		{[]string{self, sandboxTestAgentArg, "exit", "7"}, 7},
		{[]string{self, sandboxTestAgentArg, "kill-self"}, 128 + 9},
		{[]string{"/nonexistent/canary-program-3d1f", "x"}, 127},
	} {
		r := &initRun{spec: mustJSONBytes(t, initSpecFor(t, tc.argv...))}
		r.run(t)
		if r.code != tc.want {
			t.Errorf("%v: exit %d, want %d (%s)", tc.argv[1:], r.code, tc.want, r.stderr)
		}
		if strings.Contains(r.stderr, "canary") || strings.Contains(r.stderr, "spec-only") {
			t.Errorf("%v: init printed the spec: %q", tc.argv, r.stderr)
		}
	}
}

// SB-5: SIGTERM and SIGINT sent to init reach the agent's whole process group: a shell and the sleep it
// started both end on SIGTERM, and init reports the shell's death as 143. (A shell without job control
// starts background commands with SIGINT ignored, so SIGINT is shown on a foreground sleep: 130.)
func TestSandboxInitForwardsSignalsToTheAgentsGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	for sig, script := range map[syscall.Signal]string{
		syscall.SIGTERM: "sleep 60 & echo $!; wait",
		syscall.SIGINT:  "echo $$; exec sleep 60",
	} {
		spec := initSpecFor(t, "/bin/sh", "-c", script)
		var sleepPid int
		r := &initRun{spec: mustJSONBytes(t, spec), onRun: func(cmd *exec.Cmd, out *bufio.Reader) {
			line, err := out.ReadString('\n')
			if err != nil {
				t.Errorf("no pid from the agent: %v", err)
				return
			}
			sleepPid, _ = strconv.Atoi(strings.TrimSpace(line))
			_ = cmd.Process.Signal(sig)
		}}
		r.run(t)
		if r.code != 128+int(sig) {
			t.Errorf("%v: init exited %d, want %d (%s)", sig, r.code, 128+int(sig), r.stderr)
		}
		deadline := time.Now().Add(5 * time.Second)
		for processRunning(sleepPid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if processRunning(sleepPid) {
			_ = syscall.Kill(sleepPid, syscall.SIGKILL)
			t.Errorf("%v did not reach the agent's child %d", sig, sleepPid)
		}
	}
}

// processRunning reports a live (not zombie) process.
func processRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || i+2 >= len(s) || s[i+2] != 'Z'
}

// SB-5: descriptors init inherited (the spec, the conversation, the filter bwrap may leave open, anything
// else) do not reach the agent: it starts with 0, 1 and 2 only.
func TestSandboxInitClosesInheritedDescriptors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	stray, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stray.Close()
	stray2, _ := os.CreateTemp(t.TempDir(), "stray")
	defer stray2.Close()
	spec := initSpecFor(t, "/bin/sh", "-c", "ls /proc/$$/fd")
	spec.Stdin = "fd4"
	r := &initRun{spec: mustJSONBytes(t, spec), stdin: []byte("x"), extra: []*os.File{stray, stray2}}
	r.run(t)
	fds := strings.Fields(r.stdout)
	if r.code != 0 || strings.Join(fds, " ") != "0 1 2" {
		t.Fatalf("the agent's descriptors: %q (exit %d, %s)", r.stdout, r.code, r.stderr)
	}
}

// SB-5: with a bridge, init listens on the given address and joins each connection to the egress
// socket, half closes included.
func TestSandboxInitBridgesToTheEgressSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "e.sock")
	uds, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer uds.Close()
	go func() {
		for {
			c, err := uds.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b, _ := io.ReadAll(c) // the client's half close ends this
				_, _ = c.Write(append([]byte("pong:"), b...))
			}(c)
		}
	}()
	probe, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := probe.Addr().String()
	probe.Close()
	spec := newSandboxInitSpec([]string{selfExe(t), sandboxTestAgentArg, "bridge", addr}, []string{}, t.TempDir(), false, sock)
	spec.Bridge.Listen = addr // the sandbox's own 127.0.0.1:3128 is the host's here
	r := &initRun{spec: mustJSONBytes(t, spec)}
	r.run(t)
	if r.code != 0 || r.stdout != "pong:ping" {
		t.Fatalf("through the bridge: %q (exit %d, %s)", r.stdout, r.code, r.stderr)
	}
}

// The spec init reads and the one the daemon writes agree on every field (newSandboxInitSpec).
func TestSandboxSpecRoundTrip(t *testing.T) {
	s := newSandboxInitSpec([]string{"/a", "b"}, nil, "/w", true, "/run/anet/egress.sock")
	raw := mustJSONBytes(t, s)
	want := `{"v":1,"argv":["/a","b"],"env":[],"cwd":"/w","stdin":"fd4","bridge":{"listen":"127.0.0.1:3128","uds":"/run/anet/egress.sock"},"rlimits":{"fsize":268435456,"nofile":4096,"core":0}}`
	if string(raw) != want {
		t.Errorf("spec\n got %s\nwant %s", raw, want)
	}
	var back sandboxInitSpec
	if err := decodeSandboxSpec(raw, &back); err != nil || back.check() != nil {
		t.Fatalf("round trip: %v", err)
	}
}

// FuzzSandboxSpec: the spec decoder never panics, refuses anything over 1 MiB, and whatever it accepts
// meets the rules check enforces and survives a round trip unchanged. The probe's spec goes through the
// same decoder.
func FuzzSandboxSpec(f *testing.F) {
	for _, s := range []sandboxInitSpec{
		newSandboxInitSpec([]string{"/usr/bin/claude", "-p"}, []string{"A=1", "HOME=/home/u"}, "/w", false, ""),
		newSandboxInitSpec([]string{"/a"}, nil, "/", true, "/run/anet/egress.sock"),
	} {
		f.Add(mustJSONBytesF(s))
	}
	f.Add([]byte(`{"v":1,"tcp":"127.0.0.1:1","abstract":"x"}`))
	f.Add([]byte(`{"v":1,"argv":["/a"],"env":[],"cwd":"/","stdin":"null","bridge":null,"rlimits":{"fsize":1,"nofile":2,"core":3}} {}`))
	f.Add([]byte(`{"v":1,"argv":["/a"],"env":["\ud800=x"],"cwd":"/\u0000","stdin":"fd4","rlimits":{"fsize":-1}}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var probe sandboxCheckSpec
		_ = decodeSandboxSpec(raw, &probe)
		var s sandboxInitSpec
		if decodeSandboxSpec(raw, &s) != nil || s.check() != nil {
			return
		}
		if len(raw) > sandboxSpecMax {
			t.Fatalf("accepted %d bytes", len(raw))
		}
		if s.V != 1 || len(s.Argv) == 0 || !strings.HasPrefix(s.Argv[0], "/") || !strings.HasPrefix(s.Cwd, "/") ||
			(s.Stdin != "fd4" && s.Stdin != "null") || s.Rlimits == nil || s.Env == nil {
			t.Fatalf("accepted a spec that breaks the rules: %+v", s)
		}
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var back sandboxInitSpec
		if err := decodeSandboxSpec(b, &back); err != nil || back.check() != nil {
			t.Fatalf("re-encoded spec refused: %v", err)
		}
		if b2, _ := json.Marshal(back); string(b2) != string(b) {
			t.Fatalf("round trip changed the spec:\n%s\n%s", b, b2)
		}
	})
}

func mustJSONBytesF(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
