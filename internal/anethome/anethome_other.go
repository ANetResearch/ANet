//go:build !windows

package anethome

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// osValidName adds nothing outside Windows.
func osValidName(string) bool { return true }

// nameTaken: an entry of exactly this name.
func nameTaken(idsDir, name string) bool {
	_, err := os.Lstat(filepath.Join(idsDir, name))
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// listedAlready: directory names are distinct outside Windows.
func listedAlready([]Identity, string) bool { return false }
