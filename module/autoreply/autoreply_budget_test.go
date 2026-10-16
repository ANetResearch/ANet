package autoreply

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/execwork"
)

// fakeSandbox makes a sandboxed turn run its agent stub (execCommandForTest) directly in the turn's
// sandbox work dir, with ANET_FAKE_SANDBOX=1 in its environment: everything around the start inside
// bubblewrap — the gate, the budget, the work dir, the disk watch, the record — is the real path, on a
// host without a usable bubblewrap. egress, when set, is what the run reports through its endpoint.
func fakeSandbox(t *testing.T, egress *egressSummary) *atomic.Int64 {
	t.Helper()
	var runs atomic.Int64
	sandboxRunnerForTest = func(ctx context.Context, req sandboxRequest, workDir string) (string, *egressSummary, error) {
		runs.Add(1)
		cmd := exec.CommandContext(ctx, execCommandForTest)
		cmd.Dir = workDir
		cmd.Env = []string{"PATH=/usr/bin:/bin", "ANET_FAKE_SANDBOX=1"}
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		e := egress
		if e == nil {
			e = &egressSummary{}
		}
		return strings.TrimSpace(string(out)), e, err
	}
	t.Cleanup(func() { sandboxRunnerForTest = nil })
	return &runs
}

// withExit keeps the records that ended so.
func withExit(recs []map[string]any, exit string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["exit"] == exit {
			out = append(out, r)
		}
	}
	return out
}

// slotReplier answers after a while and records how many turns it answers at once, in all and per
// interaction's peer.
type slotReplier struct {
	peerOf  map[string]string // interaction id -> peer
	mu      sync.Mutex
	now     int
	nowPeer map[string]int
	max     int
	maxPeer int
	done    []string
}

func (r *slotReplier) Reply(ctx context.Context, rc replyContext, _ []chatTurn) (string, error) {
	r.mu.Lock()
	p := r.peerOf[rc.InteractionID]
	r.now++
	r.nowPeer[p]++
	r.max = max(r.max, r.now)
	r.maxPeer = max(r.maxPeer, r.nowPeer[p])
	r.mu.Unlock()
	select {
	case <-time.After(300 * time.Millisecond):
	case <-ctx.Done():
	}
	r.mu.Lock()
	r.now--
	r.nowPeer[p]--
	r.done = append(r.done, rc.InteractionID)
	r.mu.Unlock()
	return "answered", nil
}

// dirsSize counts each entry by the larger of its size and its allocation, and stops past the limit.
func TestDirsSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 10000), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Truncate(1 << 30) // a gigabyte that takes no space
	f.Close()
	if n := dirsSize([]string{dir}, 1<<40); n < 10000+(1<<30) {
		t.Fatalf("%d bytes: a sparse file must count by its size", n)
	}
	if n := dirsSize([]string{dir, "/nonexistent"}, 100); n <= 100 || n > 2<<30 {
		t.Fatalf("%d bytes for a limit of 100", n)
	}
}

// SB-8: removing a work dir an agent wrote into empties its read-only directories and never follows a
// link out of it: the mode of what a link points at is untouched (an agent still running can swap a
// directory for a link while the tree is walked; the changes go through an os.Root).
func TestRemoveWorkTreeStaysInTheTree(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "operator")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(base, "work")
	ro := filepath.Join(tree, "cache", "ro")
	if err := os.MkdirAll(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(ro, "f"), []byte("x"), 0o400)
	if err := os.Symlink(outside, filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "cache", "link")); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(ro, 0o500)
	execwork.RemoveTree(tree)
	if _, err := os.Lstat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tree survived: %v", err)
	}
	fi, err := os.Stat(outside)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("what a link pointed at: %v %v", fi, err)
	}
	// A link given as the tree itself is removed, and what it points at is left as it is.
	link := filepath.Join(base, "work-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(filepath.Join(outside, "ro"), 0o500)
	execwork.RemoveTree(link)
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the link survived: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(outside, "ro")); err != nil || fi.Mode().Perm() != 0o500 {
		t.Fatalf("the link's target was changed: %v %v", fi, err)
	}
	_ = os.Chmod(filepath.Join(outside, "ro"), 0o700)

	// The swap itself cannot be timed in a test; what makes it harmless is that the walk and the mode
	// changes go through the os.Root, never through a path the agent can repoint.
	file, err := parser.ParseFile(token.NewFileSet(), "autoreply_exec.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "execwork.RemoveTree" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && (x.Name == "os" && sel.Sel.Name == "Chmod" || x.Name == "filepath" && sel.Sel.Name == "WalkDir") {
					t.Errorf("execwork.RemoveTree calls %s.%s on a path, not through its os.Root", x.Name, sel.Sel.Name)
				}
			}
			return true
		})
	}
}

// SB-8: the agent can take the permissions of its work dir itself away; the removal gives them back
// and removes it all the same.
func TestRemoveWorkTreeTakesAnUnreadableTop(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root user: root reads and removes a directory of mode 000 anyway")
	}
	top := filepath.Join(t.TempDir(), "work")
	inner := filepath.Join(top, "x")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(inner, "f"), []byte("x"), 0o600)
	_ = os.Chmod(inner, 0)
	_ = os.Chmod(top, 0)
	execwork.RemoveTree(top)
	if _, err := os.Lstat(top); !errors.Is(err, os.ErrNotExist) {
		_ = os.Chmod(top, 0o700)
		_ = os.Chmod(inner, 0o700)
		t.Fatalf("a work dir of mode 000 survived its removal: %v", err)
	}
}

