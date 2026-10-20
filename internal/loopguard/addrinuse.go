package loopguard

import (
	"errors"
	"syscall"
)

// IsAddrInUse reports a bind failure because the address is held: errors.Is(err, syscall.EADDRINUSE),
// and on windows also syscall.Errno 10048 (WSAEADDRINUSE) and 10013 (WSAEACCES: an excluded port
// range, or exclusive use by another socket). Standard library only (the loopguard deps test).
//
// It is meant for the error of a listen call. On Windows syscall.EADDRINUSE is a value the Go runtime
// invents and a real bind conflict never matches it, so the Winsock codes are recognized as well.
// WSAEACCES is included because that is what a bind returns when the port lies in a range the system
// has excluded, or when another account's socket holds the address (enhanced socket security): in
// either case the address cannot be had, which is what a caller of IsAddrInUse needs to know.
func IsAddrInUse(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.EADDRINUSE) || addrInUseOS(err)
}
