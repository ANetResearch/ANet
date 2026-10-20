// Package daemon is the anet v0.1 app layer: a thin, centralized client of the official Hub. One daemon
// per operator holds a self-certifying identity (KEL), a durable local delegation log (interactions),
// and a relay client that talks to the Hub over HTTP — register, find, delegate, deliver, review.
//
// anet runs NO model and has NO P2P transport (P2P is a later version). The actual work is done by the
// operator's EXTERNAL agent (cursor/claude/openclaw, or any script), which reads tasks via the CLI
// (`inbox`/`thread`) and drives the conversation with `anet message` / `anet end`.
package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ANetResearch/ANet/internal/fsperm"
	"github.com/ANetResearch/ANet/internal/version"
)

// DaemonPointerPath is a uid-scoped, HOME-independent file a running daemon writes (and removes on
// shutdown) so a CLI invoked in a DIFFERENT environment than the operator — e.g. an agent's tool
// sandbox whose HOME/ANET_DATA_DIR differ — can still locate the live daemon. It lives in RuntimeDir,
// keyed by uid: the 0700 dir / 0600 file confine it to that uid (which can already read the control
// token). The CLI consults it only as a FALLBACK when its own data dir has no live daemon, so an
// operator running several daemons with explicit ANET_DATA_DIR is unaffected (the last daemon's
// pointer simply wins for the no-data-dir fallback case, i.e. the one-daemon norm). Readers use
// readDaemonPointerFile, which searches every candidate runtime dir.
func DaemonPointerPath() string {
	return filepath.Join(RuntimeDir(), "daemon.json")
}

// DaemonsDir holds one small file per running daemon — the local "logged-in identities" the web console
// can switch between (like accounts in a chat app). Each daemon writes its entry on start (refreshing it
// after a rename via hub-register) and removes it on shutdown.
func DaemonsDir() string {
	return filepath.Join(RuntimeDir(), "daemons")
}

// errNotPrivateDir reports a directory that failed checkPrivateDir.
var errNotPrivateDir = errors.New("not a private directory of this user")

// readDaemonPointerFile returns the contents of the first daemon pointer found in the candidate
// runtime dirs (runtimeDirCandidates).
func readDaemonPointerFile() ([]byte, error) {
	var firstErr error
	for _, d := range runtimeDirCandidates() {
		b, err := os.ReadFile(filepath.Join(d, "daemon.json"))
		if err == nil {
			return b, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no private runtime dir holds a daemon pointer: %w", os.ErrNotExist)
	}
	return nil, firstErr
}

// Version is the anet release version (single source of truth in internal/version).
const Version = version.V

// BuildCommit and BuildAt are what this binary was built from, stamped
// at link time. Empty-looking values mean an unstamped build, which says
// so rather than inventing a plausible commit.
var (
	BuildCommit = version.Commit
	BuildAt     = version.BuiltAt
	// BuildTags is the tag string this binary was compiled with, empty for
	// a default build. Two binaries at the same commit are not the same
	// binary if one was built with -tags shell and can run commands on its
	// host, and an operator auditing a fleet has to be able to read that
	// off the binary rather than off which file they think they copied.
	BuildTags = version.Tags
)

// Layout is the daemon's on-disk layout rooted at a data dir (default ~/.anet, env ANET_DATA_DIR).
type Layout struct{ Root string }

// DefaultRoot is ANET_DATA_DIR, else ~/.anet, else ./.anet if the home dir is unknown.
func DefaultRoot() string {
	if d := os.Getenv("ANET_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".anet"
	}
	return filepath.Join(home, ".anet")
}

// NewLayout returns the layout for root ("" → DefaultRoot).
func NewLayout(root string) Layout {
	if root == "" {
		root = DefaultRoot()
	}
	return Layout{Root: root}
}

func (l Layout) ConfigPath() string       { return filepath.Join(l.Root, "config.json") }
func (l Layout) IdentityPath() string     { return filepath.Join(l.Root, "identity.kel") }
func (l Layout) ControlTokenPath() string { return filepath.Join(l.Root, "control_token.txt") }
func (l Layout) LogPath() string          { return filepath.Join(l.Root, "daemon.log") }

// EvidenceLedgerPath is the local evidence chain. The .jsonl name is
// historical — the records are CBOR now (see ledger.go) — and the path is
// kept so an upgrading node continues its own chain instead of silently
// starting a second one beside it.
func (l Layout) EvidenceLedgerPath() string { return filepath.Join(l.Root, "evidence.ael.jsonl") }

// InteractionsDir holds the local delegation log (inbound tasks + outbound delegations); see
// internal/runtime/interactions.
func (l Layout) InteractionsDir() string { return filepath.Join(l.Root, "interactions") }

// writeFileAtomic writes data to path durably: write a sibling temp file then rename over the target,
// so a crash mid-write leaves either the old file or the new one — never a torn file. The temp file
// is given perm; rename is atomic within one filesystem (temp is a sibling, so same fs).
//
// Each write has a temp file of its own. With one fixed name, two writes at once (two config writes
// from the control plane, say) wrote into the same file, and the rename could put a mix of the two
// in place: a config.json that no longer parses, and a daemon that then refuses to start (found on
// the review of redteam F9).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = atomicSync(f) // the bytes are on disk before the name points at them
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, perm)
	}
	if err == nil && perm&0o077 == 0 {
		err = protectTemp(tmp)
	}
	if err == nil {
		err = fsperm.ReplaceFile(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = syncDir(dir) // and the rename itself; best effort, as not every file system can sync a directory
	return nil
}

// atomicSync flushes f to disk (KEL §2.9, D-35: temporary file, fsync, rename). A variable so a
// test can see it called.
var atomicSync = func(f *os.File) error { return f.Sync() }
