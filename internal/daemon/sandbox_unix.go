//go:build linux || darwin

package daemon

// sandbox_unix.go holds what the two sandboxes share: the Linux one (bubblewrap, sandbox_linux.go) and
// the macOS one (Seatbelt, sandbox_darwin.go). Both run anet's own init in the sandbox with the agent's
// command, environment and working directory in a spec on fd 3 and its conversation on fd 4 (LEC
// §2.6.5); both give each run a directory of its own under <cache>/anet/run, removed when the run ends
// and swept at start-up (SB-9); both find the agent's runtime the same way.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// sandboxProbeName starts the names of the probe's temp dir and abstract socket. They are local names
// that never go on the wire, so they are built on "anet-", the wire registry's exempt prefix for local
// file and id names, instead of being registered as wire strings.
const sandboxProbeName = "anet-" + "probe-"

// firstLine is the first line of a tool's error output (or err when there is none), for the operator's
// detail.
func firstLine(s string, err error) string {
	if s == "" && err != nil {
		return err.Error()
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 300)
}

// sandboxFds returns the descriptors a sandboxed process starts with, as exec.Cmd.ExtraFiles: fd 3 a pipe
// carrying spec, fd 4 a pipe carrying stdin (closed when stdin is nil), and on Linux fd 5 a pipe carrying
// the seccomp program (prog nil: no fd 5, as on macOS). The spec and stdin are written from goroutines, since either can be larger than a pipe holds
// (the conversation is up to 256 KiB, LEC AV-9); closeFiles closes the read ends and waits for those
// writes to end — an agent that never read its stdin leaves the writer to fail on the closed pipe.
func sandboxFds(spec, prog, stdin []byte) ([]*os.File, func(), error) {
	var opened []*os.File
	closeAll := func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			opened = append(opened, r, w)
		}
		return r, w, err
	}
	specR, specW, err := pipe()
	if err != nil {
		return nil, nil, err
	}
	var progR, progW *os.File
	if prog != nil {
		if progR, progW, err = pipe(); err != nil {
			closeAll()
			return nil, nil, err
		}
	}
	var inR, inW *os.File
	if stdin != nil {
		if inR, inW, err = pipe(); err != nil {
			closeAll()
			return nil, nil, err
		}
	}
	// The filter is a few hundred bytes: it fits in the pipe, and bwrap reads it to the end.
	if progW != nil {
		_, werr := progW.Write(prog)
		progW.Close()
		if werr != nil {
			closeAll()
			return nil, nil, werr
		}
	}
	var writers sync.WaitGroup
	feed := func(w *os.File, b []byte) {
		writers.Add(1)
		go func() {
			defer writers.Done()
			_, _ = w.Write(b)
			_ = w.Close()
		}()
	}
	feed(specW, spec)
	if inW != nil {
		feed(inW, stdin)
	}
	var once sync.Once
	closeFiles := func() {
		once.Do(func() {
			_ = specR.Close()
			if progR != nil {
				_ = progR.Close()
			}
			if inR != nil {
				_ = inR.Close()
			}
			writers.Wait()
		})
	}
	if progR == nil {
		return []*os.File{specR, inR}, closeFiles, nil
	}
	return []*os.File{specR, inR, progR}, closeFiles, nil
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

// sandboxInitExe is the resolved path of this binary, which the sandbox runs as init (bound at
// /run/anet/init on Linux, run from where it is on macOS).
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

// sandboxSockDirPrefix starts the name of a run's socket directory under the temp dir, when the run
// directory's path is too long for the socket (a local name, built on the registry's exempt "anet-").
const sandboxSockDirPrefix = "anet-" + "sock-"

