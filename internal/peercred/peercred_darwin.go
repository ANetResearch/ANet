package peercred

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const supported = true

// xucredVersion is XUCRED_VERSION of <sys/ucred.h>. A struct of another
// version has another layout, and reading it as this one would hand back
// whatever sits where the uid used to be.
const xucredVersion = 0

// fromFD reads LOCAL_PEERCRED (what getpeereid(3) uses) and, separately,
// LOCAL_PEERPID. The gid is the first group, as getpeereid has it.
func fromFD(fd int) (Cred, error) {
	x, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return unknown, err
	}
	if x.Version != xucredVersion {
		return unknown, fmt.Errorf("peercred: xucred version %d, expected %d", x.Version, xucredVersion)
	}
	c := Cred{UID: int(x.Uid), GID: -1, PID: -1, Source: "LOCAL_PEERCRED"}
	if x.Ngroups > 0 {
		c.GID = int(x.Groups[0])
	}
	if pid, err := unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID); err == nil {
		c.PID = pid
	}
	return c, nil
}
