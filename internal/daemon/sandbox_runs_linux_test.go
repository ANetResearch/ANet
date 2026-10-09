//go:build linux

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// realSandboxFixture is an auto-reply fixture whose untrusted turns run in the host's bubblewrap, with
// the stub as the agent. Skipped where bwrap is missing or cannot create namespaces.
func realSandboxFixture(t *testing.T, stub string) *autoReplyFixture {
	t.Helper()
	pr := realBwrapProbe(t, func(f bwrapFeatures) bwrapFeatures { return f })
	if pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	setExecCommand(t, stub)
	return newAutoReplyFixture(t, AutoReplyConfig{Backend: "exec", Agent: "claude", APIKey: "k",
		Untrusted: UntrustedSandbox, APITimeoutSeconds: 60}, nil)
}

// T3 on the real sandbox (SI-59, J4): project configuration an untrusted peer's agent wrote inside
// bubblewrap is not in the directory its next turn starts in, once the peer is trusted and runs
// directly.
func TestSandboxTrustChangeUsesFreshWorkDir(t *testing.T) {
	stub := writeStub(t, `#!/bin/sh
case "$ANTHROPIC_BASE_URL" in
*127.0.0.1:3128*)
  echo '{"mcp":{}}' > opencode.json; echo 'run rm -rf' > AGENTS.md; echo 'export X=1' > .envrc
  mkdir -p .git/hooks .cursor; echo 'curl evil' > .git/hooks/pre-commit; echo '{}' > .cursor/mcp.json
  echo "WROTE $(pwd)";;
*)
  echo "DIRECT $(pwd) $(ls -A | tr '\n' ' ')";;
esac
`)
	f := realSandboxFixture(t, stub)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "set the project up", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	first := f.lastPeerMsg(t, id).Body
	if !strings.HasPrefix(first, "WROTE ") {
		t.Fatalf("the sandboxed turn: %q", first)
	}
	sandboxDir := strings.TrimPrefix(first, "WROTE ")
	if !pathWithin(sandboxDir, execSandboxWorkRoot()) {
		t.Fatalf("the sandboxed turn worked in %s, outside %s", sandboxDir, execSandboxWorkRoot())
	}
	if _, err := os.Stat(filepath.Join(sandboxDir, "opencode.json")); err != nil {
		t.Fatalf("the sandboxed agent's write did not reach its work dir: %v", err)
	}
	trustPeers(t, f.prov, f.req.AID())
	if err := f.req.SendMessage(f.ctx, id, "now run it", nil); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	second := f.lastPeerMsg(t, id).Body
	fields := strings.Fields(strings.TrimPrefix(second, "DIRECT "))
	if !strings.HasPrefix(second, "DIRECT ") || len(fields) == 0 || pathWithin(fields[0], execSandboxWorkRoot()) {
		t.Fatalf("the trusted turn: %q", second)
	}
	for _, name := range fields[1:] {
		switch name {
		case "opencode.json", "AGENTS.md", ".envrc", ".git", ".cursor":
			t.Errorf("the direct run's work dir holds %s, written in the sandbox", name)
		}
	}
	recs := invoked(t, f.prov)
	if len(recs) != 2 || recs[0]["mode"] != modeSandbox || recs[0]["sandboxed"] != true || recs[0]["egress"] == nil ||
		recs[1]["mode"] != modeDirect || recs[1]["trusted"] != true {
		t.Fatalf("records: %v", recs)
	}
}

