//go:build linux

package daemon

// sandbox_linux.go builds the bubblewrap sandbox that runs a local agent for an untrusted peer (LEC
// §4.3.2, SB-1 to SB-3).
//
// bwrap starts from an empty root and creates every namespace it can: user, network, IPC, PID, UTS and
// cgroup (--unshare-cgroup-try), in a new session, dying with its parent, with every capability dropped
// and, where this bwrap supports it, no further user namespaces (--disable-userns). The seccomp filter of
// seccomp_linux.go arrives on fd 5 (--seccomp 5). The network namespace has only its own lo: the agent
// reaches the model through the run's egress socket, which init bridges to 127.0.0.1:3128 inside, and
// nothing else — not the host's loopback services, not abstract Unix sockets, not the LAN.
//
// The filesystem is built from what is listed:
//   - read-only: /usr and the /bin, /sbin and /lib* entries (as symlinks when the host has merged /usr),
//     the /etc files in sandboxEtc, and the agent's runtime (its binary's directory, npm package root,
//     script interpreter);
//   - /dev and a fresh /proc; tmpfs on /dev/shm and /run (64 MiB and 16 MiB where bwrap takes --size);
//   - the anet binary at /run/anet/init (read-only) and the run's egress socket at /run/anet/egress.sock;
//   - $HOME, /tmp and /var/tmp bound to the run directory's home/, tmp/ and vartmp/ (on disk, removed
//     after the run);
//   - read-write: the interaction's sandbox work dir, bound last because it usually lies under $HOME.
//
// bwrap runs `/run/anet/init __sandbox-init --spec-fd 3` and nothing else: the agent's program,
// arguments, environment and working directory reach init in the spec on fd 3 (sandbox_init.go), so no
// command line that other users can read carries any of them.
//
// Every bind source is checked against the never-bind list (the resolved data dir, the identities
// container, the runtime dirs, the direct-run work root, the ANetLink sockets and the agent CLIs'
// credential directories): a source that equals, contains or lies inside one of them makes the whole run
// unavailable rather than dropping the bind silently.
//
// Before the first run, and again every five minutes, probeBwrap starts the same sandbox around
// `/run/anet/init __sandbox-probe` and has it check five things from inside (SB-2): lo is up, the host's
// loopback port and an abstract socket the daemon listens on are out of reach, keyctl answers EPERM, and
// no user namespace can be created. A sandbox that fails any of them is not used.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// bwrapLookPath locates bubblewrap. Tests replace it to simulate a host without bwrap.
var bwrapLookPath = exec.LookPath

// sandboxEtc are the /etc entries bound read-only: user and group names, time zone, CA certificates, the
// dynamic linker cache, and alternatives symlinks some distributions route binaries through. Entries
// missing on the host are skipped (--ro-bind-try). resolv.conf is not among them: there is no DNS in the
// sandbox (CONNECT names are resolved by the daemon), so the host's resolver addresses would only be
// something an agent could read and send away.
var sandboxEtc = []string{
	"hosts", "host.conf", "nsswitch.conf", "gai.conf", "passwd", "group",
	"localtime", "timezone", "ssl", "ca-certificates", "ca-certificates.conf", "pki", "crypto-policies",
	"alternatives", "ld.so.cache", "ld.so.conf", "ld.so.conf.d", "os-release", "mime.types",
	"protocols", "services",
}

// sandboxSystemDirs are bound read-only (or recreated as symlinks when the host's entry is a symlink,
// as on merged-/usr systems).
var sandboxSystemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"}

// tmpfs sizes where bwrap takes --size (bytes).
const (
	sandboxShmSize = 64 << 20
	sandboxRunSize = 16 << 20
)

// systemd-run wrapper (LEC SB-7): memory and task limits through a user scope.
var sandboxSystemdRunArgs = []string{"--user", "--scope", "--quiet", "-p", "MemoryMax=2G", "-p", "TasksMax=256", "--"}

// bwrapProbeTTL is how long a probe result is reused.
const bwrapProbeTTL = 5 * time.Minute

// bwrapProbeTimeout bounds one probe run (the checks inside take a few seconds at most).
const bwrapProbeTimeout = 20 * time.Second

