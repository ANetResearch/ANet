package peercred

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const supported = true

// xucredVersion is XUCRED_VERSION of <sys/ucred.h>; see the darwin file.
const xucredVersion = 0

// fromFD reads LOCAL_PEERCRED (what getpeereid(3) uses). FreeBSD keeps the
// pid in a union the x/sys binding does not expose, so PID stays unknown.
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
	return c, nil
}
