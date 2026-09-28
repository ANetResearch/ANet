//go:build unix

package backendconn

import (
	"io/fs"
	"syscall"
)

// owners reads the uid and gid of an entry os.Lstat described.
func owners(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
