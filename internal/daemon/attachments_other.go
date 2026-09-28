//go:build !linux

package daemon

import "os"

// openExistingDir opens the existing directory p one component at a time, following none as a link
// (walkDir); see the Linux version. Here it needs read permission on every directory above p.
func openExistingDir(p string) (*os.Root, error) { return walkDir(p) }
