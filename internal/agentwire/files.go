//go:build !no_mcp

package agentwire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// change is one planned edit. Planning reads, applying writes; the split
// lets `anet agents` (and doctor) report what wire would do without doing
// it, and lets a conflict stop a tool before any of its files is touched.
type change struct {
	path   string
	before []byte // content when planned; nil when the file did not exist
	after  []byte // content to write
	del    bool   // remove the file instead of writing it
	// chmodOnly narrows the mode of an unchanged file (a Hermes config
	// holding the A2A token must stay 0600).
	chmodOnly bool
	// mode is the permission for a file that does not exist yet.
	mode fs.FileMode
	// narrow, when non-zero, masks the final permission: a file that
	// carries a token is never left readable by group or other, whatever
	// mode it had before.
	narrow fs.FileMode
	// rmdir names a directory to remove after a delete if it is then
	// empty (the skill directory anet created).
	rmdir string
	// run replaces the write with a tool's own CLI (Claude Code), one
	// command after another; verify checks the file afterwards.
	run    [][]string
	verify func(after []byte) error
	// generated is the text anet itself would write to this file. A file
	// that still holds exactly that is not backed up: the copy would add
	// nothing, and in the skill directory it would be left behind.
	generated []byte
	note      Msg
}

// readMaybe returns the file's content, or nil when it does not exist.
func readMaybe(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, ioError(path, err)
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// realPath follows a symbolic link to the file it names, so a config kept
// in a dotfiles repository and linked into place is edited where it lives
// instead of having its link replaced by a copy.
func realPath(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}

// apply makes the change and returns the backup it made, if any.
func (c change) apply(o *Options) (string, error) {
	path := realPath(c.path)
	cur, err := readMaybe(path)
	if err != nil {
		return "", err
	}
	// The file is read once to plan and once here. Something else writing
	// it in between (Claude Code rewrites ~/.claude.json on every start) is
	// reported rather than overwritten with a plan made from older content.
	if !sameContent(cur, c.before) {
		return "", fail("wire_changed_during_run", "path", c.path)
	}
	// realPath resolves every link that leads somewhere, so a link still
	// here points at nothing. Writing would replace the operator's link
	// with a file; following it would create a file wherever it points.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return "", fail("wire_dangling_symlink", "path", c.path)
	}
	perm := c.mode
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	} else if perm == 0 {
		// A plan that forgot to say is not a reason to create a file
		// nobody, not even its owner, can read.
		perm = 0o600
	}
	if c.narrow != 0 {
		perm &= c.narrow
	}
	if c.chmodOnly {
		return "", ioError(c.path, chmodPrivate(path, perm))
	}
	var bak string
	if cur != nil && (c.generated == nil || !bytes.Equal(cur, c.generated)) {
		if bak, err = backup(o, path, cur, perm); err != nil {
			if _, ok := AsMsg(err); ok {
				return "", err
			}
			return "", fail("wire_backup_failed", "path", c.path, "error", sysText(err))
		}
	}
	switch {
	case c.run != nil:
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		for _, argv := range c.run {
			out, err := o.Run(ctx, argv[0], argv[1:]...)
			if err != nil {
				return bak, fail("wire_cli_failed", "command", strings.Join(argv, " "), "error", cliError(err, out))
			}
		}
		if c.verify != nil {
			after, rerr := readMaybe(path)
			if rerr != nil {
				return bak, ioError(c.path, rerr)
			}
			if err := c.verify(after); err != nil {
				return bak, err
			}
		}
	case c.del:
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return bak, ioError(c.path, err)
		}
		if c.rmdir != "" {
			_ = os.Remove(c.rmdir) // only succeeds when empty, which is the point
		}
	default:
		if err := writeAtomic(path, c.after, perm); err != nil {
			return bak, ioError(c.path, err)
		}
	}
	return bak, nil
}

func sameContent(a, b []byte) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return bytes.Equal(a, b)
}

// backup copies data to <path>.anet-bak-<UTC time>, never over an existing
// backup: two edits of one file within a second (wire, then unwire) must
// not replace the copy of the original with a copy of anet's own edit.
func backup(o *Options, path string, data []byte, perm fs.FileMode) (string, error) {
	// A backup of a file that may hold a secret is private to its owner.
	perm &= 0o600
	if perm == 0 {
		perm = 0o600
	}
	stamp := o.Now().UTC().Format("20060102-150405")
	for i := 1; i <= 100; i++ {
		name := path + ".anet-bak-" + stamp
		if i > 1 {
			name += fmt.Sprintf("-%d", i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := protectNew(name, perm); err != nil {
			f.Close()
			os.Remove(name)
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(name)
			return "", err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			os.Remove(name)
			return "", err
		}
		return name, f.Close()
	}
	return "", fail("wire_backup_limit", "path", path)
}

// writeAtomic writes data through a sibling temp file and a rename, so a
// crash leaves the old file or the new one and never half of each — these
// are other programs' configurations, and a torn one stops them starting.
func writeAtomic(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".anet-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := protectNew(tmp, perm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// fileMode returns the permission bits of path, or 0 when it is absent
// (os_unix.go, os_windows.go).
func fileMode(path string) fs.FileMode { return accessMode(path) }

// short renders a path under home as ~/…, for output lines.
func short(o *Options, p string) string {
	if o.Home != "" {
		if rel, err := filepath.Rel(o.Home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}

// cliError is the first line of what a tool's own CLI said when it failed, or
// the error from starting it: enough to tell why, and bounded. Control
// characters (terminal escapes among them) are dropped, so the line cannot
// rewrite what anet prints around it.
func cliError(err error, out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if line == "" {
		line = err.Error()
	}
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
	if r := []rune(line); len(r) > cliErrorMax {
		line = string(r[:cliErrorMax]) + "..."
	}
	return line
}

// cliErrorMax bounds the part of a tool's own message that anet repeats.
const cliErrorMax = 200
