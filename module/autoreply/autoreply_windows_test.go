//go:build windows

package autoreply

// autoreply_windows_test.go: what the module does on Windows, where there is no sandbox. Every test is
// self-contained (t.TempDir(), no network, no repository files) so that it runs as a cross-compiled test
// binary (`autoreply.test.exe -test.run Windows -test.v`) and on a Windows CI runner.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/ANetResearch/ANet/module"
)

// winStub makes the agent binary a batch file that counts its runs, and returns the count. The tests below
// expect it never to run.
func winStub(t *testing.T) (runs func() int) {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "runs")
	stub := filepath.Join(dir, "stub.cmd")
	script := "@echo run>>\"" + count + "\"\r\n@echo EXEC-OK\r\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	setExecCommand(t, stub)
	return func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "run")
	}
}

// systemTool is a program of the Windows system directory.
func systemTool(t *testing.T, rel string) string {
	t.Helper()
	sys, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(sys, rel)
}

// mkJunction creates the directory junction link pointing at target (mklink /J needs no privilege).
func mkJunction(t *testing.T, link, target string) {
	t.Helper()
	cmdExe := systemTool(t, `System32\cmd.exe`)
	c := exec.Command(cmdExe)
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + cmdExe + `" /d /c mklink /J "` + link + `" "` + target + `"`}
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
}

// mkSymlink creates a file symbolic link, or skips the test when this account lacks the privilege.
func mkSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skip("creating a symbolic link needs SeCreateSymbolicLinkPrivilege (an elevated account or Developer Mode), which this account lacks")
		}
		t.Fatal(err)
	}
}

// An untrusted peer's turn with untrusted=sandbox is refused, as sandbox_unavailable with the local reason
// platform_unsupported, and the agent never starts: there is no sandbox on Windows and no fallback to an
// unconfined run (LEC OP-1, GT-2).
func TestWindowsUntrustedSandboxTurnIsRefused(t *testing.T) {
	if sandboxPrepareForTest != nil || sandboxRunnerForTest != nil {
		t.Fatal("a sandbox seam is set; this test needs the platform's own check")
	}
	if code := noSandbox(t); code != sandboxCodePlatformUnsupported {
		t.Fatalf("noSandbox = %q", code)
	}
	runs := winStub(t)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Untrusted: UntrustedSandbox}, nil)
	ix := fx.deliver(fxPeerA, false, "task")
	fx.tick()
	if n := runs(); n != 0 {
		t.Fatalf("the agent ran %d times", n)
	}
	if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable || len(x.sent) != 0 {
		t.Fatalf("the requester was told %+v", x)
	}
	recs := fx.invoked()
	if len(recs) != 1 || recs[0]["exit"] != exitSandboxUnavailable || recs[0]["unavailable"] != sandboxCodePlatformUnsupported ||
		recs[0]["sandboxed"] != false {
		t.Fatalf("records: %v", recs)
	}
}

