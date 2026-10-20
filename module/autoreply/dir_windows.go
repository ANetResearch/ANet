//go:build windows

package autoreply

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ANetResearch/ANet/internal/fsperm"
)

// errWorkRootUnknown is the refusal when the user cache directory, under which the work dirs lie, is not
// known.
const errWorkRootUnknown = "exec work dir: the user cache directory is unknown"

// mkdirPrivate creates dir and its missing parents. dir itself is created with a protected DACL that
// grants access to this user, SYSTEM and Administrators only, or, when it exists and belongs to this
// user, is given that DACL; an existing dir that is a link or belongs to another account is refused.
func mkdirPrivate(dir string) error {
	return fsperm.MkdirAllPrivate(dir)
}

// mkdirTempPrivate creates a new directory in dir (os.TempDir() when dir is "") whose name is prefix
// followed by random characters, and returns its path. Unlike os.MkdirTemp, which leaves the new
// directory with the access its parent passes on, the directory is created private (mkdirPrivate): the
// parent can be the operator's configured work_dir, which other accounts may be able to read.
func mkdirTempPrivate(dir, prefix string) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	if strings.ContainsAny(prefix, `/\`) {
		return "", &os.PathError{Op: "mkdirtemp", Path: prefix, Err: errors.New("pattern contains path separator")}
	}
	if fi, err := os.Stat(dir); err != nil {
		return "", err
	} else if !fi.IsDir() {
		return "", &os.PathError{Op: "mkdirtemp", Path: dir, Err: errors.New("not a directory")}
	}
	for try := 0; try < 100; try++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		p := filepath.Join(dir, prefix+hex.EncodeToString(b[:]))
		if _, err := os.Lstat(p); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := mkdirPrivate(p); err != nil {
			return "", err
		}
		return p, nil
	}
	return "", fmt.Errorf("mkdirtemp %s: no free name for %s", dir, prefix)
}
