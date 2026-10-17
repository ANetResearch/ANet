//go:build linux || darwin

package autoreply

// sandbox_seatbelt.go is the macOS sandbox (LEC §4.3.8, MS-1 to MS-7): Seatbelt, through
// /usr/bin/sandbox-exec. sandbox_darwin.go wires it in on macOS; it is plain Go apart from that, so its
// launch and its probe are also built and exercised on Linux, with a stand-in sandbox-exec.
//
// A run gets a profile of its own (seatbelt_profile.go), written into its run directory, and is started
// as `/usr/bin/sandbox-exec -f <run>/profile.sb -- <anet> __sandbox-init --spec-fd 3` (MS-1):
// sandbox-exec by its absolute path, never looked up on PATH, and nothing of the agent's on the command
// line. init reads the agent's command, environment and working directory from the spec on fd 3 and its
// conversation from fd 4, as on Linux (sandbox_init.go).
//
// There are no namespaces. The agent sees the host's file system through the profile: the system's
// libraries and tools, its own runtime, the run directory (its HOME and TMPDIR) and the interaction's
// work dir, and nothing of the operator's. It reaches the network only through the run's egress
// endpoint, which listens on a random port of the host's 127.0.0.1 (the only address the profile lets it
// connect to) and takes only requests carrying the run's 256-bit random key (EG-3). The key is in the
// agent's environment alone: not on any command line, not in the profile, not in a log.
//
// The agent stays in init's process group, the one the daemon starts sandbox-exec in (MS-5): a timeout
// kills that group, and init ends what the agent left in it when the agent exits. A process that leaves
// the group outlives the run, still in the sandbox and, once the endpoint is closed, with no network
// (LEC §6.2 item 8). The limits are init's rlimits only (MS-6: "rlimit_only").
//
// Before the first run, and again every five minutes, the same kind of profile runs `__sandbox-probe`,
// which checks five things from inside (MS-4): the real home and a file outside the run cannot be read,
// another port of the host's loopback cannot be reached, the egress endpoint can, /usr/bin/security does
// not start (while /usr/bin/true runs), and the work dir takes a file. A sandbox that fails one is not
// used.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ANetResearch/ANet/internal/pathsafe"
)

// seatbeltSecurityTool is the keychain tool the probe must not be able to start (MS-4 item 4), and
// seatbeltProbeControl a program the profile lets run, which must run first: the item then tells a
// sandbox that refuses the keychain tool from one in which nothing starts at all.
var (
	seatbeltSecurityTool = "/usr/bin/security"
	seatbeltProbeControl = "/usr/bin/true"
)

// Local reasons the macOS sandbox is unavailable (ux §1.4 H, MS-4). A peer only ever sees
// sandbox_unavailable.
const (
	seatbeltCodeNoSandboxExec     = "no_sandbox_exec"
	seatbeltCodeNetnsFailed       = "netns_failed"
	seatbeltCodeFSIsolationFailed = "fs_isolation_failed"
)

const (
	// seatbeltProbeTTL is how long a probe result is reused (MS-4, as SB-2).
	seatbeltProbeTTL = 5 * time.Minute
	// seatbeltProbeTimeout bounds one probe run.
	seatbeltProbeTimeout = 20 * time.Second
	// seatbeltProfileName is the profile's file in the run directory.
	seatbeltProfileName = "profile.sb"
	// egressKeyBytes is the size of a run's random key (EG-3): 256 bits.
	egressKeyBytes = 32
)

// seatbeltProbe is one probe result.
type seatbeltProbe struct {
	Path   string
	Report sandboxCheckReport
	Err    error // nil when the sandbox is usable; else a *sandboxProbeError
	At     time.Time
}

var seatbeltProbeCache struct {
	sync.Mutex
	last *seatbeltProbe
}

// seatbeltPrepare checks that sandbox-exec is at path and that the probe passes with it, and sets the
// plan to use it.
func seatbeltPrepare(p *sandboxPlan, path string) error {
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return &sandboxProbeError{Code: seatbeltCodeNoSandboxExec, Detail: path + " is missing or not executable"}
	}
	pr := probeSeatbelt(path)
	if pr.Err != nil {
		return pr.Err
	}
	p.SandboxExec = path
	return nil
}