// Local reasons the sandbox is unavailable (UX §1.4 H). They reach the operator (status, doctor); a peer
// only ever sees sandbox_unavailable.
const (
	sandboxCodeNoBwrap           = "no_bwrap"
	sandboxCodeUsernsBlocked     = "userns_blocked"
	sandboxCodeUsernsUnavailable = "userns_unavailable"
	sandboxCodeNetnsFailed       = "netns_failed"
	sandboxCodeSeccompFailed     = "seccomp_failed"
)

// apparmorRestrictUsernsPath is the Ubuntu sysctl that stops unconfined programs from using user
// namespaces; tests point it elsewhere.
var apparmorRestrictUsernsPath = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

// bwrapProbe is one probe result.
type bwrapProbe struct {
	Path     string
	Features bwrapFeatures
	Report   sandboxCheckReport
	Err      error // nil when the sandbox is usable; else a *sandboxProbeError
	At       time.Time
}

var bwrapProbeCache struct {
	sync.Mutex
	last *bwrapProbe
}

// prepare locates bubblewrap and checks, with the probe, that the sandbox holds on this host.
func (p *sandboxPlan) prepare() error {
	path, err := bwrapLookPath("bwrap")
	if err != nil {
		return &sandboxProbeError{Code: sandboxCodeNoBwrap, Detail: "bubblewrap (bwrap) is not installed or not on PATH"}
	}
	pr := probeBwrap(path)
	if pr.Err != nil {
		return pr.Err
	}
	p.Bwrap, p.Features = path, pr.Features
	return nil
}

// probeBwrap returns the probe result for this bwrap, running the probe when there is no result younger
// than bwrapProbeTTL.
func probeBwrap(path string) bwrapProbe {
	bwrapProbeCache.Lock()
	defer bwrapProbeCache.Unlock()
	if c := bwrapProbeCache.last; c != nil && c.Path == path && time.Since(c.At) < bwrapProbeTTL {
		return *c
	}
	pr := runBwrapProbe(path, detectBwrapFeatures(path))
	bwrapProbeCache.last = &pr
	return pr
}

// detectBwrapFeatures reads bwrap's help for the optional flags the arguments use.
func detectBwrapFeatures(path string) bwrapFeatures {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--help")
	cmd.Env = []string{}
	out, _ := cmd.CombinedOutput()
	return parseBwrapHelp(string(out))
}

func parseBwrapHelp(help string) bwrapFeatures {
	has := func(flag string) bool {
		for _, f := range strings.Fields(help) {
			if f == flag {
				return true
			}
		}
		return false
	}
	return bwrapFeatures{DisableUserns: has("--disable-userns"), Size: has("--size")}
}

