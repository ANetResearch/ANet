//go:build windows

package backendconn

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// F11: on Windows a unix:// backend is refused at configuration time and at dial time, a TCP target
// without allow_tcp is refused with advice that fits Windows, and a listener on this host is refused
// because there is no socket table; no refusal advises a Unix socket.
func TestWindowsBackendRefusals(t *testing.T) {
	rules, err := Policy{AllowTCP: true}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	unixT, err := Parse("unix:///run/backend.sock")
	if err != nil {
		t.Fatal(err)
	}
	err = rules.Admit(unixT)
	if err == nil || !strings.Contains(err.Error(), "not available on Windows") {
		t.Fatalf("Admit of a unix:// target: %v", err)
	}
	_, _, err = rules.CheckPath(unixT.Socket)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not available on Windows") {
		t.Fatalf("CheckPath: %v", err)
	}

	noTCP, err := Policy{}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	tcpT, err := Parse("https://backend.example.org/v1")
	if err != nil {
		t.Fatal(err)
	}
	err = noTCP.Admit(tcpT)
	if err == nil || !strings.Contains(err.Error(), "allow_tcp") || strings.Contains(err.Error(), "unix://") {
		t.Fatalf("Admit of TCP without allow_tcp: %v", err)
	}
	if err := rules.Admit(tcpT); err != nil {
		t.Fatalf("Admit of TCP with allow_tcp: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	err = rules.checkTCP(c)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "on Windows, use a backend on another host over https") ||
		strings.Contains(err.Error(), "Unix socket (unix") {
		t.Fatalf("checkTCP of a listener on this host: %v", err)
	}
}
