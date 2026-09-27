// Package scripts holds no Go code. These tests pin the property the joint scripts must have to be
// run on a machine that also runs other checkouts or production daemons (docs/notes/0015 §4): they
// stop what they started, found by path, and nothing else.
package scripts

import (
	"encoding/base64"
	"encoding/hex"
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

// The three scripts that used to stop every anet on the machine by name, and the scripts written after
// them to the same rule for the same test hosts: joint-a2a.sh, joint-official.sh and the mutation runner
// that drives joint.sh.
func TestJointScriptsDoNotStopProcessesByName(t *testing.T) {
	byName := regexp.MustCompile(`\b(pgrep|pkill|killall)\b`)
	for _, f := range []string{"joint.sh", "scenario.sh", "joint-fleet.sh", "joint-a2a.sh", "joint-official.sh", "mutations/mutate.sh"} {
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
	for _, b := range []string{"bash", "python3", "sleep", "sh", "cat"} {
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

	// Someone reading a file under the path (less, an editor, tail): it names the path as argv[1] but runs
	// nothing from there. cat on a FIFO blocks in open() with the FIFO as its argument.
	fifo := filepath.Join(mine, "hub.log")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	catBin, _ := exec.LookPath("cat")

	byExe := start(t, "binary under the path", filepath.Join(mine, "bin", "sleep"), "300")
	deleted := start(t, "binary under the path, deleted since", filepath.Join(mine, "gone", "sleep"), "300")
	byArg := start(t, "script under the path run by an interpreter", shBin, script)
	outside := start(t, "sibling directory", filepath.Join(other, "sleep"), "300")
	system := start(t, "system binary", sleepBin, "300")
	viewer := start(t, "non-interpreter naming a file under the path", catBin, fifo)
	if err := os.Remove(filepath.Join(mine, "gone", "sleep")); err != nil {
		t.Fatal(err)
	}

	listed := libsh(t, base, `pids_under "$1"`, mine)
	for _, p := range []*proc{byExe, deleted, byArg} {
		if !containsLine(listed, p.cmd.Process.Pid) {
			t.Errorf("pids_under does not list the %s (pid %d): %q", p.name, p.cmd.Process.Pid, listed)
		}
	}
	for _, p := range []*proc{outside, system, viewer} {
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
	for _, p := range []*proc{outside, system, viewer} {
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
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "anet"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file is judged by its directory too: /usr/bin/env stands for /usr/local/bin/anet-hub, the
	// production hub a mistyped root would otherwise name.
	out := libsh(t, home, `
for p in / /tmp /usr /opt /root /home /usr/local/bin /usr/bin relative/dir "$HOME" "$HOME/bin" "$HOME/.local/bin" \
         /usr/bin/env "$HOME/bin/anet"; do
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

// A work directory the scripts build binaries into and run them from must be one nobody else can change:
// as root on a shared test host, a /tmp/joint-0 another user made first would let them swap the binaries.
func TestOwnDirRefusesDirectoriesOthersCanChange(t *testing.T) {
	needLinuxShell(t)
	base := t.TempDir()
	// Under umask 002 the test's own directory is group-writable; whether its group has other members
	// depends on the host, so it is made private and the cases below use "others" only.
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, mode os.FileMode) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(base, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	mk("shared", 0o777)                // world-writable, not sticky: anyone may rename what is in it
	mk("tmplike", 0o777|os.ModeSticky) // like /tmp: others cannot touch entries they do not own
	out := libsh(t, base, `
own_dir "$1/fresh/j" || echo "refused a fresh directory"
[ "$(stat -c %a "$1/fresh/j")" = 700 ] || echo "fresh directory is $(stat -c %a "$1/fresh/j"), want 700"
own_dir "$1/fresh/j" || echo "refused it the second time"
own_dir "$1/tmplike/j" || echo "refused a directory under a sticky world-writable one"
own_dir "$1/shared" && echo "accepted a world-writable directory"
own_dir "$1/tmplike" && echo "accepted a sticky world-writable directory itself"
own_dir "$1/shared/j" && echo "accepted a directory under a world-writable, non-sticky one"
own_dir relative/j && echo "accepted a relative path"
true`, base)
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

// canary_hits finds a canary however a file holds it — as it is, in hex, in base64 or base64url at any
// byte offset (a hub stores envelopes and receipts as base64 in JSON; a daemon's evidence chain is base64
// CBOR per line, where a plain grep finds nothing) — and says so when it could not look.
func TestCanaryHitsFindsEncodedCopies(t *testing.T) {
	needLinuxShell(t)
	dir := t.TempDir()
	const canary = "cnry0123456789abcdef0123456789abcdef"
	write := func(name string, b []byte) {
		t.Helper()
		p := filepath.Join(dir, "d", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{}
	for pre := 0; pre < 3; pre++ {
		// The canary at every offset mod 3, inside a record the way a chain line or a JSON field holds it.
		rec := append(append([]byte(strings.Repeat("\x01", pre+5)), canary...), "\x00tail"...)
		name := "b64-" + strconv.Itoa(pre) + ".ael.jsonl"
		write(name, []byte("AAAA\n"+base64.StdEncoding.EncodeToString(rec)+"\n"))
		want[name] = "base64/"
		name = "url-" + strconv.Itoa(pre) + ".json"
		write(name, []byte(`{"x":"`+base64.RawURLEncoding.EncodeToString(append([]byte{0xfb, 0xff}, rec...))+`"}`))
		want[name] = "base64"
	}
	write("plain.log", []byte("2026/09/27 call text="+canary+"\n"))
	want["plain.log"] = "plain"
	write("hex.txt", []byte("bytes "+hex.EncodeToString([]byte("<"+canary+">"))))
	want["hex.txt"] = "hex"
	write("clean.db", []byte("nothing here but cnry0123 and 0123456789abcdef"))
	out := libsh(t, dir, `canary_hits "$1" "$2/d" "$2/gone"`, canary, dir)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := lines[len(lines)-1]; last != "# searched "+strconv.Itoa(len(want)+1) {
		t.Errorf("last line %q, want the count of files read (%d)", last, len(want)+1)
	}
	for name, enc := range want {
		found := false
		for _, l := range lines {
			found = found || (strings.HasPrefix(l, filepath.Join(dir, "d", name)+" (") && strings.Contains(l, enc))
		}
		if !found {
			t.Errorf("%s: no %s hit in\n%s", name, enc, out)
		}
	}
	if strings.Contains(out, "clean.db") {
		t.Errorf("a file without the canary was reported:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(dir, "gone")+" (missing)") {
		t.Errorf("a missing path is not reported:\n%s", out)
	}
	if os.Geteuid() != 0 {
		write("locked", []byte(canary))
		if err := os.Chmod(filepath.Join(dir, "d", "locked"), 0); err != nil {
			t.Fatal(err)
		}
		out := libsh(t, dir, `canary_hits "$1" "$2/d/locked"`, canary, dir)
		if !strings.Contains(out, "(unreadable") {
			t.Errorf("an unreadable file passes for a clean one:\n%s", out)
		}
	}
}

