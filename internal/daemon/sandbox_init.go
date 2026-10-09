//go:build unix

package daemon

// sandbox_init.go is `anet __sandbox-init` and `anet __sandbox-probe` (LEC SB-5, SB-2, MS-4, §2.6.5): the
// first program that runs inside a sandbox. bwrap (Linux) starts `/run/anet/init __sandbox-init --spec-fd
// 3`, where /run/anet/init is this binary bound read-only; sandbox-exec (macOS) starts this binary where
// it is, with the same arguments. Neither command line carries anything of the agent's: its argv,
// environment and working directory arrive on fd 3 as a JSON spec, written by the daemon and read here
// once.
//
// init then, in this order: reads and closes the spec; opens the agent's stdin (fd 4, or /dev/null);
// marks every other inherited descriptor close-on-exec; sets RLIMIT_CORE, RLIMIT_FSIZE and RLIMIT_NOFILE;
// when the spec has a bridge, listens on the sandbox's own 127.0.0.1:3128 and joins each connection to
// the run's egress socket (/run/anet/egress.sock), which is the only way out of the sandbox's network
// namespace; starts the agent, in a process group of its own on Linux and in init's own group on macOS
// (sandboxAgentSharesGroup); forwards SIGTERM and SIGINT to the agent's group; on macOS ends what the
// agent left in the group once it exits; and exits with the agent's status (128+n when a signal ended
// it).
//
// It never prints the spec: the agent's arguments and environment can hold the conversation and, until
// the egress endpoint injects them, a model key. Its own messages are one line on stderr; stdout belongs
// to the agent, whose reply the daemon reads from it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

const (
	// sandboxInitPath is where the anet binary is bound inside the sandbox.
	sandboxInitPath = "/run/anet/init"
	// sandboxEgressPath is where the run's egress socket is bound inside a Linux sandbox.
	sandboxEgressPath = "/run/anet/egress.sock"
	// sandboxBridgeListen is the sandbox-side address of the egress endpoint (LEC §2.6.1).
	sandboxBridgeListen = "127.0.0.1:3128"
	// sandboxSpecFd is the descriptor the spec arrives on; sandboxStdinFd carries the conversation;
	// sandboxSeccompFd is the filter bwrap reads (--seccomp 5).
	sandboxSpecFd    = 3
	sandboxStdinFd   = 4
	sandboxSeccompFd = 5
	// sandboxSpecMax bounds the spec init reads.
	sandboxSpecMax = 1 << 20
	// sandboxSpecVersion is the spec's "v".
	sandboxSpecVersion = 1
	// Resource limits init sets for the agent (LEC SB-5, §5.1).
	sandboxRlimitFsize  = 256 << 20
	sandboxRlimitNofile = 4096
	sandboxRlimitCore   = 0

	// Exit statuses of init itself: 2 when started by hand (no spec descriptor), 125 when the spec is
	// unusable or the sandbox cannot be set up, 126 or 127 when the agent cannot be started.
	sandboxInitExitUsage    = 2
	sandboxInitExitBadSpec  = 125
	sandboxInitExitCannot   = 126
	sandboxInitExitNotFound = 127
)

// sandboxInitSpec is the spec on fd 3 (LEC §2.6.5). It stays on this machine: the daemon writes it into
// a pipe, init reads it.
type sandboxInitSpec struct {
	V       int              `json:"v"`
	Argv    []string         `json:"argv"`
	Env     []string         `json:"env"`
	Cwd     string           `json:"cwd"`
	Stdin   string           `json:"stdin"` // "fd4" | "null"
	Bridge  *sandboxBridge   `json:"bridge"`
	Rlimits *sandboxRlimitsT `json:"rlimits"`
}

// sandboxBridge is the egress bridge: listen inside the sandbox, forward to the run's socket.
type sandboxBridge struct {
	Listen string `json:"listen"`
	UDS    string `json:"uds"`
}

// sandboxRlimitsT are the limits init sets, soft and hard, before it starts the agent.
type sandboxRlimitsT struct {
	Fsize  uint64 `json:"fsize"`
	Nofile uint64 `json:"nofile"`
	Core   uint64 `json:"core"`
}

// newSandboxInitSpec is the spec the daemon writes for one agent process.
func newSandboxInitSpec(argv, env []string, cwd string, stdinFd4 bool, bridgeUDS string) sandboxInitSpec {
	s := sandboxInitSpec{
		V: sandboxSpecVersion, Argv: argv, Env: env, Cwd: cwd, Stdin: "null",
		Rlimits: &sandboxRlimitsT{Fsize: sandboxRlimitFsize, Nofile: sandboxRlimitNofile, Core: sandboxRlimitCore},
	}
	if s.Env == nil {
		s.Env = []string{}
	}
	if stdinFd4 {
		s.Stdin = "fd4"
	}
	if bridgeUDS != "" {
		s.Bridge = &sandboxBridge{Listen: sandboxBridgeListen, UDS: bridgeUDS}
	}
	return s
}