// runBwrapProbe starts the sandbox around `__sandbox-probe` with feats and classifies the outcome.
func runBwrapProbe(path string, feats bwrapFeatures) bwrapProbe {
	res := bwrapProbe{Path: path, Features: feats, At: time.Now()}
	fail := func(code, format string, args ...any) bwrapProbe {
		res.Err = &sandboxProbeError{Code: code, Detail: fmt.Sprintf(format, args...)}
		return res
	}
	prog, err := seccompProgramForHost()
	if err != nil {
		return fail(sandboxCodeSeccompFailed, "%v", err)
	}
	// The self-check itself could not be set up: reported as a failed self-check, with the detail.
	initExe, err := sandboxInitExe()
	if err != nil {
		return fail(sandboxCodeNetnsFailed, "cannot locate the anet binary: %v", err)
	}
	// Probe files live under the system temp dir only (LEC §5.5), removed before returning.
	dir, err := os.MkdirTemp("", "anet-probe-")
	if err != nil {
		return fail(sandboxCodeNetnsFailed, "probe directory: %v", err)
	}
	defer removeSandboxTree(dir)
	for _, sub := range []string{"home", "tmp", "vartmp", "work"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			return fail(sandboxCodeNetnsFailed, "probe directory: %v", err)
		}
	}

	// What must stay out of reach: a TCP port on the host's loopback and an abstract Unix socket. A
	// connection to either is a failure even if the probe claims otherwise.
	var reached atomic.Int32
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(sandboxCodeNetnsFailed, "host listener: %v", err)
	}
	defer tcpLn.Close()
	abstract := "anet-probe-" + randHex(8)
	absLn, err := net.Listen("unix", "@"+abstract)
	if err != nil {
		return fail(sandboxCodeNetnsFailed, "abstract listener: %v", err)
	}
	defer absLn.Close()
	for _, ln := range []net.Listener{tcpLn, absLn} {
		go func(ln net.Listener) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				reached.Add(1)
				_ = c.Close()
			}
		}(ln)
	}

	home := sandboxHome(dir)
	args, err := buildBwrapArgs(bwrapSpec{
		Init: initExe, RunDir: dir, Home: home, WorkDir: filepath.Join(dir, "work"),
		System: scanSystemDirs(), Features: feats, Probe: true,
	})
	if err != nil {
		return fail(sandboxCodeNetnsFailed, "%v", err)
	}
	spec, _ := json.Marshal(sandboxCheckSpec{V: sandboxSpecVersion, TCP: tcpLn.Addr().String(), Abstract: abstract})
	files, closeFiles, err := sandboxFds(spec, prog)
	if err != nil {
		return fail(sandboxCodeNetnsFailed, "%v", err)
	}
	defer closeFiles()

	ctx, cancel := context.WithTimeout(context.Background(), bwrapProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{}
	cmd.Dir = dir
	cmd.ExtraFiles = files
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	errText := strings.TrimSpace(stderr.String())

	var rep sandboxCheckReport
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &rep); jerr != nil {
		// The probe never reported: bwrap could not build the sandbox, or init died in it.
		if ctx.Err() != nil {
			return fail(sandboxCodeNetnsFailed, "the self-check did not finish in %s", bwrapProbeTimeout)
		}
		code := classifyBwrapFailure(runErr, errText)
		return fail(code, "%s", firstLine(errText, runErr))
	}
	res.Report = rep
	switch {
	case !rep.netOK() || reached.Load() > 0:
		return fail(sandboxCodeNetnsFailed, "network isolation self-check failed: %s", rep.summary(reached.Load()))
	case !rep.filterOK():
		return fail(sandboxCodeSeccompFailed, "system call filter self-check failed: %s", rep.summary(0))
	}
	return res
}

// classifyBwrapFailure names why bwrap ran no probe (SB-2): a filter it could not load, a user namespace
// Ubuntu's AppArmor policy forbids, or one this kernel or container does not allow.
func classifyBwrapFailure(runErr error, stderr string) string {
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Exited() && ws.ExitStatus() == 128+int(syscall.SIGSYS) {
			return sandboxCodeSeccompFailed
		}
	}
	low := strings.ToLower(stderr)
	if strings.Contains(low, "seccomp") {
		return sandboxCodeSeccompFailed
	}
	if !strings.Contains(low, "bwrap:") && stderr != "" {
		// bwrap built the sandbox; the probe inside failed before it could report.
		return sandboxCodeNetnsFailed
	}
	if b, err := os.ReadFile(apparmorRestrictUsernsPath); err == nil && strings.TrimSpace(string(b)) == "1" {
		return sandboxCodeUsernsBlocked
	}
	return sandboxCodeUsernsUnavailable
}

func firstLine(s string, err error) string {
	if s == "" && err != nil {
		return err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 300)
}

// summary lists the checks that failed, for the operator's detail line.
func (r sandboxCheckReport) summary(reached int32) string {
	var bad []string
	add := func(ok bool, name string) {
		if !ok {
			if d := r.Detail[name]; d != "" {
				name += " (" + d + ")"
			}
			bad = append(bad, name)
		}
	}
	add(r.LoUp, "lo")
	add(r.HostTCPRefused, "host_tcp")
	add(r.AbstractRefused, "abstract")
	add(r.KeyctlEPERM, "keyctl")
	add(r.UsernsRefused, "userns")
	if reached > 0 {
		bad = append(bad, fmt.Sprintf("host listeners reached %d times", reached))
	}
	return strings.Join(bad, "; ")
}