// probeSeatbelt returns the probe result for this sandbox-exec, running the probe when there is no result
// younger than seatbeltProbeTTL.
func probeSeatbelt(path string) seatbeltProbe {
	seatbeltProbeCache.Lock()
	defer seatbeltProbeCache.Unlock()
	if c := seatbeltProbeCache.last; c != nil && c.Path == path && time.Since(c.At) < seatbeltProbeTTL {
		return *c
	}
	pr := runSeatbeltProbe(path)
	seatbeltProbeCache.last = &pr
	return pr
}

// runSeatbeltProbe runs `__sandbox-probe` under a profile built like a run's and classifies the outcome:
// items 2 and 3 failing is netns_failed, items 1, 4 and 5 failing (or a sandbox that never started) is
// fs_isolation_failed (MS-4).
func runSeatbeltProbe(path string) seatbeltProbe {
	res := seatbeltProbe{Path: path, At: time.Now()}
	fail := func(code, format string, args ...any) seatbeltProbe {
		res.Err = &sandboxProbeError{Code: code, Detail: fmt.Sprintf(format, args...)}
		return res
	}
	initExe, err := sandboxInitExe()
	if err != nil {
		return fail(seatbeltCodeFSIsolationFailed, "cannot locate the anet binary: %v", err)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		home, err = filepath.EvalSymlinks(home)
	}
	if err != nil || home == "" {
		return fail(seatbeltCodeFSIsolationFailed, "the home directory is unknown: %v", err)
	}
	// Probe files live under the system temp dir only (LEC §5.5), removed before returning. Seatbelt
	// matches real paths, and the temp dir is reached through /var, a link to /private/var.
	tmp, err := os.MkdirTemp("", sandboxProbeName)
	if err != nil {
		return fail(seatbeltCodeFSIsolationFailed, "probe directory: %v", err)
	}
	defer removeSandboxTree(tmp)
	dir, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return fail(seatbeltCodeFSIsolationFailed, "probe directory: %v", err)
	}
	runDir, workDir, outside := filepath.Join(dir, "run"), filepath.Join(dir, "work"), filepath.Join(dir, "outside")
	for _, d := range []string{runDir, filepath.Join(runDir, "home"), filepath.Join(runDir, "tmp"), workDir, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return fail(seatbeltCodeFSIsolationFailed, "probe directory: %v", err)
		}
	}
	outsideFile := filepath.Join(outside, "probe")
	if err := os.WriteFile(outsideFile, []byte("outside the run\n"), 0o600); err != nil {
		return fail(seatbeltCodeFSIsolationFailed, "probe file: %v", err)
	}

	// What must stay out of reach: another port of the host's loopback. What must be reachable: the
	// stand-in for the run's egress endpoint, the one port the profile opens. Connections are counted on
	// this side too, so a probe that misreports is caught: nothing accepts while the probe runs (a connect
	// completes once the kernel has queued it), and what is queued is counted after it exits.
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(seatbeltCodeNetnsFailed, "host listener: %v", err)
	}
	defer other.Close()
	egress, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(seatbeltCodeNetnsFailed, "host listener: %v", err)
	}
	defer egress.Close()

	profile, err := buildSeatbeltProfile(seatbeltSpec{Init: initExe, RunDir: runDir, WorkDir: workDir,
		Port: egress.Addr().(*net.TCPAddr).Port})
	if err != nil {
		return fail(seatbeltCodeFSIsolationFailed, "%v", err)
	}
	profilePath := filepath.Join(runDir, seatbeltProfileName)
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		return fail(seatbeltCodeFSIsolationFailed, "profile: %v", err)
	}
	spec, _ := json.Marshal(sandboxCheckSpec{V: sandboxSpecVersion, TCP: other.Addr().String(),
		Egress: egress.Addr().String(), Home: home, Outside: outsideFile, Work: workDir})
	files, closeFiles, err := sandboxFds(spec, nil, nil)
	if err != nil {
		return fail(seatbeltCodeFSIsolationFailed, "%v", err)
	}
	defer closeFiles()

	ctx, cancel := context.WithTimeout(context.Background(), seatbeltProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, seatbeltArgs(profilePath, initExe, "__sandbox-probe")...)
	cmd.Env = []string{}
	cmd.Dir = workDir
	cmd.ExtraFiles = files
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	var rep sandboxCheckReport
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &rep); jerr != nil {
		// The probe never reported: sandbox-exec could not apply the profile, or init died in it.
		if ctx.Err() != nil {
			return fail(seatbeltCodeFSIsolationFailed, "the self-check did not finish in %s", seatbeltProbeTimeout)
		}
		return fail(seatbeltCodeFSIsolationFailed, "%s", firstLine(strings.TrimSpace(stderr.String()), runErr))
	}
	res.Report = rep
	otherReached, egressReached := seatbeltQueued(other), seatbeltQueued(egress)
	switch {
	case !rep.HostTCPRefused || otherReached > 0 || !rep.EgressReached || egressReached == 0:
		return fail(seatbeltCodeNetnsFailed, "network isolation self-check failed: %s",
			seatbeltSummary(rep, otherReached, egressReached))
	case !rep.HomeUnreadable || !rep.SecurityRefused || !rep.WorkWritable:
		return fail(seatbeltCodeFSIsolationFailed, "file and process isolation self-check failed: %s",
			seatbeltSummary(rep, otherReached, egressReached))
	}
	return res
}