// sandboxProbeError is a sandbox that cannot be used here, with the local reason code (UX §1.4 H:
// no_bwrap, userns_blocked, userns_unavailable, netns_failed, seccomp_failed) and a detail for the
// operator. errors.Is reports it as errSandboxUnavailable, so every caller fails the turn closed.
type sandboxProbeError struct {
	Code   string
	Detail string
}

func (e *sandboxProbeError) Error() string {
	return fmt.Sprintf("%v: %s: %s", errSandboxUnavailable, e.Code, e.Detail)
}

func (e *sandboxProbeError) Unwrap() error { return errSandboxUnavailable }

// errBadSandboxSpec is every reason a spec is refused. Its text never quotes the spec.
var errBadSandboxSpec = errors.New("the sandbox spec is not usable")

// decodeSandboxSpec parses and checks a spec: one JSON object of the known keys, version 1, an absolute
// program and working directory, a known stdin, limits present, and a bridge with both ends when there is
// one.
func decodeSandboxSpec(raw []byte, v any) error {
	if len(raw) > sandboxSpecMax {
		return errBadSandboxSpec
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errBadSandboxSpec
	}
	if _, err := dec.Token(); err != io.EOF {
		return errBadSandboxSpec
	}
	return nil
}

func (s *sandboxInitSpec) check() error {
	if s.V != sandboxSpecVersion || len(s.Argv) == 0 || !strings.HasPrefix(s.Argv[0], "/") ||
		!strings.HasPrefix(s.Cwd, "/") || (s.Stdin != "fd4" && s.Stdin != "null") || s.Rlimits == nil ||
		s.Env == nil {
		return errBadSandboxSpec
	}
	if s.Bridge != nil && (s.Bridge.Listen == "" || s.Bridge.UDS == "") {
		return errBadSandboxSpec
	}
	for _, kv := range s.Env {
		if !strings.Contains(kv, "=") {
			return errBadSandboxSpec
		}
	}
	return nil
}

// SandboxInitMain is `anet __sandbox-init --spec-fd 3`. It returns the process exit status.
func SandboxInitMain(args []string) int {
	fd, ok := sandboxSpecFdArg(args)
	if !ok {
		sandboxInitSay("内部命令,由 anet 在沙箱里启动,不需要手动运行。", "internal command: anet starts it inside the sandbox; there is no need to run it by hand.")
		return sandboxInitExitUsage
	}
	raw, err := readSandboxSpec(fd)
	if err != nil {
		sandboxInitSay("沙箱规格读取失败。", "could not read the sandbox spec.")
		return sandboxInitExitBadSpec
	}
	var spec sandboxInitSpec
	if decodeSandboxSpec(raw, &spec) != nil || spec.check() != nil {
		sandboxInitSay("沙箱规格不可用。", "the sandbox spec is not usable.")
		return sandboxInitExitBadSpec
	}
	return runSandboxInit(spec)
}

// sandboxSpecFdArg accepts exactly "--spec-fd N" with N ≥ 3 naming an inherited pipe.
func sandboxSpecFdArg(args []string) (int, bool) {
	var s string
	switch {
	case len(args) == 2 && args[0] == "--spec-fd":
		s = args[1]
	case len(args) == 1 && strings.HasPrefix(args[0], "--spec-fd="):
		s = strings.TrimPrefix(args[0], "--spec-fd=")
	default:
		return 0, false
	}
	fd, err := strconv.Atoi(s)
	if err != nil || fd < 3 || !inheritedPipe(fd) {
		return 0, false
	}
	return fd, true
}

// inheritedPipe reports whether fd is open and a pipe. The daemon passes the spec and the conversation
// as pipes. The check matters because a descriptor the parent left closed does not stay free: the Go
// runtime opens files of its own at start (since Go 1.25, the cgroup's cpu.max), and the lowest free
// number is what it gets.
func inheritedPipe(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFIFO
}

// readSandboxSpec reads at most sandboxSpecMax+1 bytes from fd and closes it.
func readSandboxSpec(fd int) ([]byte, error) {
	f := os.NewFile(uintptr(fd), "sandbox-spec")
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, sandboxSpecMax+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > sandboxSpecMax {
		return nil, errBadSandboxSpec
	}
	return raw, nil
}

