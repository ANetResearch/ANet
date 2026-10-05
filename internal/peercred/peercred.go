// Package peercred tells either end of a Unix socket connection which account
// is on the other end, as the kernel recorded it (LEC PC-1).
//
// A Unix socket is an authorisation boundary only when that word is asked
// for. File modes say who may connect; they do not say who did, nor who is
// listening on a path the daemon is about to hand a token, a call's arguments
// or a delegation to. So a client that dials a backend or the peer process
// (internal/backendconn) and a server that takes connections from its daemon
// (tools/anetpeer) both ask here, and refuse before a byte is written or read.
//
// The semantics are those of getpeereid(3), with the pid where the platform
// gives it:
//
//	linux    SO_PEERCRED
//	darwin   LOCAL_PEERCRED (xucred, its version checked) and LOCAL_PEERPID
//	freebsd  LOCAL_PEERCRED (xucred, its version checked)
//
// Seen from a client, the credentials are those of the process that called
// listen(2); seen from a server, those of the process that called connect(2).
// Every other platform answers ErrUnsupported, and a caller must then refuse
// (fail closed) unless an operator said otherwise for that one connection
// (backendconn's peer_check "path-only").
//
// Standard library and golang.org/x/sys/unix only (the deps test in
// internal/loopguard). ANetLink keeps a copy of the same rule in its own
// internal/peercred, since an internal package of another module cannot be
// imported.
package peercred

import (
	"errors"
	"net"
)

// ErrUnsupported means this platform cannot tell who is on the other end of
// a Unix socket, so a caller must fail closed.
var ErrUnsupported = errors.New("peercred: this platform cannot report who is on the other end of a Unix socket")

// Cred is what the kernel recorded about the process on the other end.
type Cred struct {
	UID, GID int
	// PID is -1 where the platform does not report it.
	PID int
	// Source names the mechanism that answered (SO_PEERCRED,
	// LOCAL_PEERCRED), for a log line.
	Source string
}

// unknown goes with every error: uid 0 is root, so a zero Cred must never
// stand for "could not tell".
var unknown = Cred{UID: -1, GID: -1, PID: -1}

// Supported reports whether Of can answer on this platform: whether a
// refusal for ErrUnsupported would refuse every connection here. doctor and
// anetpeer ask it to say so once, up front.
func Supported() bool { return supported }

// Of returns the credentials of the process on the other end of c, which
// must be a Unix socket connection. On a platform with no such word it is
// ErrUnsupported, whatever c is.
func Of(c net.Conn) (Cred, error) {
	if !supported {
		return unknown, ErrUnsupported
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return unknown, errors.New("peercred: not a Unix socket connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return unknown, err
	}
	cred, cerr := unknown, error(nil)
	if err := raw.Control(func(fd uintptr) { cred, cerr = fromFD(int(fd)) }); err != nil {
		return unknown, err
	}
	if cerr != nil {
		return unknown, cerr
	}
	return cred, nil
}