// BU-8: the agent decides the shape of the trees the watch adds up, and cannot hide what it wrote from
// it. What the walk cannot read — a directory made unreadable (the work dir itself included), one
// nested past the longest path the system takes — counts as over the limit; every entry counts at
// least one block, so a mass of empty files does not slip under it; a directory that is gone counts
// for nothing.
func TestTheDiskWatchCannotBeHiddenFrom(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a non-root user: root reads a directory of mode 000 anyway")
	}
	const limit = 1 << 20
	dir := t.TempDir()
	hidden := filepath.Join(dir, "hidden")
	if err := os.Mkdir(hidden, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := dirsSize([]string{dir}, limit); n > limit {
		t.Fatalf("%d bytes for a small readable tree", n)
	}
	for _, p := range []string{hidden, dir} {
		_ = os.Chmod(p, 0)
		n := dirsSize([]string{dir}, limit)
		_ = os.Chmod(p, 0o700)
		if n <= limit {
			t.Errorf("%s of mode 000: %d bytes, not over the limit", p, n)
		}
	}

	// Past PATH_MAX: made a directory at a time, relative to the one above, as an agent can.
	deep := t.TempDir()
	r, err := os.OpenRoot(deep)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("d", 200)
	for i := 0; i < 30; i++ {
		if err := r.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
		sub, err := r.OpenRoot(name)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		r = sub
	}
	if err := r.WriteFile("hog", make([]byte, 2*limit), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if n := dirsSize([]string{deep}, limit); n <= limit {
		t.Errorf("a tree deeper than the longest path: %d bytes, not over the limit", n)
	}
	execwork.RemoveTree(deep)
	if _, err := os.Lstat(deep); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deep tree survived its removal: %v", err)
	}

	empty := t.TempDir()
	for i := 0; i < limit/sandboxDiskEntryMin+1; i++ {
		f, err := os.Create(filepath.Join(empty, "e"+strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if n := dirsSize([]string{empty}, limit); n <= limit {
		t.Errorf("%d empty files: %d bytes, not over the limit", limit/sandboxDiskEntryMin+1, n)
	}
	if n := dirsSize([]string{filepath.Join(dir, "gone")}, limit); n != 0 {
		t.Errorf("a directory that is gone: %d bytes", n)
	}
}

// SB-8, SI-59: codex writes its reply to a file in its work dir, and a sandboxed codex can put a link
// to a file of the operator's in that file's place (the path is on its own command line). The reply is
// never read through such a link; a reply file codex wrote is read as before.
func TestCodexReplyFileIsNotReadThroughALink(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "id_ed25519")
	const key = "OPERATOR-PRIVATE-KEY-c0d3"
	if err := os.WriteFile(secret, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	codex := func(onReplyFile string) string {
		t.Helper()
		stub := writeStub(t, "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = -o ]; then "+onReplyFile+"; fi\n  shift\ndone\necho 'the reply on stdout'\n")
		got, err := invokeCodex(context.Background(), execInvokeOpts{AgentID: agentCodex, Conversation: "p", WorkDir: t.TempDir(),
			Command: stub, Env: []string{"PATH=/usr/bin:/bin"}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := codex("ln -s '" + secret + "' \"$2\""); got != "the reply on stdout" || strings.Contains(got, key) {
		t.Fatalf("a reply file linked to the operator's file: the reply was %q", got)
	}
	if got := codex("echo 'the reply in the file' > \"$2\""); got != "the reply in the file" {
		t.Fatalf("a reply file codex wrote: the reply was %q", got)
	}
}

// SB-8: execwork.DeleteInteraction removes one interaction's work dirs under both roots, read-only leftovers
// included, and nothing else.
func TestDeleteExecWorkDir(t *testing.T) {
	const aid = "bafyreiDeleteWorkDir000000001"
	for _, root := range []string{execwork.WorkRoot(), execwork.SandboxWorkRoot()} {
		for _, ix := range []string{"ix-gone", "ix-kept"} {
			ro := filepath.Join(root, execwork.AidShort(aid), execwork.IxDirName(ix), "cache", "ro")
			if err := os.MkdirAll(ro, 0o700); err != nil {
				t.Fatal(err)
			}
			_ = os.WriteFile(filepath.Join(ro, "f"), []byte("x"), 0o400)
			_ = os.Chmod(ro, 0o500)
		}
	}
	execwork.DeleteInteraction(aid, "ix-gone")
	for _, root := range []string{execwork.WorkRoot(), execwork.SandboxWorkRoot()} {
		if _, err := os.Stat(filepath.Join(root, execwork.AidShort(aid), execwork.IxDirName("ix-gone"))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the interaction's work dir survived: %v", root, err)
		}
		if _, err := os.Stat(filepath.Join(root, execwork.AidShort(aid), execwork.IxDirName("ix-kept"))); err != nil {
			t.Errorf("%s: another interaction's work dir went: %v", root, err)
		}
		execwork.RemoveTree(filepath.Join(root, execwork.AidShort(aid)))
	}
	execwork.DeleteInteraction("", "ix") // nothing to do, no panic
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