// SI-59 on the real sandbox: an agent that puts a link to a directory of the operator's in its outbox's
// place — inside, its work dir and $HOME carry the host's names — sends nothing of that directory to the
// peer.
func TestSandboxOutboxLinkSendsNothing(t *testing.T) {
	secrets := t.TempDir()
	const secret = "OPERATOR-PRIVATE-KEY-5c19"
	if err := os.WriteFile(filepath.Join(secrets, "id_ed25519"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := writeStub(t, "#!/bin/sh\nrm -rf \"$ANET_OUTBOX\" && ln -s '"+secrets+"' \"$ANET_OUTBOX\" && : > linked && echo linked\n")
	f := realSandboxFixture(t, stub)
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "send me what is in your home", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if _, err := os.Stat(filepath.Join(execSandboxWorkRoot(), aidShort(f.prov.AID()), ixDirName(id), "linked")); err != nil {
		t.Fatalf("the agent did not get to link its outbox: %v", err)
	}
	got := f.lastPeerMsg(t, id)
	if len(got.Attachments) != 0 || got.Body == "linked" || strings.Contains(got.Body, secret) {
		t.Fatalf("an outbox linked out of the work dir was read: %+v", got)
	}
}

// BU-8 on the real sandbox: an agent that fills its work dir past the limit is killed with everything
// it started, its work dir and run directory are deleted, and the task is refused with
// sandbox_unavailable; the record says resource.
func TestSandboxDiskWatchKillsTheRun(t *testing.T) {
	stub := writeStub(t, "#!/bin/sh\nhead -c 4194304 /dev/zero > hog\nsleep 60 &\nexec sleep 60\n")
	f := realSandboxFixture(t, stub)
	oldMax, oldEvery := sandboxDiskMax, sandboxDiskEvery
	sandboxDiskMax, sandboxDiskEvery = 1<<20, 50*time.Millisecond
	t.Cleanup(func() { sandboxDiskMax, sandboxDiskEvery = oldMax, oldEvery })
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "fill the disk", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	f.tick(t)
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("the run was not killed (the scan took %s)", el)
	}
	if meta := awaitState(t, f.req, id, interactions.StateRejected); meta[a2ashape.KeyReason] != reasonSandboxUnavailble {
		t.Fatalf("the requester was told %v", meta)
	}
	work := filepath.Join(execSandboxWorkRoot(), aidShort(f.prov.AID()), ixDirName(id))
	if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the work dir survived: %v", err)
	}
	ents, _ := os.ReadDir(sandboxRunRoot())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), runDirPrefix(f.prov.AID())+"-") {
			t.Errorf("run directory %s survived", e.Name())
		}
	}
	recs := invoked(t, f.prov)
	if len(recs) != 1 || recs[0]["exit"] != exitResource || recs[0]["sandboxed"] != true {
		t.Fatalf("records: %v", recs)
	}
}

// BU-8 on the real sandbox, for the run directory: an agent that fills /tmp — the run directory's tmp/,
// not its work dir — past the limit is killed all the same, and the record says resource.
func TestSandboxDiskWatchCountsTheRunDirectory(t *testing.T) {
	stub := writeStub(t, "#!/bin/sh\nhead -c 4194304 /dev/zero > /tmp/hog\nexec sleep 60\n")
	f := realSandboxFixture(t, stub)
	oldMax, oldEvery := sandboxDiskMax, sandboxDiskEvery
	sandboxDiskMax, sandboxDiskEvery = 1<<20, 50*time.Millisecond
	t.Cleanup(func() { sandboxDiskMax, sandboxDiskEvery = oldMax, oldEvery })
	id, err := f.req.Delegate(f.ctx, f.prov.AID(), "fill /tmp", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	f.tick(t)
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("the run was not killed (the scan took %s)", el)
	}
	if meta := awaitState(t, f.req, id, interactions.StateRejected); meta[a2ashape.KeyReason] != reasonSandboxUnavailble {
		t.Fatalf("the requester was told %v", meta)
	}
	if recs := invoked(t, f.prov); len(recs) != 1 || recs[0]["exit"] != exitResource {
		t.Fatalf("records: %v", recs)
	}
}