// seatbeltQueued takes and counts the connections waiting on one of the probe's host listeners once the
// probe has exited: every connection it made is in the listener's queue by then, so a short deadline
// takes them all, and a probe that says it could not connect where it did, or could where it did not, is
// caught whatever the scheduling.
func seatbeltQueued(ln net.Listener) int32 {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return 0
	}
	_ = tl.SetDeadline(time.Now().Add(100 * time.Millisecond))
	var n int32
	for {
		c, err := tl.Accept()
		if err != nil {
			return n
		}
		n++
		_ = c.Close()
	}
}

// seatbeltArgs are sandbox-exec's arguments: the profile, then anet's own init (or the probe) and
// nothing of the agent's (MS-1, AV-1).
func seatbeltArgs(profile, initExe, sub string) []string {
	return []string{"-f", profile, "--", initExe, sub, "--spec-fd", strconv.Itoa(sandboxSpecFd)}
}

// seatbeltSummary lists the MS-4 items that failed, for the operator's detail line.
func seatbeltSummary(r sandboxCheckReport, otherReached, egressReached int32) string {
	var bad []string
	add := func(ok bool, name, key string) {
		if !ok {
			if d := r.Detail[key]; d != "" {
				name += " (" + d + ")"
			}
			bad = append(bad, name)
		}
	}
	add(r.HomeUnreadable, "1 home", "home")
	add(r.HostTCPRefused && otherReached == 0, "2 host_tcp", "host_tcp")
	add(r.EgressReached && egressReached > 0, "3 egress", "egress")
	add(r.SecurityRefused, "4 security", "security")
	add(r.WorkWritable, "5 work", "work")
	return strings.Join(bad, "; ")
}

// seatbeltSpecUsable: the macOS probe needs both host ports, the real home, the outside file and the work
// dir.
func seatbeltSpecUsable(s sandboxCheckSpec) bool {
	return s.TCP != "" && s.Egress != "" && s.Home != "" && s.Outside != "" && s.Work != ""
}

