//go:build linux

package main

import "syscall"

// Filesystem types (statfs f_type) that copy on write, so deleted data can
// stay in a snapshot (FS §5.4 retention). overlayfs is not looked into.
const (
	fsMagicBtrfs    = 0x9123683e
	fsMagicZFS      = 0x2fc12fc1
	fsMagicBcachefs = 0xca451a4e
)

// copyOnWriteFS reports whether dir is on btrfs, zfs or bcachefs.
func copyOnWriteFS(dir string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false
	}
	switch uint32(st.Type) {
	case fsMagicBtrfs, fsMagicZFS, fsMagicBcachefs:
		return true
	}
	return false
}
