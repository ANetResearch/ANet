package backendconn

import (
	"errors"
	"net"
	"syscall"
)

// peerCred returns the uid and gid of the process that listens on the far end of c, a Unix socket
// connection, as the kernel recorded them when it called listen(2) (SO_PEERCRED).
func peerCred(c net.Conn) (uid, gid int, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("not a Unix socket connection")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := rc.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if serr != nil {
		return 0, 0, serr
	}
	return int(cred.Uid), int(cred.Gid), nil
}