// runSandboxChecks runs the five checks of SB-2 inside the sandbox.
func runSandboxChecks(spec sandboxCheckSpec) sandboxCheckReport {
	r := sandboxCheckReport{Detail: map[string]string{}}
	note := func(k string, err error) {
		if err != nil {
			r.Detail[k] = err.Error()
		}
	}
	// 1. lo is up and takes a listener (the egress bridge listens on it).
	if ifc, err := net.InterfaceByName("lo"); err != nil {
		note("lo", err)
	} else if ifc.Flags&net.FlagUp == 0 {
		r.Detail["lo"] = "down"
	} else if ln, err := net.Listen("tcp", "127.0.0.1:0"); err != nil {
		note("lo", err)
	} else {
		_ = ln.Close()
		r.LoUp = true
	}
	// 2. The host's loopback port.
	if c, err := net.DialTimeout("tcp", spec.TCP, 2*time.Second); err != nil {
		r.HostTCPRefused = true
	} else {
		_ = c.Close()
		r.Detail["host_tcp"] = "connected"
	}
	// 3. The host's abstract socket.
	if c, err := net.DialTimeout("unix", "@"+spec.Abstract, 2*time.Second); err != nil {
		r.AbstractRefused = true
	} else {
		_ = c.Close()
		r.Detail["abstract"] = "connected"
	}
	// 4. keyctl(KEYCTL_GET_KEYRING_ID, KEY_SPEC_SESSION_KEYRING, create).
	sessionKeyring := int32(-3)
	if _, _, e := syscall.RawSyscall(syscall.SYS_KEYCTL, 0, uintptr(uint32(sessionKeyring)), 1); e == syscall.EPERM {
		r.KeyctlEPERM = true
	} else if e == 0 {
		r.Detail["keyctl"] = "succeeded"
	} else {
		r.Detail["keyctl"] = e.Error()
	}
	// 5. A new user namespace, by unshare(2) in a single-threaded child and by clone(2).
	unshareErr := trySandboxUserns(&syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWUSER})
	cloneErr := trySandboxUserns(&syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER})
	r.UsernsRefused = unshareErr != nil && cloneErr != nil
	if unshareErr == nil {
		r.Detail["userns"] = "unshare succeeded"
	} else if cloneErr == nil {
		r.Detail["userns"] = "clone succeeded"
	} else {
		r.Detail["userns"] = unshareErr.Error()
	}
	return r
}

// trySandboxUserns starts this binary as `__sandbox-probe --userns-child` (which exits 0 at once) with the
// namespace flags in attr. nil means the child ran, so the user namespace was created.
func trySandboxUserns(attr *syscall.SysProcAttr) error {
	self, err := os.Executable()
	if err != nil {
		self = sandboxInitPath
	}
	cmd := exec.Command(self, "__sandbox-probe", sandboxProbeUsernsChild)
	cmd.Env = []string{}
	cmd.SysProcAttr = attr
	return cmd.Run()
}

// --- arguments ---

// sandboxSystemEntry is one of sandboxSystemDirs as found on the host: a directory (Link "") or a
// symbolic link to Link.
type sandboxSystemEntry struct {
	Path, Link string
}

// scanSystemDirs looks at sandboxSystemDirs on this host.
func scanSystemDirs() []sandboxSystemEntry {
	var out []sandboxSystemEntry
	for _, d := range sandboxSystemDirs {
		fi, err := os.Lstat(d)
		if err != nil {
			continue
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			if target, err := os.Readlink(d); err == nil {
				out = append(out, sandboxSystemEntry{Path: d, Link: target})
			}
		case fi.IsDir():
			out = append(out, sandboxSystemEntry{Path: d})
		}
	}
	return out
}

// bwrapSpec is the input of buildBwrapArgs. Every field is decided before the call, so the arguments are
// a function of it alone (G-BWRAP).
type bwrapSpec struct {
	Init       string               // the anet binary, bound read-only at /run/anet/init
	EgressSock string               // the run's egress socket, bound at /run/anet/egress.sock ("" for none)
	RunDir     string               // the run directory, holding home/, tmp/ and vartmp/
	Home       string               // $HOME inside the sandbox, bound to RunDir/home
	WorkDir    string               // bound read-write at the same path
	RuntimeRO  []string             // agent runtime directories, bound read-only at the same paths
	NeverBind  []string             // no bind source may equal, contain or lie inside one of these
	System     []sandboxSystemEntry // the host's /usr, /bin, … (scanSystemDirs)
	Features   bwrapFeatures        // optional flags this bwrap takes
	Probe      bool                 // run __sandbox-probe instead of __sandbox-init
}

