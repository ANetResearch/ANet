package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/peercred"
)

// selfOf is the AID this peer process carries, as its daemon announced it.
func selfOf(p *peer) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.self
}

// waitSelf waits until p carries aid.
func waitSelf(t *testing.T, p *peer, aid string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for selfOf(p) != aid {
		if time.Now().After(deadline) {
			t.Fatalf("the peer process carries %q, want %q", selfOf(p), aid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// LEC PC-4: a connection to the daemon socket takes the daemon's
// place only when the process on the other end is this user's. One that is
// not is closed before a byte of it is read, and the daemon that was there
// keeps its place: it is still the one deliveries are handed to. (The test
// cannot connect as another account without sudo,
// TestADaemonSocketConnectionOfAnotherAccountIsClosed does; here the peer's
// credOf stands in for one.)
func TestAConnectionOfAnotherAccountDoesNotReplaceTheDaemon(t *testing.T) {
	dir := t.TempDir()
	p, err := start(filepath.Join(dir, "a.sock"), filepath.Join(dir, "a.wire"), filepath.Join(dir, "rv"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	d := dialRawDaemon(t, filepath.Join(dir, "a.sock"))
	d.send(frame{Op: "hello", V: wireVersion, Self: "aid-daemon"})
	waitSelf(t, p, "aid-daemon")

	setCredOf := func(f func(net.Conn) (peercred.Cred, error)) {
		p.mu.Lock()
		p.credOf = f
		p.mu.Unlock()
	}
	for _, why := range []string{"another account", "credentials it cannot read"} {
		switch why {
		case "another account":
			setCredOf(func(c net.Conn) (peercred.Cred, error) {
				cr, err := peercred.Of(c)
				cr.UID++
				return cr, err
			})
		default:
			setCredOf(func(net.Conn) (peercred.Cred, error) {
				return peercred.Cred{UID: -1, GID: -1, PID: -1}, errors.New("no credentials")
			})
		}
		intruder := dialRawDaemon(t, filepath.Join(dir, "a.sock"))
		// Written before the refusal can be seen: never read.
		intruder.c.Write([]byte(`{"op":"hello","v":3,"self":"aid-intruder"}` + "\n"))
		if _, _, err := intruder.read(); !errors.Is(err, io.EOF) && !isReset(err) {
			t.Fatalf("%s: the connection was not closed: %v", why, err)
		}
		if got := selfOf(p); got != "aid-daemon" {
			t.Fatalf("%s took the daemon's place: the peer process carries %q", why, got)
		}
		p.mu.Lock()
		same := p.conn != nil
		p.mu.Unlock()
		if !same {
			t.Fatalf("%s: the daemon's session was dropped", why)
		}
	}
	// The daemon that was there is still the one a delivery is handed to.
	setCredOf(nil)
	if err := handOffTo(t, p, d, "aid-daemon"); err != nil {
		t.Fatalf("the daemon was not handed the delivery: %v", err)
	}
}

// handOffTo hands p a delivery for aid and has d, the daemon expected to
// get it, ack it. It returns once the hand-off has: one left waiting for an
// ack would outlive the test, and read handOffTimeout while a later test
// sets it.
func handOffTo(t *testing.T, p *peer, d *rawDaemon, aid string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := p.handOff(frame{Op: "send", V: wireVersion, To: aid, Envelope: "ZW52"})
		done <- err
	}()
	f, _, err := d.read()
	if err == nil && (f.Op != "recv" || f.Envelope != "ZW52") {
		err = fmt.Errorf("handed %+v", f)
	}
	if err == nil {
		d.send(frame{Op: "ack", V: wireVersion, ID: f.ID})
	}
	select {
	case herr := <-done:
		if err == nil {
			err = herr
		}
	case <-time.After(2 * handOffTimeout):
		t.Fatal("the hand-off did not return")
	}
	return err
}

// isReset reports a connection the far end closed with unread data, which
// some kernels report as a reset rather than an EOF.
func isReset(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "broken pipe"))
}

// PC-4: the daemon socket's directory is made 0700 when missing; what is at
// the socket's path is removed only when it is a socket nobody listens on;
// and a path the daemon would refuse to connect through (a directory others
// can write) is refused at start, saying why.
func TestTheDaemonSocketIsListenedOnSafely(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")

	// A directory that is not there yet.
	sock := filepath.Join(dir, "run", "anetpeer", "d.sock")
	p, err := start(sock, filepath.Join(dir, "a.wire"), rv, "")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Dir(sock))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the socket's new directory: %v %v, want 0700", fi.Mode(), err)
	}
	d := dialRawDaemon(t, sock)
	d.send(frame{Op: "hello", V: wireVersion, Self: "aid-first"})
	waitSelf(t, p, "aid-first")
	// A second peer process on the same path: the socket is in use, and is
	// left to the first. Asking whether anyone listens there costs the
	// daemon of the first nothing: it is still the one a delivery is handed
	// to.
	if _, err := start(sock, filepath.Join(dir, "b.wire"), rv, ""); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("a socket in use: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the first serve and close the second's question
	if err := handOffTo(t, p, d, "aid-first"); err != nil {
		t.Fatalf("after a second peer process asked whether the socket is in use, the daemon is no longer handed deliveries: %v", err)
	}
	p.stop()

	// A socket nobody listens on (a peer process that died) is replaced.
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	p2, err := start(stale, filepath.Join(dir, "c.wire"), rv, "")
	if err != nil {
		t.Fatalf("a stale socket: %v", err)
	}
	p2.stop()

	// Anything else at the path is left as it is.
	file := filepath.Join(dir, "file.sock")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := start(file, filepath.Join(dir, "d.wire"), rv, ""); err == nil {
		t.Fatal("a regular file at the socket's path was replaced")
	}
	if b, _ := os.ReadFile(file); string(b) != "keep" {
		t.Fatal("the file at the socket's path was changed")
	}

	// A directory every user can write: the daemon would not connect.
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := start(filepath.Join(open, "d.sock"), filepath.Join(dir, "e.wire"), rv, ""); err == nil ||
		!strings.Contains(err.Error(), "the daemon would not connect") {
		t.Fatalf("a socket in a directory every user can write: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(open, "d.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused socket was left behind: %v", err)
	}
}

// Where the kernel cannot say who connects, anetpeer does not serve the
// daemon socket at all: anyone on the machine could take the daemon's place.
func TestAPlatformThatCannotTellIsNotServed(t *testing.T) {
	old := peercredSupported
	peercredSupported = func() bool { return false }
	t.Cleanup(func() { peercredSupported = old })
	dir := t.TempDir()
	_, err := start(filepath.Join(dir, "a.sock"), filepath.Join(dir, "a.wire"), filepath.Join(dir, "rv"), "")
	if err == nil || !strings.Contains(err.Error(), "cannot tell who connects") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a.wire")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the peer wire was left listening: %v", err)
	}
}
