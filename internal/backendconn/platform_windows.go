//go:build windows

package backendconn

import "fmt"

// tcpWithoutAllowFormat is Admit's refusal of a TCP target without allow_tcp (one %q: the URL). On
// Windows no listener on this host can be checked, so the advice is a backend on another host.
const tcpWithoutAllowFormat = "url %q is reached over TCP, which needs allow_tcp: true; on Windows only a backend on another host " +
	"over https can be used, because a listener on this host cannot be checked (there is no socket table to tell who holds a port)"

// noSocketTableReason is checkTCP's refusal of a listener on this host: Windows offers anet no socket
// table to name the account that holds a port.
const noSocketTableReason = "this system has no socket table to tell who holds a port on this host; on Windows, use a backend on another host over https"

// unixUnavailable says why a unix:// backend cannot be used on Windows.
const unixUnavailable = "a Unix socket backend is not available on Windows, because anet cannot verify which account listens on a socket there; " +
	"use a backend on another host over https"

// admitUnix refuses a unix:// target at configuration time on Windows.
func admitUnix(raw string) error { return fmt.Errorf("url %q: %s", raw, unixUnavailable) }

// unixSocketsHere refuses every unix:// dial on Windows before the path is looked at.
func unixSocketsHere(sock string) error { return refuse(sock, "%s", unixUnavailable) }
