//go:build !unix

package backendconn

import "io/fs"

// owners: no file owners outside Unix, so no socket path can be checked and a unix:// backend is
// refused.
func owners(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
