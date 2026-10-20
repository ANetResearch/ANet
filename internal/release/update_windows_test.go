package release

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperEnv makes this test binary, started as a child, a process that
// holds its executable image open: it prints "ready" and sleeps until its
// stdin closes or a minute has passed.
const helperEnv = "ANET_RELEASE_TEST_HELPER"

func TestWindowsHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "sleep" {
		t.Skip("started only as a child of the Windows replacement tests")
	}
	os.Stdout.WriteString("ready\n")
	done := make(chan struct{})
	go func() {
		io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
	}
	os.Exit(0)
}

// runningCopy copies this test binary to dir/anet.exe and starts it as a
// child that keeps running; stop ends it and waits for it.
func runningCopy(t *testing.T, dir string) (exe string, original []byte, stop func()) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	original, err = os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(dir, "anet.exe")
	if err := os.WriteFile(exe, original, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestWindowsHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"=sleep")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("the child did not start: %q %v", line, err)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		stdin.Close()
		ch := make(chan error, 1)
		go func() { ch <- cmd.Wait() }()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-ch
		}
	}
	t.Cleanup(stop)
	return exe, original, stop
}

func asides(t *testing.T, exe string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), filepath.Base(exe)+asideInfix) {
			out = append(out, filepath.Join(filepath.Dir(exe), e.Name()))
		}
	}
	return out
}

func writeNew(t *testing.T, dir string, body []byte) string {
	t.Helper()
	tmp := filepath.Join(dir, ".anet-update-test.exe")
	if err := os.WriteFile(tmp, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return tmp
}

// A running anet.exe is replaced: the new binary takes its name, the
// running one keeps running under the aside name, and SweepAside removes
// the aside file once nothing runs from it.
func TestWindowsReplaceRunningExecutable(t *testing.T) {
	dir := t.TempDir()
	exe, original, stop := runningCopy(t, dir)

	// The premise: a running image cannot be replaced by a plain rename.
	probe := writeNew(t, dir, []byte("probe"))
	if err := os.Rename(probe, exe); err == nil {
		t.Fatal("renaming over a running executable succeeded; the premise of rename-aside does not hold here")
	}
	os.Remove(probe)

	next := []byte("MZ the next release")
	tmp := writeNew(t, dir, next)
	if err := replaceExe(tmp, exe); err != nil {
		t.Fatalf("replaceExe while the binary runs: %v", err)
	}
	if b, err := os.ReadFile(exe); err != nil || !bytes.Equal(b, next) {
		t.Fatalf("%s is not the new binary: %v", exe, err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file is still there: %v", err)
	}
	as := asides(t, exe)
	if len(as) != 1 {
		t.Fatalf("aside files: %v, want one", as)
	}
	if b, err := os.ReadFile(as[0]); err != nil || !bytes.Equal(b, original) {
		t.Fatalf("the aside file is not the previous binary: %v", err)
	}

	// Files that only look like aside files are never removed.
	keep := []string{exe + ".old-12a", exe + ".old-", exe + ".older-1", filepath.Join(dir, "other.exe.old-1")}
	for _, k := range keep {
		if err := os.WriteFile(k, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(exe+".old-7", 0o755); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, exe+".old-7")

	// While the child runs from the aside file, it cannot be removed, and
	// SweepAside leaves it without complaint.
	SweepAside(exe)
	if got := asides(t, exe); len(got) != 4 { // the aside, .old-12a, .old-, .old-7
		t.Fatalf("after a sweep while the old binary runs: %v", got)
	}
	if _, err := os.Stat(as[0]); err != nil {
		t.Fatalf("the aside file of a running binary was removed: %v", err)
	}

	stop()
	// An aside of a stale earlier update, in another case.
	stale := filepath.Join(dir, "ANET.EXE.old-1700000000000")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The image of an ended process can stay mapped for a moment after
	// Wait returns; the sweep is repeated for a short while, as the next
	// `anet update` would.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		SweepAside(exe)
		if _, err := os.Stat(as[0]); errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			break
		}
	}
	for _, gone := range []string{as[0], stale} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was not removed: %v", gone, err)
		}
	}
	for _, k := range keep {
		if _, err := os.Stat(k); err != nil {
			t.Errorf("%s was removed: %v", k, err)
		}
	}
	if b, err := os.ReadFile(exe); err != nil || !bytes.Equal(b, next) {
		t.Fatalf("the sweep touched %s: %v", exe, err)
	}
}