// runDirPrefix names this identity's run directories: <aid8> of <cache>/anet/run/<aid8>-<rand>/.
func runDirPrefix(aid string) string {
	short := aidShort(aid)
	if short == "" {
		short = "unknown"
	}
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	return short
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
	dir, err := os.MkdirTemp(root, runDirPrefix(aid)+"-")
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
func removeSandboxTree(dir string) { removeWorkTree(dir) }

// leftoverOwner is the uid whose directories the start-up sweep removes: this process's. A variable so
// tests can stand in for another user.
var leftoverOwner = os.Getuid

// removeSandboxLeftovers removes the run directories, and the socket directories under the temp dir, of
// this identity's runs: what a run the daemon was killed in the middle of left (LEC SB-9). It runs
// before this process's first sandboxed run, when no run of this identity is under way. The temp dir is
// shared with the host's other users and the run prefix is public (the AID's tail): a directory there
// that this user does not own is not a leftover of ours and is left alone.
func removeSandboxLeftovers(aid string) {
	prefix := runDirPrefix(aid) + "-"
	sweep := func(dir, namePrefix string) {
		if dir == "" {
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), namePrefix) {
				continue
			}
			p := filepath.Join(dir, e.Name())
			fi, err := os.Lstat(p)
			if err != nil {
				continue
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != leftoverOwner() {
				continue
			}
			removeSandboxTree(p)
		}
	}
	sweep(sandboxRunRoot(), prefix)
	sweep(os.TempDir(), sandboxSockDirPrefix+prefix)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- agent runtime ---

// agentRuntime resolves the agent binary and lists what must be visible for it to run: the directory
// holding the command as invoked (so argv[0] stays what the CLI expects), the directory or npm package
// root holding the resolved file, the same for a "#!" interpreter, and, for a Python interpreter, the
// installation or virtual environment it runs from (pythonRuntime).
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
	if interp, named := shebangInterpreter(real); interp != "" {
		dirs = append(dirs, filepath.Dir(interp), runtimeRoot(interp))
		dirs = append(dirs, pythonRuntime(named, interp)...)
	}
	return exe, dirs, nil
}

// pythonRuntime lists what a Python interpreter needs besides its binary: a "#!" line naming a virtual
// environment's bin/python (a pip-installed CLI such as hermes, or one installed with pipx or uv) runs
// with that environment's packages, under <venv>/lib, and its pyvenv.cfg, so the whole environment
// <venv> is listed; an interpreter of its own installation outside the system directories (a uv- or
// pyenv-managed Python) needs its standard library, so that installation's prefix is. A directory is
// listed only when it is recognisably one of the two (it holds pyvenv.cfg, or lib/python3.*), never the
// mere parent of some bin directory: a "#!" naming ~/bin/python must not show the whole home directory.
// named is the interpreter as the "#!" line names it, real the file it resolves to. Every link on the way
// from one to the other is followed too, and its installation listed by the path the link names: uv links
// a venv's python to <pythons>/cpython-3.12-<platform>/bin/python3.12, where cpython-3.12-<platform> is
// itself a link to the installed patch release, and inside the sandbox that path has to exist as written.
func pythonRuntime(named, real string) []string {
	if !strings.HasPrefix(filepath.Base(real), "python") {
		return nil
	}
	var out []string
	hops := []string{named}
	for p, i := named, 0; i < 8; i++ { // a chain longer than 8 links is not an installation we know
		t, err := os.Readlink(p)
		if err != nil {
			break
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(filepath.Dir(p), t)
		}
		hops = append(hops, t)
		p = t
	}
	for _, p := range append(hops, real) {
		bin := filepath.Dir(p)
		if filepath.Base(bin) != "bin" {
			continue
		}
		prefix := filepath.Dir(bin)
		if prefix == "/" || underSystemDir(prefix) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(prefix, "pyvenv.cfg")); err == nil && fi.Mode().IsRegular() {
			out = append(out, prefix)
			continue
		}
		if libs, _ := filepath.Glob(filepath.Join(prefix, "lib", "python3*")); len(libs) > 0 {
			out = append(out, prefix)
		}
	}
	return out
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
// and the interpreter as the line names it (found on PATH for env), or "" when file is not a script or
// the interpreter cannot be found.
func shebangInterpreter(file string) (real, named string) {
	f, err := os.Open(file)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return "", ""
	}
	if !strings.HasPrefix(line, "#!") {
		return "", ""
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 {
		return "", ""
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
			return "", ""
		}
		if interp, err = exec.LookPath(interp); err != nil {
			return "", ""
		}
	}
	real, err = filepath.EvalSymlinks(interp)
	if err != nil {
		return "", ""
	}
	return real, interp
}
