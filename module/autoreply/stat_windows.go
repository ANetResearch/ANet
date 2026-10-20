//go:build windows

package autoreply

import "io/fs"

// allocatedBytes is the size of fi. The sandbox disk watch that uses it never runs on Windows, which has
// no sandbox (sandbox_other.go); the file size is the closest figure the file system reports here.
func allocatedBytes(fi fs.FileInfo) int64 {
	return fi.Size()
}
