//go:build windows

package loopguard

import (
	"errors"
	"syscall"
)

// Winsock error codes (winerror.h). The syscall package names WSAEACCES but not WSAEADDRINUSE.
const (
	wsaEACCES     syscall.Errno = 10013
	wsaEADDRINUSE syscall.Errno = 10048
)

// addrInUseOS recognizes the Winsock codes a bind returns when the address cannot be had.
func addrInUseOS(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == wsaEADDRINUSE || errno == wsaEACCES
}