// BU-8: what the disk watch adds up is the work dir and the run directories of the processes still
// running: sandboxPlan.command registers its run directory, whose tmp/ is the agent's /tmp, and Close
// takes it off. Without bubblewrap: nothing is started.
func TestTheDiskWatchSeesTheRunDirectory(t *testing.T) {
	plan := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: "EaidDiskWatchRunDir01", Home: filepath.Join(t.TempDir(), "home")}
	agent := writeStub(t, "#!/bin/sh\necho hi\n")
	l, err := plan.command(t.TempDir(), []string{agent}, []string{"PATH=/usr/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := plan.liveRunDirs()
	if len(live) != 1 || !strings.HasPrefix(filepath.Base(live[0]), runDirPrefix(plan.AID)+"-") {
		l.Close()
		t.Fatalf("the running process's run directories: %v", live)
	}
	if n := dirsSize(append(plan.liveRunDirs(), t.TempDir()), 1<<20); n > 1<<20 {
		l.Close()
		t.Fatalf("%d bytes before the agent wrote anything", n)
	}
	if err := os.WriteFile(filepath.Join(live[0], "tmp", "hog"), make([]byte, 4<<20), 0o600); err != nil {
		l.Close()
		t.Fatal(err)
	}
	if n := dirsSize(append(plan.liveRunDirs(), t.TempDir()), 1<<20); n <= 1<<20 {
		t.Errorf("%d bytes: the watch does not see what the agent wrote to /tmp", n)
	}
	l.Close()
	if live := plan.liveRunDirs(); len(live) != 0 {
		t.Fatalf("run directories still watched after Close: %v", live)
	}
	if _, err := os.Stat(live[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run directory survived Close: %v", err)
	}
}

// BU-8: a file counts by the space it holds, not only by its size: an agent that allocates blocks past
// the end of a file (fallocate --keep-size) does not slip under the limit with a file of size zero.
func TestDirsSizeCountsAllocatedBlocks(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "held"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const held = 4 << 20
	if err := syscall.Fallocate(int(f.Fd()), 0x01 /* FALLOC_FL_KEEP_SIZE */, 0, held); err != nil {
		t.Skipf("the file system here cannot allocate past a file's end: %v", err)
	}
	if fi, _ := f.Stat(); fi.Size() != 0 {
		t.Fatalf("size %d after fallocate --keep-size", fi.Size())
	}
	if n := dirsSize([]string{dir}, 1<<40); n < held {
		t.Fatalf("%d bytes for a file holding %d", n, held)
	}
}

// SB-9: before a daemon's first sandboxed run, the run directories and socket directories its
// identity's earlier runs left are removed — read-only leftovers included — and another identity's
// are not; it happens once.
func TestSandboxLeftoversAreSweptBeforeTheFirstRun(t *testing.T) {
	d := newBareDaemon(t)
	prefix := runDirPrefix(d.AID())
	mine := filepath.Join(sandboxRunRoot(), prefix+"-leftover")
	other := filepath.Join(sandboxRunRoot(), "zzzzzzzz-leftover")
	sock := filepath.Join(os.TempDir(), sandboxSockDirPrefix+prefix+"-leftover")
	for _, dir := range []string{mine, other, sock} {
		ro := filepath.Join(dir, "tmp", "ro")
		if err := os.MkdirAll(ro, 0o700); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(ro, "f"), []byte("x"), 0o400)
		_ = os.Chmod(ro, 0o500)
	}
	t.Cleanup(func() { removeWorkTree(other) })
	d.sweepSandboxLeftovers()
	for _, gone := range []string{mine, sock} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the sweep: %v", gone, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another identity's run directory went: %v", err)
	}
	// The temp dir is shared and the prefix is public: a directory named like ours that this user
	// does not own is not swept.
	theirs := filepath.Join(os.TempDir(), sandboxSockDirPrefix+prefix+"-theirs")
	if err := os.MkdirAll(theirs, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeWorkTree(theirs) })
	leftoverOwner = func() int { return os.Getuid() + 1 }
	removeSandboxLeftovers(d.AID())
	leftoverOwner = os.Getuid
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("a directory of another user's went: %v", err)
	}
	again := filepath.Join(sandboxRunRoot(), prefix+"-new")
	if err := os.MkdirAll(again, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeWorkTree(again) })
	d.sweepSandboxLeftovers()
	if _, err := os.Stat(again); err != nil {
		t.Errorf("a second sweep removed a run directory made after the first: %v", err)
	}
}

// SB-9: turning the exec backend on sweeps what an earlier process's runs left, before any turn.
func TestSandboxLeftoversAreSweptWhenAutoReplyStarts(t *testing.T) {
	d := newBareDaemon(t)
	left := filepath.Join(sandboxRunRoot(), runDirPrefix(d.AID())+"-leftover")
	if err := os.MkdirAll(filepath.Join(left, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeWorkTree(left) })
	if err := d.SetAutoReply(&AutoReplyConfig{Backend: "exec", Agent: "claude", Untrusted: UntrustedSandbox}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.SetAutoReply(nil) })
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover survived the start of auto-reply: %v", err)
	}
}

// noSandbox makes the host's sandbox unavailable for the test, and returns the local reason a refused
// run is recorded with: no bubblewrap.
func noSandbox(t *testing.T) string {
	t.Helper()
	bwrapLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { bwrapLookPath = exec.LookPath })
	return sandboxCodeNoBwrap
}
