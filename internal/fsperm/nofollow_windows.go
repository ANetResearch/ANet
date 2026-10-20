//go:build windows

package fsperm

// NoFollow is the flag os.Root.OpenFile callers add to refuse a final link: syscall.O_NOFOLLOW on unix,
// 0 on windows (the Root never follows a reparse point out of the root there, F9). Every call site
// that uses it must also compare os.SameFile(<Lstat before>, <f.Stat() after>).
//
// On Windows syscall.O_NOFOLLOW does not exist. os.Root opens every path component without letting
// the kernel follow a reparse point and resolves a symbolic link itself only when its target stays
// inside the root; the os.SameFile comparison closes the window between the Lstat and the open.
const NoFollow = 0
