//go:build !no_a2a && !windows

package a2a

import "os"

// keepPrivate narrows an existing state file to 0600 on every start.
func keepPrivate(path string) error { return os.Chmod(path, 0o600) }

// protectTemp has nothing to add on unix: the temporary file is already 0600.
func protectTemp(string) error { return nil }