// seatbeltChecks runs the five checks of MS-4 inside the sandbox.
func seatbeltChecks(spec sandboxCheckSpec) sandboxCheckReport {
	r := sandboxCheckReport{Detail: map[string]string{}}
	// 1. Neither the real home (its listing) nor a file outside the run can be read.
	var read []string
	if _, err := os.ReadDir(spec.Home); err == nil {
		read = append(read, "listed the real home")
	}
	if _, err := os.ReadFile(spec.Outside); err == nil {
		read = append(read, "read a file outside the run")
	}
	r.HomeUnreadable = len(read) == 0
	if !r.HomeUnreadable {
		r.Detail["home"] = strings.Join(read, ", ")
	}
	// 2. Another port of the host's loopback.
	if c, err := net.DialTimeout("tcp", spec.TCP, 2*time.Second); err != nil {
		r.HostTCPRefused = true
	} else {
		_ = c.Close()
		r.Detail["host_tcp"] = "connected"
	}
	// 3. The egress endpoint.
	if c, err := net.DialTimeout("tcp", spec.Egress, 5*time.Second); err != nil {
		r.Detail["egress"] = err.Error()
	} else {
		_ = c.Close()
		r.EgressReached = true
	}
	// 4. The keychain tool does not start.
	if ok, why := seatbeltSecurityRefused(); ok {
		r.SecurityRefused = true
	} else {
		r.Detail["security"] = why
	}
	// 5. The work dir takes a file.
	f := filepath.Join(spec.Work, sandboxProbeName+randHex(4))
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		r.Detail["work"] = err.Error()
	} else {
		_ = os.Remove(f)
		r.WorkWritable = true
	}
	return r
}

