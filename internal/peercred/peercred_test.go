package peercred

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// listen is a listener on a socket in a fresh directory.
func listen(t *testing.T, dir string) (net.Listener, string) {
	t.Helper()
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, sock
}

// accepted is the Cred the server end reads of the next connection to ln.
func accepted(t *testing.T, ln net.Listener) <-chan Cred {
	t.Helper()
	got := make(chan Cred, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		defer c.Close()
		cr, err := Of(c)
		if err != nil {
			t.Error(err)
			close(got)
			return
		}
		got <- cr
	}()
	return got
}

// A connection this very process makes comes back as this process, from
// either end: the one fact every check built on Of relies on.
func TestOfReportsThisProcessFromBothEnds(t *testing.T) {
	if !Supported() {
		t.Skip("peer credentials are not readable on " + runtime.GOOS)
	}
	ln, sock := listen(t, t.TempDir())
	server := accepted(t, ln)
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	client, err := Of(c)
	if err != nil {
		t.Fatal(err)
	}
	var srv Cred
	select {
	case srv = <-server:
	case <-time.After(10 * time.Second):
		t.Fatal("no connection accepted")
	}
	for end, cr := range map[string]Cred{"the listener, seen by the client": client, "the client, seen by the listener": srv} {
		if cr.UID != os.Getuid() || cr.GID != os.Getgid() || cr.Source == "" {
			t.Errorf("%s: %+v, want uid %d gid %d and a source", end, cr, os.Getuid(), os.Getgid())
		}
		if runtime.GOOS != "freebsd" && cr.PID != os.Getpid() {
			t.Errorf("%s: pid %d, want %d", end, cr.PID, os.Getpid())
		}
	}
}

// Anything that is not a Unix socket has no peer credentials, and saying so
// is an error, never a zero Cred (uid 0 is root).
func TestOfRefusesWhatIsNotAUnixSocket(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	cr, err := Of(a)
	if err == nil || cr.UID != -1 || cr.GID != -1 || cr.PID != -1 {
		t.Fatalf("a pipe: %+v %v", cr, err)
	}
	if Supported() && errors.Is(err, ErrUnsupported) {
		t.Fatal("a pipe on a supported platform is not ErrUnsupported: that error means the platform")
	}
}

// Linux, macOS and FreeBSD are read; everywhere else Of answers
// ErrUnsupported, whatever the connection, so that callers fail closed.
func TestSupportedIsLinuxDarwinFreeBSD(t *testing.T) {
	want := runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "freebsd"
	if Supported() != want {
		t.Fatalf("Supported() = %v on %s", Supported(), runtime.GOOS)
	}
	if !want {
		if cr, err := Of(&net.UnixConn{}); !errors.Is(err, ErrUnsupported) || cr.UID != -1 {
			t.Fatalf("an unsupported platform: %+v %v, want ErrUnsupported", cr, err)
		}
	}
}