// failRename makes renameFile fail for the renames whose source fail
// names, and do the others.
func failRename(t *testing.T, fail func(oldpath string) bool) {
	t.Helper()
	prev := renameFile
	renameFile = func(oldpath, newpath string) error {
		if fail(oldpath) {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errors.New("injected failure")}
		}
		return prev(oldpath, newpath)
	}
	t.Cleanup(func() { renameFile = prev })
}

// The new binary cannot take the name: the old one is renamed back, and
// nothing in the directory has changed but the staged file, which Apply
// removes.
func TestWindowsReplaceRestoresOnFailure(t *testing.T) {
	dir := t.TempDir()
	exe, original, _ := runningCopy(t, dir)
	tmp := writeNew(t, dir, []byte("MZ next"))
	failRename(t, func(oldpath string) bool { return oldpath == tmp })

	err := replaceExe(tmp, exe)
	if err == nil || errors.Is(err, ErrReplaceIncomplete) {
		t.Fatalf("replaceExe: %v, want the rename's own error", err)
	}
	if b, err := os.ReadFile(exe); err != nil || !bytes.Equal(b, original) {
		t.Fatalf("%s is not the previous binary after a failed replacement: %v", exe, err)
	}
	if as := asides(t, exe); len(as) != 0 {
		t.Fatalf("aside files left after the restore: %v", as)
	}
}

// The binary cannot even be moved aside: nothing changes.
func TestWindowsReplaceRefusesWhenTheBinaryCannotMove(t *testing.T) {
	dir := t.TempDir()
	exe, original, _ := runningCopy(t, dir)
	tmp := writeNew(t, dir, []byte("MZ next"))
	failRename(t, func(oldpath string) bool { return oldpath == exe })
	if err := replaceExe(tmp, exe); err == nil || !strings.Contains(err.Error(), "aside") {
		t.Fatalf("replaceExe: %v", err)
	}
	if b, err := os.ReadFile(exe); err != nil || !bytes.Equal(b, original) {
		t.Fatalf("%s changed: %v", exe, err)
	}
	if as := asides(t, exe); len(as) != 0 {
		t.Fatalf("aside files: %v", as)
	}
}

// Neither the new binary nor the restore succeeds: the error says so and
// names the file the previous binary is in.
func TestWindowsReplaceReportsAnIncompleteReplacement(t *testing.T) {
	dir := t.TempDir()
	exe, original, _ := runningCopy(t, dir)
	tmp := writeNew(t, dir, []byte("MZ next"))
	failRename(t, func(oldpath string) bool { return oldpath != exe })
	err := replaceExe(tmp, exe)
	if !errors.Is(err, ErrReplaceIncomplete) {
		t.Fatalf("replaceExe: %v, want ErrReplaceIncomplete", err)
	}
	as := asides(t, exe)
	if len(as) != 1 || !strings.Contains(err.Error(), as[0]) {
		t.Fatalf("the error %q does not name the aside file %v", err, as)
	}
	if b, rerr := os.ReadFile(as[0]); rerr != nil || !bytes.Equal(b, original) {
		t.Fatalf("the aside file is not the previous binary: %v", rerr)
	}
}

// With no binary in place yet, the staged file is renamed to its name.
func TestWindowsReplaceWithoutAPreviousBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "anet.exe")
	tmp := writeNew(t, dir, []byte("MZ first"))
	if err := replaceExe(tmp, exe); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(exe); err != nil || string(b) != "MZ first" {
		t.Fatalf("%v %q", err, b)
	}
	if as := asides(t, exe); len(as) != 0 {
		t.Fatalf("aside files: %v", as)
	}
}

// The staged binary is named so that it can be started for the probe.
func TestWindowsUpdateTempPatternIsAnExecutable(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), updateTempPattern)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !strings.HasSuffix(strings.ToLower(f.Name()), ".exe") || !strings.HasPrefix(filepath.Base(f.Name()), ".anet-update-") {
		t.Fatalf("temporary name %s", f.Name())
	}
	if err := chmodTemp(f, 0o755); err != nil {
		t.Fatalf("chmodTemp: %v", err)
	}
}
