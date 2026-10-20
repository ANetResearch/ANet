//go:build !windows

package autoreply

import (
	"io/fs"
	"syscall"
)

// allocatedBytes is the space the file system gave fi.
func allocatedBytes(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return 0
}