// trusted=sandbox is refused the same way: a trusted peer's turn decided for the sandbox never runs
// directly.
func TestWindowsTrustedSandboxTurnIsRefused(t *testing.T) {
	runs := winStub(t)
	fx := newTurnFx(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k", Trusted: TrustedSandbox}, nil)
	ix := fx.deliver(fxPeerA, true, "task")
	fx.tick()
	if n := runs(); n != 0 {
		t.Fatalf("the agent ran %d times: a sandboxed turn fell back to a direct run", n)
	}
	if x := fx.ix(ix); x.state != "rejected" || x.reason != module.ReasonSandboxUnavailable || len(x.sent) != 0 {
		t.Fatalf("the requester was told %+v", x)
	}
	recs := fx.invoked()
	if len(recs) != 1 || recs[0]["exit"] != exitSandboxUnavailable || recs[0]["unavailable"] != sandboxCodePlatformUnsupported ||
		recs[0]["mode"] != modeSandbox {
		t.Fatalf("records: %v", recs)
	}
}

// The platform check itself: a sandbox plan cannot be made, and the error is errSandboxUnavailable with
// the code platform_unsupported.
func TestWindowsSandboxPlanIsPlatformUnsupported(t *testing.T) {
	n := newBareDaemon(t)
	_, err := n.newSandboxPlan()
	var pe *sandboxProbeError
	if !errors.Is(err, errSandboxUnavailable) || !errors.As(err, &pe) || pe.Code != sandboxCodePlatformUnsupported {
		t.Fatalf("newSandboxPlan: %v", err)
	}
}

// An outbox entry that is a junction is refused, and so is a junction in the outbox's own place: neither
// may carry the operator's files to the peer.
func TestWindowsCollectOutboxRefusesAJunction(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "key.txt"), []byte("operator secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	outbox := filepath.Join(work, "outbox")
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outbox, "a.txt"), []byte("reply file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if atts, err := collectOutbox(outbox); err != nil || len(atts) != 1 || string(atts[0].Data) != "reply file" {
		t.Fatalf("a plain outbox: %v %v", atts, err)
	}

	mkJunction(t, filepath.Join(outbox, "link"), secret)
	if atts, err := collectOutbox(outbox); err == nil {
		t.Fatalf("an outbox entry that is a junction was collected: %d attachments", len(atts))
	}

	planted := filepath.Join(work, "outbox2")
	mkJunction(t, planted, secret)
	if atts, err := collectOutbox(planted); err == nil {
		for _, a := range atts {
			if strings.Contains(string(a.Data), "operator secret") {
				t.Fatal("a junction in the outbox's place delivered the operator's file")
			}
		}
		t.Fatalf("a junction in the outbox's place was read: %d attachments", len(atts))
	}
}

// readWorkFile (codex's reply file) refuses a junction in the file's place.
func TestWindowsReadWorkFileRefusesAJunction(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	if err := os.Mkdir(secret, 0o700); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	mkJunction(t, filepath.Join(work, "reply.txt"), secret)
	if _, err := readWorkFile(work, "reply.txt", 1<<20); err == nil {
		t.Fatal("a junction in the reply file's place was read")
	}
}

// writeConversationFile refuses a junction planted where conversation.md goes.
func TestWindowsConversationFileRefusesAPlantedJunction(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(root, "in")
	if err := os.Mkdir(in, 0o700); err != nil {
		t.Fatal(err)
	}
	mkJunction(t, filepath.Join(in, conversationFileName), target)
	if _, _, err := writeConversationFile(execInvokeOpts{InputDir: in, Conversation: "peer text"}); err == nil {
		t.Fatal("conversation.md was written through a planted junction")
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Fatalf("the junction's target was written: %v", ents)
	}
}

