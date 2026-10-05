//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANet/internal/peercred/peercredtest"
)

// PC-4 with a real second account (peercredtest, the CI peercred job): a
// process of another user that can reach the daemon socket — its directory
// passable, the socket writable by all — is closed before a byte of its
// hello is read, sent nothing, and does not take the daemon's place.
func TestADaemonSocketConnectionOfAnotherAccountIsClosed(t *testing.T) {
	peercredtest.Require(t)
	dir, err := os.MkdirTemp("/tmp", "anetpeer-xuid-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o711); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "d.sock")
	p, err := start(sock, filepath.Join(dir, "a.wire"), filepath.Join(dir, "rv"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	if err := os.Chmod(sock, 0o777); err != nil {
		t.Fatal(err)
	}
	d := dialRawDaemon(t, sock)
	d.send(frame{Op: "hello", V: wireVersion, Self: "aid-daemon"})
	waitSelf(t, p, "aid-daemon")
	if n := peercredtest.ConnectAs(t, sock, `{"op":"hello","v":3,"self":"aid-intruder"}`+"\n"); n != 0 {
		t.Fatalf("the other account was sent %d bytes", n)
	}
	if got := selfOf(p); got != "aid-daemon" {
		t.Fatalf("the other account took the daemon's place: %q", got)
	}
}
