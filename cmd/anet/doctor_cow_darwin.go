//go:build darwin

package main

import "syscall"

// copyOnWriteFS reports whether dir is on apfs.
func copyOnWriteFS(dir string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false
	}
	var name []byte
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return string(name) == "apfs"
}
