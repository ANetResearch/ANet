// Package daemon is the anet v0.1 app layer: a thin, centralized client of the official Hub. One daemon
// per operator holds a self-certifying identity (KEL), a durable local delegation log (interactions),
// and a relay client that talks to the Hub over HTTP — register, find, delegate, deliver, review.
//
// anet runs NO model and has NO P2P transport (P2P is a later version). The actual work is done by the
// operator's EXTERNAL agent (cursor/claude/openclaw, or any script), which reads tasks via the CLI
// (`inbox`/`thread`) and drives the conversation with `anet message` / `anet end`.
package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ANetResearch/ANet/internal/version"
)

// DaemonPointerPath is a uid-scoped, HOME-independent file a running daemon writes (and removes on
// shutdown) so a CLI invoked in a DIFFERENT environment than the operator — e.g. an agent's tool
// sandbox whose HOME/ANET_DATA_DIR differ — can still locate the live daemon. It is under a fixed
// /tmp path (NOT os.TempDir(), which honors $TMPDIR and could diverge between the daemon and the agent
// shell) keyed by uid: same uid → same path, and the 0700 dir / 0600 file confine it to that uid (which
// can already read the control token). The CLI consults it only as a FALLBACK when its own data dir has
// no live daemon, so an operator running several daemons with explicit ANET_DATA_DIR is unaffected
// (the last daemon's pointer simply wins for the no-data-dir fallback case, i.e. the one-daemon norm).
func DaemonPointerPath() string {
	return filepath.Join(RuntimeDir(), "daemon.json")
}

// RuntimeDir is the uid-scoped, HOME-independent runtime dir (/tmp/anet-<uid>) for cross-process
// coordination: the single-daemon pointer and the multi-daemon identity registry (see DaemonsDir).
func RuntimeDir() string {
	return filepath.Join("/tmp", fmt.Sprintf("anet-%d", os.Getuid()))
}

// DaemonsDir holds one small file per running daemon — the local "logged-in identities" the web console
// can switch between (like accounts in a chat app). Each daemon writes its entry on start (refreshing it
// after a rename via hub-register) and removes it on shutdown.
func DaemonsDir() string {
	return filepath.Join(RuntimeDir(), "daemons")
}

// Version is the anet release version (single source of truth in internal/version).
const Version = version.V

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

// InteractionsDir holds the local delegation log (inbound tasks + outbound delegations); see
// internal/runtime/interactions.
func (l Layout) InteractionsDir() string { return filepath.Join(l.Root, "interactions") }

// EnsureRoot creates the root data dir (0700 — it holds private keys).
func (l Layout) EnsureRoot() error { return os.MkdirAll(l.Root, 0o700) }

// writeFileAtomic writes data to path durably: write a sibling temp file then rename over the target,
// so a crash mid-write leaves either the old file or the new one — never a torn file. The temp file
// inherits perm; rename is atomic within one filesystem (temp is a sibling, so same fs).
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