// buildBwrapArgs returns bwrap's arguments (without the program) for spec. It fails when a bind would
// expose a never-bind path or a path is unusable. The command at the end is init or the probe, never the
// agent.
func buildBwrapArgs(spec bwrapSpec) ([]string, error) {
	for _, p := range []struct{ name, path string }{{"anet binary", spec.Init}, {"run directory", spec.RunDir},
		{"sandbox home", spec.Home}, {"sandbox work dir", spec.WorkDir}} {
		if p.path == "" || !filepath.IsAbs(p.path) {
			return nil, fmt.Errorf("%s %q is not an absolute path", p.name, p.path)
		}
	}
	if filepath.Clean(spec.Home) == "/" {
		return nil, errors.New("the sandbox home cannot be /")
	}
	if spec.EgressSock != "" && !filepath.IsAbs(spec.EgressSock) {
		return nil, fmt.Errorf("egress socket %q is not an absolute path", spec.EgressSock)
	}
	a := []string{
		"--unshare-user", "--unshare-net", "--unshare-ipc", "--unshare-pid", "--unshare-uts", "--unshare-cgroup-try",
		"--new-session", "--die-with-parent", "--cap-drop", "ALL",
	}
	if spec.Features.DisableUserns {
		a = append(a, "--disable-userns")
	}
	a = append(a, "--seccomp", strconv.Itoa(sandboxSeccompFd))
	bindTo := func(flag, src, dst string) error {
		if n, bad := bindConflict(src, spec.NeverBind); bad {
			return fmt.Errorf("refusing to bind %s into the sandbox: it overlaps %s", src, n)
		}
		a = append(a, flag, src, dst)
		return nil
	}
	bind := func(flag, src string) error { return bindTo(flag, src, src) }
	for _, e := range spec.System {
		if e.Link != "" {
			a = append(a, "--symlink", e.Link, e.Path)
			continue
		}
		if err := bind("--ro-bind", e.Path); err != nil {
			return nil, err
		}
	}
	for _, e := range sandboxEtc {
		if err := bind("--ro-bind-try", filepath.Join("/etc", e)); err != nil {
			return nil, err
		}
	}
	a = append(a, "--dev", "/dev", "--proc", "/proc")
	tmpfs := func(path string, size int) {
		if spec.Features.Size {
			a = append(a, "--size", strconv.Itoa(size))
		}
		a = append(a, "--tmpfs", path)
	}
	tmpfs("/dev/shm", sandboxShmSize)
	tmpfs("/run", sandboxRunSize) // before the two binds under /run
	if err := bindTo("--ro-bind", spec.Init, sandboxInitPath); err != nil {
		return nil, err
	}
	if spec.EgressSock != "" {
		if err := bindTo("--bind", spec.EgressSock, sandboxEgressPath); err != nil {
			return nil, err
		}
	}
	for _, m := range []struct{ sub, dst string }{{"tmp", "/tmp"}, {"vartmp", "/var/tmp"}, {"home", spec.Home}} {
		if err := bindTo("--bind", filepath.Join(spec.RunDir, m.sub), m.dst); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	for _, r := range spec.RuntimeRO {
		if r == "" || seen[r] || underSystemDir(r) {
			continue
		}
		seen[r] = true
		if err := bind("--ro-bind", r); err != nil {
			return nil, err
		}
	}
	// Last: the work dir usually lies under $HOME, and a later mount covers an earlier one.
	if err := bind("--bind", spec.WorkDir); err != nil {
		return nil, err
	}
	sub := "__sandbox-init"
	if spec.Probe {
		sub = "__sandbox-probe"
	}
	return append(a, "--", sandboxInitPath, sub, "--spec-fd", strconv.Itoa(sandboxSpecFd)), nil
}

// sandboxCommandLine is the program and arguments that start bwrap with args: bwrap itself, or, when
// systemdRun names systemd-run, bwrap inside a user scope with the memory and task limits (SB-7).
func sandboxCommandLine(bwrap, systemdRun string, args []string) (string, []string) {
	if systemdRun == "" {
		return bwrap, append([]string{}, args...)
	}
	out := append(append([]string{}, sandboxSystemdRunArgs...), bwrap)
	return systemdRun, append(out, args...)
}

// underSystemDir reports a path already visible through the /usr, /bin, /sbin or /lib* binds.
func underSystemDir(p string) bool {
	for _, d := range sandboxSystemDirs {
		if pathWithin(p, d) {
			return true
		}
	}
	return false
}

// --- one sandboxed process ---

// command prepares one agent process for the sandbox: argv and env are the agent's command and complete
// environment, workDir its working directory. The returned launch runs bwrap with the spec on fd 3 and
// the filter on fd 5; Close removes the run directory and releases the descriptors after the process
// ended.
func (p *sandboxPlan) command(workDir string, argv, env []string) (*sandboxLaunch, error) {
	if p.Bwrap == "" {
		return nil, sandboxUnavailable("sandbox plan was not prepared")
	}
	if len(argv) == 0 {
		return nil, sandboxUnavailable("no agent command")
	}
	exe, runtimeDirs, err := agentRuntime(argv[0])
	if err != nil {
		return nil, sandboxUnavailable("agent binary %s: %v", argv[0], err)
	}
	initExe, err := sandboxInitExe()
	if err != nil {
		return nil, sandboxUnavailable("cannot locate the anet binary: %v", err)
	}
	prog, err := seccompProgramForHost()
	if err != nil {
		return nil, &sandboxProbeError{Code: sandboxCodeSeccompFailed, Detail: err.Error()}
	}
	runDir, err := newSandboxRunDir(p.AID)
	if err != nil {
		return nil, sandboxUnavailable("run directory: %v", err)
	}
	ok := false
	defer func() {
		if !ok {
			removeSandboxTree(runDir)
		}
	}()
	home := p.Home
	if home == "" || !filepath.IsAbs(home) || filepath.Clean(home) == "/" {
		home = sandboxHome(runDir)
	}
	args, err := buildBwrapArgs(bwrapSpec{
		Init:       initExe,
		EgressSock: "", // v030-todo(EXE-2): bind the run's egress socket and bridge it (spec.bridge)
		RunDir:     runDir, Home: home, WorkDir: workDir,
		RuntimeRO: append(runtimeDirs, p.ExtraRO...),
		NeverBind: p.NeverBind,
		System:    scanSystemDirs(),
		Features:  p.Features,
	})
	if err != nil {
		return nil, sandboxUnavailable("%v", err)
	}
	// v030-todo(EXE-4): wrap in systemd-run (SB-7) when the user's systemd is running.
	path, full := sandboxCommandLine(p.Bwrap, "", args)
	spec, err := json.Marshal(newSandboxInitSpec(append([]string{exe}, argv[1:]...), envWithHome(env, home), workDir, false, ""))
	if err != nil {
		return nil, err
	}
	if len(spec) > sandboxSpecMax {
		return nil, fmt.Errorf("the agent's command and environment are %d bytes, over the sandbox's %d", len(spec), sandboxSpecMax)
	}
	files, closeFiles, err := sandboxFds(spec, prog)
	if err != nil {
		return nil, sandboxUnavailable("%v", err)
	}
	ok = true
	return &sandboxLaunch{
		Path: path, Args: full,
		Env:   []string{}, // bwrap and init need nothing; the agent's environment is in the spec
		Files: files,
		Close: func() {
			closeFiles()
			removeSandboxTree(runDir)
		},
	}, nil
}

// sandboxFds returns the descriptors a sandboxed process starts with, as exec.Cmd.ExtraFiles: fd 3 a pipe
// carrying spec, fd 4 closed (no conversation input yet), fd 5 a pipe carrying the seccomp program. The
// spec is written from a goroutine, since it can be larger than a pipe holds; closeFiles closes the read
// ends and waits for that write to end.
func sandboxFds(spec, prog []byte) ([]*os.File, func(), error) {
	specR, specW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	progR, progW, err := os.Pipe()
	if err != nil {
		specR.Close()
		specW.Close()
		return nil, nil, err
	}
	// The filter is a few hundred bytes: it fits in the pipe, and bwrap reads it to the end.
	_, werr := progW.Write(prog)
	progW.Close()
	if werr != nil {
		specR.Close()
		specW.Close()
		progR.Close()
		return nil, nil, werr
	}
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		_, _ = specW.Write(spec)
		_ = specW.Close()
	}()
	var once sync.Once
	closeFiles := func() {
		once.Do(func() {
			_ = specR.Close()
			_ = progR.Close()
			<-wrote
		})
	}
	return []*os.File{specR, nil, progR}, closeFiles, nil
}

