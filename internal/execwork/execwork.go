// Package execwork is the layout and the retention of what the local
// execution of an auto-reply turn leaves on disk: the two work roots (direct
// runs and sandboxed runs), the per-interaction directories under them, the
// map from an interaction to the agent's own chat session, and the removal of
// all of it once the interaction has ended (FS-L11).
//
// It is the kernel's, not the auto-reply module's, because the retention
// period is the storage's: a node that turned the module off, or whose build
// has none, still removes what an earlier run left, so a change of build never
// keeps a peer's content on disk. The module uses the same package to make the
// directories.
package execwork

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ANetResearch/ANet/internal/pathsafe"
)

// WorkRoot is <user cache dir>/anet/work ($XDG_CACHE_HOME/anet/work on Linux), or "" when the
// cache dir is unknown.
func WorkRoot() string {
	c, err := os.UserCacheDir()
	if err != nil || c == "" {
		return ""
	}
	return filepath.Join(c, "anet", "work")
}

// SandboxWorkRoot is <user cache dir>/anet/work-sandbox, the root of sandboxed runs' work dirs. It
// never overlaps WorkRoot, which is on the sandbox's never-bind list (LEC SB-3), so a direct run
// never starts in a directory a sandboxed run wrote.
func SandboxWorkRoot() string {
	if root := WorkRoot(); root != "" {
		return root + "-sandbox"
	}
	return ""
}

// IxDirName names an interaction's work dir: "ix-" and a digest of its id, "selftest" for none. The id
// of an interaction that another node started is that node's text, and the work dir's path goes on
// command lines — bwrap's --bind, the agent's --workspace or -C — that every local user can read, and
// into the agent's own context; a digest carries nothing of it there (LEC AV-1), and its length is fixed.
func IxDirName(ix string) string {
	if ix == "" {
		return "selftest"
	}
	h := sha256.Sum256([]byte(ix))
	return "ix-" + hex.EncodeToString(h[:16])
}

// AidShort names an identity's work dir: the last 16 characters of the AID. AIDs are CIDs whose leading
// characters are a fixed multibase and codec prefix ("bafyrei…"), so the tail is the part that differs.
func AidShort(aid string) string {
	if len(aid) > 16 {
		aid = aid[len(aid)-16:]
	}
	return pathsafe.SafeName(aid)
}

// DeleteInteraction removes the work dirs of one interaction under both roots (LEC SB-8): the content
// retention deletes them within five minutes of the interaction's end (FS-L11). An agent may have left
// directories without write permission; they are removed all the same.
func DeleteInteraction(aid, ix string) {
	if aid == "" || ix == "" {
		return
	}
	for _, root := range []string{WorkRoot(), SandboxWorkRoot()} {
		if root != "" {
			RemoveTree(filepath.Join(root, AidShort(aid), IxDirName(ix)))
		}
	}
}

// RemoveTree removes a directory an agent wrote into: directories are made writable first (an
// agent can leave read-only ones, which os.RemoveAll alone cannot empty); symbolic links are removed,
// never followed. The walk and the mode changes go through an os.Root: an agent that is still running
// (its task ended under it) can swap a directory for a link between the walk and the change, and a
// plain chmod would follow that link to a file of the operator's outside the tree. The agent can take
// the permissions of its work dir itself away as well, so the top is made writable first, through its
// parent (a link put in its place is not followed out of that).
func RemoveTree(dir string) {
	if dir == "" {
		return
	}
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		if fi.Mode().Perm()&0o700 != 0o700 {
			if pr, err := os.OpenRoot(filepath.Dir(dir)); err == nil {
				_ = pr.Chmod(filepath.Base(dir), 0o700)
				_ = pr.Close()
			}
		}
		if r, err := os.OpenRoot(dir); err == nil {
			_ = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.IsDir() {
					_ = r.Chmod(p, 0o700)
				}
				return nil
			})
			_ = r.Close()
		}
	}
	_ = os.RemoveAll(dir)
}

// PruneExecWorkDirs removes the work dirs of the interactions that are not open (FS-L11), under both
// roots. The directories are listed first and only then is open asked which interactions are, so a
// directory made for a task that started in between is never taken for an ended one's: its task is
// open by the time open answers. Interaction ids are never reused, so a removed dir is never needed
// again; "selftest" (`anet autoreply test`) is not a task's and stays. A task that ended while its
// agent still runs has its directory removed under it; RemoveTree does not follow what the agent
// may have put in its place.
func PruneExecWorkDirs(aid string, open func() (map[string]bool, error)) error {
	if aid == "" {
		return nil
	}
	type workDir struct{ name, path string }
	var found []workDir
	for _, root := range []string{WorkRoot(), SandboxWorkRoot()} {
		if root == "" {
			continue
		}
		base := filepath.Join(root, AidShort(aid))
		ents, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() && e.Name() != "selftest" {
				found = append(found, workDir{e.Name(), filepath.Join(base, e.Name())})
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	ids, err := open()
	if err != nil {
		return err
	}
	keep := make(map[string]bool, len(ids))
	for ix := range ids {
		keep[IxDirName(ix)] = true
	}
	for _, w := range found {
		if !keep[w.name] {
			RemoveTree(w.path)
		}
	}
	return nil
}

// SweepAll is the retention period's whole removal (stateDir is the auto-reply module's state directory) (FS-L11): the work
// directories under both roots and the session bindings of the interactions
// that are not open. open lists the open interaction ids; it is asked after
// the directories are listed. It runs in every build, whether or not the
// auto-reply module is available.
func SweepAll(stateDir, aid string, open func() (map[string]bool, error)) (workErr, sessErr error) {
	workErr = PruneExecWorkDirs(aid, open)
	sessErr = Sessions(stateDir).PruneEnded(open)
	return workErr, sessErr
}