// writeConversationFile refuses a file symbolic link planted where conversation.md goes, and leaves the
// link's target as it was.
func TestWindowsConversationFileRefusesAPlantedSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "operator.txt")
	if err := os.WriteFile(target, []byte("operator data"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(root, "in")
	if err := os.Mkdir(in, 0o700); err != nil {
		t.Fatal(err)
	}
	mkSymlink(t, target, filepath.Join(in, conversationFileName))
	if _, _, err := writeConversationFile(execInvokeOpts{InputDir: in, Conversation: "peer text"}); err == nil {
		t.Fatal("conversation.md was written through a planted symbolic link")
	}
	if b, _ := os.ReadFile(target); string(b) != "operator data" {
		t.Fatalf("the link's target was changed: %q", b)
	}
}

// A conversation file written into a fresh input directory holds the conversation.
func TestWindowsConversationFileIsWritten(t *testing.T) {
	p, cleanup, err := writeConversationFile(execInvokeOpts{WorkDir: t.TempDir(), Conversation: "peer text"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if b, err := os.ReadFile(p); err != nil || string(b) != "peer text" {
		t.Fatalf("conversation.md: %q %v", b, err)
	}
}

// The allowlist compares names without regard to case, as Windows does, keeps a variable spelled as the
// daemon's environment spells it ("Path"), and admits no ANET_* variable in any spelling.
func TestWindowsExecEnvAllowlist(t *testing.T) {
	environ := []string{
		`Path=C:\Windows\System32;C:\Windows`, `SystemRoot=C:\Windows`, `windir=C:\Windows`,
		`ComSpec=C:\Windows\System32\cmd.exe`, `PATHEXT=.COM;.EXE;.BAT;.CMD`, `TEMP=C:\Users\u\AppData\Local\Temp`,
		`ProgramFiles(x86)=C:\Program Files (x86)`, `USERPROFILE=C:\Users\u`, `LocalAppData=C:\Users\u\AppData\Local`,
		`anthropic_api_key=sk-ant`, `anet_data_dir=C:\Users\u\.anet`, `ANET_DATA_DIR=C:\Users\u\.anet`,
		`Anet_Control_Token=t`, `AWS_SECRET_ACCESS_KEY=s3`, `=C:=C:\Users\u`,
	}
	env := execEnvFrom(environ, AutoReplyConfig{Agent: "claude"}, nil, `C:\w\outbox`)
	has := func(kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}
	for _, kv := range []string{
		`Path=C:\Windows\System32;C:\Windows`, `SystemRoot=C:\Windows`, `windir=C:\Windows`,
		`ComSpec=C:\Windows\System32\cmd.exe`, `PATHEXT=.COM;.EXE;.BAT;.CMD`, `TEMP=C:\Users\u\AppData\Local\Temp`,
		`ProgramFiles(x86)=C:\Program Files (x86)`, `USERPROFILE=C:\Users\u`, `LocalAppData=C:\Users\u\AppData\Local`,
		`anthropic_api_key=sk-ant`, `ANET_OUTBOX=C:\w\outbox`,
	} {
		if !has(kv) {
			t.Errorf("the environment lacks %s: %q", kv, env)
		}
	}
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if strings.EqualFold(k, "ANET_OUTBOX") {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(k), "ANET_") || strings.EqualFold(k, "AWS_SECRET_ACCESS_KEY") || k == "" {
			t.Errorf("the environment carries %q", e)
		}
	}
}

// Agent binary names are compared without regard to case and without .exe, .cmd or .bat.
func TestWindowsAgentBinaryNames(t *testing.T) {
	for bin, want := range map[string]bool{
		`C:\Tools\cursor.exe`: true, `C:\Tools\Cursor.CMD`: true, `C:\Tools\cursor.bat`: true, `C:\Tools\cursor`: true,
		`C:\Tools\cursor-agent.exe`: false, `C:\Tools\cursor.ps1`: false, `C:\Tools\cursor.exe.exe`: false,
	} {
		if got := agentBinIs(bin, "cursor"); got != want {
			t.Errorf("agentBinIs(%q, cursor) = %v, want %v", bin, got, want)
		}
	}
}

// An agent found only in the daemon's working directory is not found: Windows searches the current
// directory before PATH, and exec.LookPath reports that as exec.ErrDot.
func TestWindowsAgentInTheWorkingDirectoryIsNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude.exe"), []byte("MZ"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", t.TempDir())
	// Windows searches the current directory unless this variable is set (to any value): make sure it is not.
	t.Setenv("NoDefaultCurrentDirectoryInExePath", "")
	if err := os.Unsetenv("NoDefaultCurrentDirectoryInExePath"); err != nil {
		t.Fatal(err)
	}
	if p, err := detectClaudeBin(); err == nil {
		t.Fatalf("an agent in the working directory was found: %s", p)
	}
}

// encodedPS is s as powershell -EncodedCommand takes it.
func encodedPS(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for k, c := range u {
		binary.LittleEndian.PutUint16(b[2*k:], c)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// psQuote is s as a PowerShell single-quoted string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// processGone waits up to d for process pid to end and reports whether it has.
func processGone(t *testing.T, pid int, d time.Duration) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return true // no such process
		}
		t.Fatalf("OpenProcess(%d): %v", pid, err)
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(d.Milliseconds()))
	if err != nil {
		t.Fatalf("WaitForSingleObject(%d): %v", pid, err)
	}
	return ev == windows.WAIT_OBJECT_0
}

