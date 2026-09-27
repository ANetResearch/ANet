//go:build linux

package daemon

// sandbox_linux.go builds the bubblewrap sandbox that runs a local agent for an untrusted peer
// (A2A-DESIGN §6, review item C7).
//
// The sandbox starts from bubblewrap's empty root and adds only what is listed:
//   - read-only: /usr, the /bin, /sbin and /lib* entries (as symlinks when the host has merged /usr),
//     the /etc files in sandboxEtc, the agent binary's directory and runtime (npm package root, script
//     interpreter), and /run/systemd/resolve when /etc/resolv.conf points into it;
//   - tmpfs: /tmp, /var/tmp, /dev/shm, /run and the home directory, so the agent sees an empty home and
//     none of the host's runtime sockets;
//   - read-write: the interaction's work dir, which is also the working directory;
//   - namespaces: --unshare-ipc --unshare-pid --unshare-uts, plus --new-session and --die-with-parent.
//
// Every bind source is checked against the plan's never-bind list (the resolved data dir, the
// identities container, /tmp/anet-<uid>, $XDG_RUNTIME_DIR, /run/user/<uid>, the ANetLink sockets and
// the agent CLIs' credential directories): a source that equals, contains or lies inside one of them
// makes the whole run unavailable rather than dropping the bind silently.
//
// The network namespace is shared with the host. The agent can therefore reach the internet (it needs
// its model API), and also loopback TCP services and abstract Unix sockets on the host; A2A-DESIGN §21
// item 5 records this. It cannot read the control token, but the control port answers /ping.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// bwrapLookPath locates bubblewrap. Tests replace it to simulate a host without bwrap.
var bwrapLookPath = exec.LookPath

// sandboxEtc are the /etc entries bound read-only: name resolution, user and group names, time zone, CA
// certificates, the dynamic linker cache, and alternatives symlinks some distributions route binaries
// through. Entries missing on the host are skipped (--ro-bind-try).
var sandboxEtc = []string{
	"resolv.conf", "hosts", "host.conf", "nsswitch.conf", "gai.conf", "passwd", "group",
	"localtime", "timezone", "ssl", "ca-certificates", "ca-certificates.conf", "pki", "crypto-policies",
	"alternatives", "ld.so.cache", "ld.so.conf", "ld.so.conf.d", "os-release", "mime.types",
	"protocols", "services",
}

// sandboxSystemDirs are bound read-only (or recreated as symlinks when the host's entry is a symlink,
// as on merged-/usr systems).
var sandboxSystemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32"}

// bwrapProbeTTL is how long a probe result is reused.
const bwrapProbeTTL = 5 * time.Minute

var bwrapProbeCache struct {
	sync.Mutex
	path string
	err  error
	at   time.Time
}

// prepare locates bubblewrap and checks, with a trial run, that it can create the namespaces here. User
// namespaces can be disabled by the kernel, by a container runtime or by an AppArmor policy; the trial
// run is the only reliable test.
func (p *sandboxPlan) prepare() error {
	path, err := bwrapLookPath("bwrap")
	if err != nil {
		return sandboxUnavailable("bubblewrap (bwrap) is not installed or not on PATH")
	}
	if err := probeBwrap(path); err != nil {
		return sandboxUnavailable("bubblewrap cannot create the sandbox on this host: %v", err)
	}
	p.Bwrap = path
	return nil
}

func probeBwrap(path string) error {
	bwrapProbeCache.Lock()
	defer bwrapProbeCache.Unlock()
	if bwrapProbeCache.path == path && time.Since(bwrapProbeCache.at) < bwrapProbeTTL {
		return bwrapProbeCache.err
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		truePath = "/bin/true"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path,
		"--unshare-ipc", "--unshare-pid", "--unshare-uts", "--new-session", "--die-with-parent",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--", truePath).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	bwrapProbeCache.path, bwrapProbeCache.err, bwrapProbeCache.at = path, err, time.Now()
	return err
}

// wrap turns the agent command into a bwrap command line.
func (p *sandboxPlan) wrap(workDir, bin string, args []string) (string, []string, error) {
	if p.Bwrap == "" {
		return "", nil, sandboxUnavailable("sandbox plan was not prepared")
	}
	exe, runtime, err := agentRuntime(bin)
	if err != nil {
		return "", nil, sandboxUnavailable("agent binary %s: %v", bin, err)
	}
	argv, err := buildBwrapArgs(bwrapSpec{
		WorkDir:   workDir,
		Home:      p.Home,
		NeverBind: p.NeverBind,
		RuntimeRO: append(runtime, p.ExtraRO...),
	}, exe, args)
	if err != nil {
		return "", nil, sandboxUnavailable("%v", err)
	}
	return p.Bwrap, argv, nil
}

// bwrapSpec is the input of buildBwrapArgs.
type bwrapSpec struct {
	WorkDir   string   // bound read-write and used as the working directory
	Home      string   // replaced by an empty tmpfs
	NeverBind []string // no bind may equal, contain or lie inside one of these
	RuntimeRO []string // agent runtime directories, bound read-only
}

// buildBwrapArgs returns the bwrap arguments that run exe with args under spec. It fails when a bind
// would expose a never-bind path or when the work dir is unusable.
func buildBwrapArgs(spec bwrapSpec, exe string, args []string) ([]string, error) {
	if spec.WorkDir == "" || !filepath.IsAbs(spec.WorkDir) {
		return nil, fmt.Errorf("sandbox work dir %q is not an absolute path", spec.WorkDir)
	}
	a := []string{"--unshare-ipc", "--unshare-pid", "--unshare-uts", "--new-session", "--die-with-parent"}
	bind := func(flag, src string) error {
		if n, bad := bindConflict(src, spec.NeverBind); bad {
			return fmt.Errorf("refusing to bind %s into the sandbox: it overlaps %s", src, n)
		}
		a = append(a, flag, src, src)
		return nil
	}
	for _, d := range sandboxSystemDirs {
		fi, err := os.Lstat(d)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(d)
			if err != nil {
				continue
			}
			a = append(a, "--symlink", target, d)
			continue
		}
		if fi.IsDir() {
			if err := bind("--ro-bind", d); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range sandboxEtc {
		if err := bind("--ro-bind-try", filepath.Join("/etc", e)); err != nil {
			return nil, err
		}
	}
	a = append(a, "--dev", "/dev", "--proc", "/proc")
	for _, t := range []string{"/tmp", "/var/tmp", "/dev/shm", "/run"} {
		a = append(a, "--tmpfs", t)
	}
	if spec.Home != "" && spec.Home != "/" {
		a = append(a, "--tmpfs", spec.Home)
	}
	if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && pathWithin(real, "/run/systemd/resolve") {
		if err := bind("--ro-bind-try", "/run/systemd/resolve"); err != nil {
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
	if err := bind("--bind", spec.WorkDir); err != nil {
		return nil, err
	}
	a = append(a, "--chdir", spec.WorkDir, "--", exe)
	return append(a, args...), nil
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