// envWithHome returns env with HOME set to home (the sandbox's home, which may differ from the daemon's
// when the daemon has none).
func envWithHome(env []string, home string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "HOME=") {
			out = append(out, kv)
		}
	}
	return append(out, "HOME="+home)
}

// sandboxHome is the home used when the daemon's own is unusable: the run directory's home/ at its own
// path.
func sandboxHome(runDir string) string { return filepath.Join(runDir, "home") }

// sandboxInitExe is the resolved path of this binary, which the sandbox runs as /run/anet/init.
func sandboxInitExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// sandboxRunRoot is <cache>/anet/run, the parent of every run directory.
func sandboxRunRoot() string {
	c, err := os.UserCacheDir()
	if err != nil || c == "" {
		return ""
	}
	return filepath.Join(c, "anet", "run")
}

// newSandboxRunDir creates <cache>/anet/run/<aid8>-<rand>/ with home/, tmp/ and vartmp/ (all 0700).
func newSandboxRunDir(aid string) (string, error) {
	root := sandboxRunRoot()
	if root == "" {
		return "", errors.New("the user cache directory is unknown (set XDG_CACHE_HOME or HOME)")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	short := aidShort(aid)
	if short == "" {
		short = "unknown"
	}
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	dir, err := os.MkdirTemp(root, short+"-")
	if err != nil {
		return "", err
	}
	for _, sub := range []string{"home", "tmp", "vartmp"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			removeSandboxTree(dir)
			return "", err
		}
	}
	return dir, nil
}

