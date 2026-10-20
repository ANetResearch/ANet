//go:build unix

package fsperm

import "syscall"

// NoFollow is the flag os.Root.OpenFile callers add to refuse a final link: syscall.O_NOFOLLOW on unix,
// 0 on windows (the Root never follows a reparse point out of the root there, F9). Every call site
// that uses it must also compare os.SameFile(<Lstat before>, <f.Stat() after>).
const NoFollow = syscall.O_NOFOLLOW