func runSandboxInit(spec sandboxInitSpec) int {
	stdin, err := sandboxInitStdin(spec.Stdin)
	if err != nil {
		sandboxInitSay("沙箱的对话输入不可用。", "the sandbox's conversation input is not available.")
		return sandboxInitExitBadSpec
	}
	// Nothing init inherited beyond stdio may reach the agent: fd 4 is passed on as its stdin only.
	closeOnExecFrom(3)
	if err := setSandboxRlimits(*spec.Rlimits); err != nil {
		sandboxInitSay("沙箱资源限制设置失败。", "could not set the sandbox's resource limits.")
		return sandboxInitExitBadSpec
	}
	if spec.Bridge != nil {
		ln, err := net.Listen("tcp", spec.Bridge.Listen)
		if err != nil {
			sandboxInitSay("沙箱出站端点无法监听。", "the sandbox's egress endpoint cannot listen.")
			return sandboxInitExitBadSpec
		}
		defer ln.Close()
		go serveSandboxBridge(ln, spec.Bridge.UDS)
	}

	cmd := &exec.Cmd{
		Path: spec.Argv[0], Args: spec.Argv, Env: spec.Env, Dir: spec.Cwd,
		Stdin: stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: !sandboxAgentSharesGroup},
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		var errno syscall.Errno
		code := sandboxInitExitCannot
		if errors.As(err, &errno) && errno == syscall.ENOENT || errors.Is(err, os.ErrNotExist) {
			code = sandboxInitExitNotFound
		}
		reason := "?"
		if errors.As(err, &errno) {
			reason = errno.Error()
		}
		sandboxInitSay("agent 无法启动:"+reason+"。", "the agent cannot be started: "+reason+".")
		return code
	}
	if f, ok := stdin.(*os.File); ok {
		_ = f.Close()
	}
	pgid := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				forwardSandboxSignal(pgid, s.(syscall.Signal))
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	endSandboxAgentGroup()
	return sandboxExitStatus(cmd.ProcessState, err)
}

// sandboxExitStatus is the agent's exit status, or 128+n when signal n ended it.
func sandboxExitStatus(ps *os.ProcessState, err error) int {
	if ps == nil {
		if err != nil {
			return sandboxInitExitCannot
		}
		return 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// sandboxInitStdin is fd 4 or /dev/null.
func sandboxInitStdin(mode string) (io.Reader, error) {
	if mode == "fd4" {
		if !inheritedPipe(sandboxStdinFd) {
			return nil, errBadSandboxSpec
		}
		return os.NewFile(sandboxStdinFd, "conversation"), nil
	}
	return os.Open(os.DevNull)
}

// closeOnExecFrom marks every open descriptor ≥ from close-on-exec, so exec passes the agent only what
// exec.Cmd hands it explicitly. Where neither directory of open descriptors can be read, every number
// below the agent's descriptor limit is marked (a number that is not open is left as it is).
func closeOnExecFrom(from int) {
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if fd, err := strconv.Atoi(e.Name()); err == nil && fd >= from {
				syscall.CloseOnExec(fd)
			}
		}
		return
	}
	for fd := from; fd < sandboxRlimitNofile; fd++ {
		syscall.CloseOnExec(fd)
	}
}

// setSandboxRlimits sets each limit as both the soft and the hard value, never above the hard limit init
// was given (an unprivileged process cannot raise it).
func setSandboxRlimits(l sandboxRlimitsT) error {
	for _, r := range []struct {
		res int
		v   uint64
	}{{syscall.RLIMIT_CORE, l.Core}, {syscall.RLIMIT_FSIZE, l.Fsize}, {syscall.RLIMIT_NOFILE, l.Nofile}} {
		var cur syscall.Rlimit
		if err := syscall.Getrlimit(r.res, &cur); err != nil {
			return err
		}
		lim := cur
		if uint64(cur.Max) > r.v {
			setRlimitValue(&lim.Max, r.v)
		}
		lim.Cur = lim.Max
		if err := syscall.Setrlimit(r.res, &lim); err != nil {
			return err
		}
	}
	return nil
}

// setRlimitValue stores v in a limit field, which is uint64 on Linux and macOS and int64 on FreeBSD.
func setRlimitValue[T int64 | uint64](dst *T, v uint64) { *dst = T(v) }

// serveSandboxBridge joins every connection to the sandbox's 127.0.0.1:3128 to a fresh connection to the
// run's egress socket. The endpoint on the other side enforces everything; the bridge only carries bytes.
func serveSandboxBridge(ln net.Listener, uds string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			u, err := net.Dial("unix", uds)
			if err != nil {
				return
			}
			defer u.Close()
			pipeHalfClose(c, u)
		}()
	}
}

