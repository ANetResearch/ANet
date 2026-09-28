//go:build !linux

package backendconn

import "net"

// peerCred: only Linux is read (SO_PEERCRED); elsewhere the path checks stand alone.
func peerCred(net.Conn) (uid, gid int, err error) { return 0, 0, errNoPeerCred }
