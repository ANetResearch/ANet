//go:build !windows

package autoreply

import "os"

// errWorkRootUnknown is the refusal when the user cache directory, under which the work dirs lie, is not
// known.
const errWorkRootUnknown = "exec work dir: the user cache directory is unknown (set XDG_CACHE_HOME or HOME)"

// mkdirPrivate creates dir and its missing parents and makes dir 0700.
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// mkdirTempPrivate creates a new directory in dir whose name begins with prefix (os.MkdirTemp, mode
// 0700) and returns its path.
func mkdirTempPrivate(dir, prefix string) (string, error) {
	return os.MkdirTemp(dir, prefix)
}