// terminatePID ends process pid, which this test started.
func terminatePID(pid int) {
	if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid)); err == nil {
		_ = windows.TerminateProcess(h, 1)
		_ = windows.CloseHandle(h)
	}
}

// readPID waits for the file a script writes its child's process id into.
func readPID(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
		if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
			return pid
		}
	}
	t.Fatalf("no process id in %s", path)
	return 0
}

// A run that times out is killed with everything it started: a grandchild that PowerShell started is
// gone too (procgroup's Job object), and runCmd returns within the timeout and the wait delay. The
// script uses .NET calls only: with the base environment of an agent (no LOCALAPPDATA), the first
// cmdlet that PowerShell loads on demand rebuilds its module analysis cache, which can take longer
// than the timeout on a fresh machine.
func TestWindowsRunCmdTimeoutKillsAGrandchild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	ping := systemTool(t, `System32\PING.EXE`)
	started := filepath.Join(dir, "started")
	script := "[System.IO.File]::WriteAllText(" + psQuote(started) + ", 'started')\n" +
		"$si = [System.Diagnostics.ProcessStartInfo]::new(" + psQuote(ping) + ", '-n 120 127.0.0.1')\n" +
		"$si.UseShellExecute = $false; $si.CreateNoWindow = $true\n" +
		"$p = [System.Diagnostics.Process]::Start($si)\n" +
		"[System.IO.File]::WriteAllText(" + psQuote(pidFile) + ", [string]$p.Id)\n" +
		"[System.Threading.Thread]::Sleep(120000)\n"
	ps := systemTool(t, `System32\WindowsPowerShell\v1.0\powershell.exe`)
	o := execInvokeOpts{WorkDir: dir, Env: execEnvFrom(os.Environ(), AutoReplyConfig{}, nil, "")}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	stdout, stderr, err := runCmd(ctx, o, nil, ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodedPS(script))
	elapsed := time.Since(start)
	_, startErr := os.Stat(started)
	t.Logf("stdout %q, stderr %q, script started: %v", stdout, stderr, startErr == nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runCmd: %v, want the deadline", err)
	}
	if elapsed > 27*time.Second {
		t.Fatalf("runCmd returned after %s", elapsed)
	}
	pid := readPID(t, pidFile)
	t.Cleanup(func() { terminatePID(pid) })
	if !processGone(t, pid, 5*time.Second) {
		t.Fatalf("the grandchild %d survived the timeout", pid)
	}
}

// An agent program that is a batch file is refused before it starts, whether it is configured by path or
// found through PATH and PATHEXT (an npm launcher such as claude.cmd): cmd.exe would cut the instructions
// at the first line break and interpret % & | in them. An .exe is not refused.
func TestWindowsBatchAgentIsRefused(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	for _, name := range []string{"claude.cmd", "codex.BAT"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("@echo ran>\""+marker+"\"\r\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	o := execInvokeOpts{WorkDir: dir, Env: execEnvFrom(os.Environ(), AutoReplyConfig{}, nil, "")}
	for _, bin := range []string{filepath.Join(dir, "claude.cmd"), filepath.Join(dir, "codex.BAT"), "claude", filepath.Join(dir, "claude")} {
		_, _, err := runCmd(context.Background(), o, nil, bin, "-p", "line one\nline two")
		if !errors.Is(err, errBatchAgent) {
			t.Errorf("%s: %v", bin, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a batch agent ran")
	}
	whoami := systemTool(t, `System32\whoami.exe`)
	if err := refuseBatchAgent(whoami); err != nil {
		t.Fatalf("an .exe agent: %v", err)
	}
	if out, _, err := runCmd(context.Background(), o, nil, whoami); err != nil || len(out) == 0 {
		t.Fatalf("an .exe agent did not run: %q, %v", out, err)
	}
}