// pipeHalfClose copies both ways and passes each side's end of stream on as a half close, so a client
// that shuts down its writing side still reads the whole answer.
func pipeHalfClose(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// sandboxInitSay writes one line on stderr in the daemon's language.
func sandboxInitSay(zh, en string) {
	msg := en
	if sandboxLangZh() {
		msg = zh
	}
	fmt.Fprintln(os.Stderr, "anet: "+msg)
}

// sandboxLangZh reports a Chinese locale (LC_ALL, then LC_MESSAGES, then LANG).
func sandboxLangZh() bool {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			return strings.HasPrefix(strings.ToLower(v), "zh")
		}
	}
	return false
}

// --- the probe ---

// sandboxCheckSpec is the spec of `__sandbox-probe`: a TCP port the daemon listens on at the host's
// 127.0.0.1, which must be out of reach, and on Linux an abstract Unix socket it listens on, out of reach
// too (SB-2). On macOS (MS-4) also the stand-in for the run's egress endpoint, which must be reachable,
// the real home directory and a file outside the run, which must not be readable, and the work dir,
// which must take a file. Which fields a platform needs is its usable().
type sandboxCheckSpec struct {
	V        int    `json:"v"`
	TCP      string `json:"tcp"`
	Abstract string `json:"abstract"`
	Egress   string `json:"egress,omitempty"`
	Home     string `json:"home,omitempty"`
	Outside  string `json:"outside,omitempty"`
	Work     string `json:"work,omitempty"`
}

// sandboxCheckReport is what the probe found inside the sandbox (LEC SB-2 on Linux, MS-4 on macOS). The
// sandbox is usable only when the platform's five items all hold.
type sandboxCheckReport struct {
	LoUp            bool              `json:"lo_up"`            // SB-2 1: lo is up
	HostTCPRefused  bool              `json:"host_tcp_refused"` // SB-2 2, MS-4 2: the host's loopback port is unreachable
	AbstractRefused bool              `json:"abstract_refused"` // SB-2 3: the host's abstract socket is unreachable
	KeyctlEPERM     bool              `json:"keyctl_eperm"`     // SB-2 4: keyctl answers EPERM
	UsernsRefused   bool              `json:"userns_refused"`   // SB-2 5: a new user namespace cannot be made
	Detail          map[string]string `json:"detail,omitempty"` // what each check saw

	HomeUnreadable  bool `json:"home_unreadable,omitempty"`  // MS-4 1: neither the real home nor a file outside the run can be read
	EgressReached   bool `json:"egress_reached,omitempty"`   // MS-4 3: the run's egress endpoint can be reached
	SecurityRefused bool `json:"security_refused,omitempty"` // MS-4 4: /usr/bin/security does not start
	WorkWritable    bool `json:"work_writable,omitempty"`    // MS-4 5: the work dir takes a file
}

// netOK and filterOK group the five items the way the failure codes do (SB-2).
func (r sandboxCheckReport) netOK() bool    { return r.LoUp && r.HostTCPRefused && r.AbstractRefused }
func (r sandboxCheckReport) filterOK() bool { return r.KeyctlEPERM && r.UsernsRefused }

// sandboxProbeUsernsChild is the argument the probe starts itself with when it tries to create a user
// namespace; the child exits 0 at once.
const sandboxProbeUsernsChild = "--userns-child"

// SandboxProbeMain is `anet __sandbox-probe --spec-fd 3`: it runs the checks inside the sandbox and
// prints the report as one JSON line.
func SandboxProbeMain(args []string) int {
	if len(args) == 1 && args[0] == sandboxProbeUsernsChild {
		return 0
	}
	fd, ok := sandboxSpecFdArg(args)
	if !ok {
		sandboxInitSay("内部命令,由 anet 在沙箱里启动,不需要手动运行。", "internal command: anet starts it inside the sandbox; there is no need to run it by hand.")
		return sandboxInitExitUsage
	}
	raw, err := readSandboxSpec(fd)
	var spec sandboxCheckSpec
	if err != nil || decodeSandboxSpec(raw, &spec) != nil || spec.V != sandboxSpecVersion || !spec.usable() {
		sandboxInitSay("沙箱自检规格不可用。", "the sandbox self-check spec is not usable.")
		return sandboxInitExitBadSpec
	}
	rep := runSandboxChecks(spec)
	b, _ := json.Marshal(rep)
	fmt.Println(string(b))
	return 0
}
