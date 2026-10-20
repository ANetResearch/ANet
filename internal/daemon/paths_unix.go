//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// RuntimeDir is the uid-scoped, HOME-independent runtime dir this process writes cross-process
// coordination files to: the single-daemon pointer and the multi-daemon identity registry (see
// DaemonsDir).
//
// It prefers $XDG_RUNTIME_DIR/anet when XDG_RUNTIME_DIR names a private directory of this uid
// (A2A-DESIGN §7.8), and otherwise uses /tmp/anet-<uid>. The /tmp location is a fixed path rather than
// os.TempDir() because $TMPDIR can differ between the daemon and an agent's shell. A fixed path under a
// world-writable directory can be created in advance by another local user, which would redirect the
// CLI's fallback to a daemon of that user's choosing; every write and read therefore goes through
// checkPrivateDir, which refuses a directory that is a symbolic link, is owned by another uid, or grants
// group or other access.
//
// A daemon started from a login session and a CLI started without XDG_RUNTIME_DIR resolve different
// preferred directories. Readers therefore search all candidates (runtimeDirCandidates), including the
// conventional /run/user/<uid>, so the pointer and registry are found from either environment.
func RuntimeDir() string {
	if d := xdgRuntimeAnetDir(os.Getenv("XDG_RUNTIME_DIR")); d != "" {
		return d
	}
	return legacyRuntimeDir()
}

// legacyRuntimeDir is /tmp/anet-<uid>, the runtime dir used when no private XDG runtime dir exists.
func legacyRuntimeDir() string {
	return filepath.Join("/tmp", fmt.Sprintf("anet-%d", os.Getuid()))
}

// xdgRuntimeAnetDir returns <xdg>/anet when xdg is an absolute path to a private directory of this uid,
// else "".
func xdgRuntimeAnetDir(xdg string) string {
	if xdg == "" || !filepath.IsAbs(xdg) {
		return ""
	}
	if checkPrivateDir(xdg) != nil {
		return ""
	}
	return filepath.Join(xdg, "anet")
}

// runtimeDirCandidates lists every runtime dir a daemon of this uid may have written to, preferred
// first and without duplicates: $XDG_RUNTIME_DIR/anet, /run/user/<uid>/anet, /tmp/anet-<uid>. Entries
// that do not pass checkPrivateDir are left out, so a reader never trusts a directory another uid could
// have prepared.
func runtimeDirCandidates() []string {
	var out []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		if checkPrivateDir(d) == nil {
			out = append(out, d)
		}
	}
	add(xdgRuntimeAnetDir(os.Getenv("XDG_RUNTIME_DIR")))
	add(xdgRuntimeAnetDir(filepath.Join("/run/user", strconv.Itoa(os.Getuid()))))
	add(legacyRuntimeDir())
	return out
}

// checkPrivateDir verifies that dir is a directory (not a symbolic link to one), owned by this process's
// uid, with no group or other permission bits.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s: %w (symbolic link or not a directory)", dir, errNotPrivateDir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s: %w (owned by uid %d)", dir, errNotPrivateDir, st.Uid)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s: %w (mode %o)", dir, errNotPrivateDir, fi.Mode().Perm())
	}
	return nil
}

// ensurePrivateDir creates dir (and missing parents) with mode 0700 and then verifies it with
// checkPrivateDir. An existing directory owned by this uid whose mode grants group or other access is
// narrowed to 0700 first; that state is what an older anet or a permissive umask leaves behind, and
// narrowing it is safe because this uid owns it. A symbolic link or a directory of another uid is
// refused.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 && fi.Mode().Perm()&0o077 != 0 {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
			if err := os.Chmod(dir, 0o700); err != nil {
				return err
			}
		}
	}
	return checkPrivateDir(dir)
}

// EnsureRoot creates the root data dir (0700 — it holds private keys).
func (l Layout) EnsureRoot() error { return os.MkdirAll(l.Root, 0o700) }

// protectTemp makes a temporary file private before it is renamed into place. On unix the file's
// mode bits already do that, so there is nothing to add.
func protectTemp(string) error { return nil }

// syncDir fsyncs the directory dir, so a rename in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// runtimeSensitivePaths are the runtime directories a sandbox never binds: the legacy /tmp/anet-<uid>
// and the user runtime directory /run/user/<uid>.
func runtimeSensitivePaths() []string {
	return []string{legacyRuntimeDir(), filepath.Join("/run/user", fmt.Sprint(os.Getuid()))}
}