// seatbeltSecurityRefused is MS-4 item 4 inside the sandbox: a program the profile allows runs to its end,
// and then the keychain tool's execution is refused, whatever it would have done. Without the first, a
// sandbox in which no program can start at all would pass. The string says why the item does not hold.
func seatbeltSecurityRefused() (bool, string) {
	ctl := exec.Command(seatbeltProbeControl)
	ctl.Env = []string{}
	if err := ctl.Run(); err != nil {
		return false, "no program runs in the sandbox (" + seatbeltProbeControl + ": " + err.Error() + ")"
	}
	cmd := exec.Command(seatbeltSecurityTool, "-h")
	cmd.Env = []string{}
	if err := cmd.Start(); err != nil {
		return true, ""
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return false, "started"
}

// --- one sandboxed process ---

// seatbeltCommand prepares one agent process for the sandbox: argv and env are the agent's command and
// complete environment, workDir its working directory, stdin what it reads on its standard input (nil:
// nothing). The returned launch runs sandbox-exec with the run's profile, the spec on fd 3 and stdin on
// fd 4; Close closes the run's egress endpoint, removes the run directory and releases the descriptors
// after the process ended.
func seatbeltCommand(p *sandboxPlan, workDir string, argv, env []string, stdin []byte) (*sandboxLaunch, error) {
	if p.SandboxExec == "" {
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
	runDir, err := newSandboxRunDir(p.AID)
	if err != nil {
		return nil, sandboxUnavailable("run directory: %v", err)
	}
	p.addRunDir(runDir)
	ok := false
	defer func() {
		if !ok {
			p.dropRunDir(runDir)
			removeSandboxTree(runDir)
		}
	}()
	// Seatbelt matches real paths: every path the profile names is resolved.
	realRun, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		return nil, sandboxUnavailable("run directory: %v", err)
	}
	realWork, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, sandboxUnavailable("work dir: %v", err)
	}
	var realRuntime []string
	for _, d := range runtimeDirs {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			realRuntime = append(realRuntime, r)
		}
	}

	// The run's egress endpoint (LEC §2.6.1, EG-1, EG-3): a random port of the host's loopback, keyed
	// with a fresh 256-bit key, ready before the agent starts and closed as soon as the run ends.
	var ep *egressEndpoint
	addr, key, port := "", "", 0
	if p.Egress != nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, sandboxUnavailable("egress endpoint: %v", err)
		}
		addr, key, port = ln.Addr().String(), randHex(egressKeyBytes), ln.Addr().(*net.TCPAddr).Port
		ep = newEgressEndpoint(p.Egress)
		ep.keyed(addr, key)
		ep.Serve(ln)
		defer func() {
			if !ok {
				ep.Close()
			}
		}()
	}
	profile, err := buildSeatbeltProfile(seatbeltSpec{
		Init: initExe, RunDir: realRun, WorkDir: realWork,
		Runtime: realRuntime, ReadOnly: append(seatbeltPackageTrees(realRuntime), p.ExtraRO...),
		Port: port, NeverAllow: p.NeverBind,
	})
	if err != nil {
		return nil, sandboxUnavailable("%v", err)
	}
	profilePath := filepath.Join(realRun, seatbeltProfileName)
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		return nil, sandboxUnavailable("profile: %v", err)
	}
	// MS-3: the run directory is the agent's HOME and TMPDIR; TLS roots come from the system bundle; its
	// model service is the keyed injection route and every proxy variable names the keyed endpoint; its
	// search path names only what the profile lets it run.
	// The agent's own configuration for the injection route, in the run's home (LEC AV-4). The home is
	// at the same path on the host and in the sandbox. The route it names is the keyed one, as in the
	// environment (EG-3): an agent that reads its model service's address only from that file reaches the
	// endpoint only with the key.
	var agentEnv []string
	if p.AgentHome != nil {
		home := filepath.Join(realRun, "home")
		if agentEnv, err = p.AgentHome(home, home, egressRouteBaseAt(p.Egress, addr, key)); err != nil {
			return nil, sandboxUnavailable("agent configuration: %v", err)
		}
	}
	env = sandboxEnvOverlay(env, append(sandboxEgressEnvAt(p.Egress, addr, key), agentEnv...))
	env = envWithSeatbeltPath(env, runtimeDirs)
	env = envWithHome(env, filepath.Join(realRun, "home"))
	env = envWithValue(env, "TMPDIR", filepath.Join(realRun, "tmp"))
	// The claude CLI makes its scratch directory under /tmp unless told otherwise, which the profile keeps
	// closed (there is no private /tmp as in the Linux sandbox): it is pointed at the run's own tmp/.
	env = envWithValue(env, "CLAUDE_CODE_TMPDIR", filepath.Join(realRun, "tmp"))
	env = envWithValue(env, "SSL_CERT_FILE", seatbeltSSLCertFile)
	spec, err := json.Marshal(newSandboxInitSpec(append([]string{exe}, argv[1:]...), env, workDir, stdin != nil, ""))
	if err != nil {
		return nil, err
	}
	if len(spec) > sandboxSpecMax {
		return nil, fmt.Errorf("the agent's command and environment are %d bytes, over the sandbox's %d", len(spec), sandboxSpecMax)
	}
	files, closeFiles, err := sandboxFds(spec, nil, stdin)
	if err != nil {
		return nil, sandboxUnavailable("%v", err)
	}
	ok = true
	return &sandboxLaunch{
		Path: p.SandboxExec, Args: seatbeltArgs(profilePath, initExe, "__sandbox-init"),
		Env:   []string{}, // the agent's environment, with the run's key, is in the spec
		Files: files,
		Close: func() {
			if ep != nil {
				ep.Close()
				p.addEgress(ep.Summary())
			}
			closeFiles()
			removeSandboxTree(runDir)
			p.dropRunDir(runDir)
		},
	}, nil
}

// envWithSeatbeltPath returns env with PATH narrowed to the directories the agent may run programs from
// (MS-3, as SB-4): the system's /bin and /usr/bin and the agent's runtime directories.
func envWithSeatbeltPath(env, runtimeDirs []string) []string {
	shown := func(d string) bool {
		for _, s := range append(append([]string{}, seatbeltExecSystem...), runtimeDirs...) {
			if s != "" && pathsafe.PathWithin(d, s) {
				return true
			}
		}
		return false
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		v, ok := strings.CutPrefix(kv, "PATH=")
		if !ok {
			out = append(out, kv)
			continue
		}
		var keep []string
		for _, d := range filepath.SplitList(v) {
			if filepath.IsAbs(d) && shown(d) {
				keep = append(keep, d)
			}
		}
		out = append(out, "PATH="+strings.Join(keep, string(filepath.ListSeparator)))
	}
	return out
}

// envWithValue returns env with k set to v, replacing any earlier value.
func envWithValue(env []string, k, v string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, k+"=") {
			out = append(out, kv)
		}
	}
	return append(out, k+"="+v)
}
