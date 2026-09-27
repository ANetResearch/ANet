// Package scripts holds no Go code. These tests pin the property the joint scripts must have to be
// run on a machine that also runs other checkouts or production daemons (docs/notes/0015 §4): they
// stop what they started, found by path, and nothing else.
package scripts

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The three scripts that used to stop every anet on the machine by name.
func TestJointScriptsDoNotStopProcessesByName(t *testing.T) {
	byName := regexp.MustCompile(`\b(pgrep|pkill|killall)\b`)
	for _, f := range []string{"joint.sh", "scenario.sh", "joint-fleet.sh"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if byName.MatchString(line) {
				t.Errorf("%s:%d stops processes by name: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func needLinuxShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the helpers read /proc")
	}
	for _, b := range []string{"bash", "python3", "sleep", "sh"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not on PATH", b)
		}
	}
}

// libsh runs a bash snippet with lib.sh sourced. ANet is preset so sourcing looks nothing up.
func libsh(t *testing.T, home, snippet string, args ...string) string {
	t.Helper()
	lib, err := filepath.Abs("lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"-c", ". " + lib + "\n" + snippet, "libsh"}, args...)...)
	cmd.Env = append(os.Environ(), "ANET=/nonexistent/anet", "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return string(out)
}

func copyExe(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// proc is a process the test started in its own session, the way the scripts start theirs.
type proc struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
}

func start(t *testing.T, name string, argv ...string) *proc {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{name: name, cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-p.done })
	// Until the exec completes the child is a copy of this test binary; wait for its argv.
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cmdline")
		if strings.HasPrefix(string(b), argv[0]+"\x00") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: exec did not complete", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return p
}

func (p *proc) exited(within time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(within):
		return false
	}
}

func TestStopUnderStopsOnlyWhatRunsFromThatPath(t *testing.T) {
	needLinuxShell(t)
	sleepBin, _ := exec.LookPath("sleep")
	shBin, _ := exec.LookPath("sh")
	base := t.TempDir()
	mine := filepath.Join(base, "run")
	// A sibling whose name has mine as a prefix: "under /x/run" must not mean "starts with /x/run".
	other := filepath.Join(base, "run2")

	copyExe(t, sleepBin, filepath.Join(mine, "bin", "sleep"))
	copyExe(t, sleepBin, filepath.Join(mine, "gone", "sleep"))
	copyExe(t, sleepBin, filepath.Join(other, "sleep"))
	script := filepath.Join(mine, "svc.sh")
	if err := os.WriteFile(script, []byte("while :; do sleep 1; done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	byExe := start(t, "binary under the path", filepath.Join(mine, "bin", "sleep"), "300")
	deleted := start(t, "binary under the path, deleted since", filepath.Join(mine, "gone", "sleep"), "300")
	byArg := start(t, "script under the path run by an interpreter", shBin, script)
	outside := start(t, "sibling directory", filepath.Join(other, "sleep"), "300")
	system := start(t, "system binary", sleepBin, "300")
	if err := os.Remove(filepath.Join(mine, "gone", "sleep")); err != nil {
		t.Fatal(err)
	}

	listed := libsh(t, base, `pids_under "$1"`, mine)
	for _, p := range []*proc{byExe, deleted, byArg} {
		if !containsLine(listed, p.cmd.Process.Pid) {
			t.Errorf("pids_under does not list the %s (pid %d): %q", p.name, p.cmd.Process.Pid, listed)
		}
	}
	for _, p := range []*proc{outside, system} {
		if containsLine(listed, p.cmd.Process.Pid) {
			t.Errorf("pids_under lists the %s (pid %d)", p.name, p.cmd.Process.Pid)
		}
	}

	libsh(t, base, `stop_under "$1" 5`, mine)
	for _, p := range []*proc{byExe, deleted, byArg} {
		if !p.exited(5 * time.Second) {
			t.Errorf("stop_under left the %s running", p.name)
		}
	}
	for _, p := range []*proc{outside, system} {
		if p.exited(0) {
			t.Errorf("stop_under stopped the %s", p.name)
		}
	}

	// A single file: exactly that executable.
	again := start(t, "binary named exactly", filepath.Join(mine, "bin", "sleep"), "300")
	libsh(t, base, `stop_under "$1" 5`, filepath.Join(other, "sleep"))
	if !outside.exited(5 * time.Second) {
		t.Error("stop_under PATH-to-a-file left that executable running")
	}
	if again.exited(0) {
		t.Error("stop_under PATH-to-a-file stopped a different executable")
	}
}

// A mistyped J or ROOT must not turn into "stop everything I run".
func TestOwnPathRefusesPathsNoScriptOwns(t *testing.T) {
	needLinuxShell(t)
	home := t.TempDir()
	out := libsh(t, home, `
for p in / /tmp /usr /opt /root /home /usr/local/bin /usr/bin relative/dir "$HOME" "$HOME/bin" "$HOME/.local/bin"; do
  _own_path "$p" >/dev/null && echo "claimed $p"
done
_own_path "$HOME/joint" >/dev/null || echo "refused $HOME/joint"
_own_path /tmp/joint-x >/dev/null || echo "refused /tmp/joint-x"
[ -z "$(pids_under /)" ] || echo "pids_under / listed something"
true`)
	if strings.TrimSpace(out) != "" {
		t.Errorf("unexpected:\n%s", out)
	}
}

func containsLine(s string, pid int) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == strconv.Itoa(pid) {
			return true
		}
	}
	return false
}
