//go:build !windows

package backendconn

// tcpWithoutAllowFormat is Admit's refusal of a TCP target without allow_tcp (one %q: the URL).
const tcpWithoutAllowFormat = "url %q is reached over TCP, where another local user can take the port while the backend is down; " +
	"serve it on a Unix socket and write unix:///path/to/socket (the recommended form), or set allow_tcp: true " +
	"(a listener on loopback must then run as this daemon's user)"

// noSocketTableReason is checkTCP's refusal of a listener on this host when the system has no socket
// table to name its account.
const noSocketTableReason = "this system has no socket table to tell who holds a port on this host (only Linux does); serve the backend on a Unix socket (unix:///path)"

// admitUnix accepts a unix:// target at configuration time; its path and listener are checked when it
// is dialled.
func admitUnix(string) error { return nil }

// unixSocketsHere is nil where a socket's path and listener can be checked (CheckPath, checkPeer do
// that).
func unixSocketsHere(string) error { return nil }