// removeSandboxTree removes a directory the sandbox wrote into. An agent can leave directories without
// write permission (a Go module cache, for one), which os.RemoveAll alone cannot empty, so directories
// are made writable first. Symbolic links are removed, never followed: WalkDir does not descend into
// them and they are not chmod'ed.
func removeSandboxTree(dir string) {
	if dir == "" {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- agent runtime ---

// agentRuntime resolves the agent binary and lists what must be visible for it to run: the directory
// holding the command as invoked (so argv[0] stays what the CLI expects), the directory or npm package
// root holding the resolved file, and the same for a "#!" interpreter.
func agentRuntime(bin string) (exe string, dirs []string, err error) {
	exe = bin
	if !filepath.IsAbs(exe) {
		if exe, err = exec.LookPath(bin); err != nil {
			return "", nil, err
		}
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", nil, err
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", nil, err
	}
	dirs = append(dirs, filepath.Dir(exe), runtimeRoot(real))
	if interp := shebangInterpreter(real); interp != "" {
		dirs = append(dirs, filepath.Dir(interp), runtimeRoot(interp))
	}
	return exe, dirs, nil
}

// runtimeRoot is the npm package root for a file inside node_modules (the directory directly below the
// last node_modules, including an @scope), else the file's directory.
func runtimeRoot(file string) string {
	parts := strings.Split(filepath.Clean(file), string(filepath.Separator))
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] != "node_modules" {
			continue
		}
		j := i + 1
		if strings.HasPrefix(parts[j], "@") {
			j++
		}
		if j >= len(parts)-1 {
			break // the file sits directly in node_modules (or its scope); use its directory
		}
		return string(filepath.Separator) + filepath.Join(parts[:j+1]...)
	}
	return filepath.Dir(file)
}

// shebangInterpreter returns the resolved interpreter of a "#!" script, following "/usr/bin/env prog",
// or "" when file is not a script or the interpreter cannot be found.
func shebangInterpreter(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return ""
	}
	interp := fields[0]
	if filepath.Base(interp) == "env" {
		interp = ""
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "-") {
				interp = f
				break
			}
		}
		if interp == "" {
			return ""
		}
		if interp, err = exec.LookPath(interp); err != nil {
			return ""
		}
	}
	real, err := filepath.EvalSymlinks(interp)
	if err != nil {
		return ""
	}
	return real
}
