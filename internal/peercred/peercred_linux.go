package peercred

import "golang.org/x/sys/unix"

const supported = true

// fromFD reads SO_PEERCRED: the uid, gid and pid of the other end as the
// kernel recorded them at connect(2), or, seen from a client, at listen(2).
func fromFD(fd int) (Cred, error) {
	u, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return unknown, err
	}
	return Cred{UID: int(u.Uid), GID: int(u.Gid), PID: int(u.Pid), Source: "SO_PEERCRED"}, nil
}
