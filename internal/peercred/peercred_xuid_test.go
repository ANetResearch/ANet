//go:build unix

package peercred

// The far end as another account (peercredtest): J10's kernel half, on the
// platforms that have one.

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/peercred/peercredtest"
)

// J10, the kernel's half: a client reads the uid of the process that
// listens, not the socket file's owner. Here root listens on a socket file
// that belongs to this user.
func TestOfReportsAListenerOfAnotherAccount(t *testing.T) {
	if !Supported() {
		t.Skip("peer credentials are not readable on " + runtime.GOOS)
	}
	peercredtest.Require(t)
	sock := filepath.Join(t.TempDir(), "root.sock")
	peercredtest.RootListener(t, sock)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if uid, _, _ := ownerOf(fi); uid != os.Getuid() {
		t.Fatalf("the socket file is uid %d's, want this user's", uid)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cr, err := Of(c)
	if err != nil || cr.UID != 0 {
		t.Fatalf("the listener: %+v %v, want uid 0", cr, err)
	}
}

// And a server reads the uid of the account that connected.
func TestOfReportsAClientOfAnotherAccount(t *testing.T) {
	if !Supported() {
		t.Skip("peer credentials are not readable on " + runtime.GOOS)
	}
	_, other := peercredtest.OtherUser(t)
	dir := reachableDir(t)
	ln, sock := listen(t, dir)
	if err := os.Chmod(sock, 0o777); err != nil {
		t.Fatal(err)
	}
	server := accepted(t, ln)
	peercredtest.ConnectAs(t, sock, "")
	select {
	case cr := <-server:
		if cr.UID != other {
			t.Fatalf("the client: %+v, want uid %d", cr, other)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no connection accepted")
	}
}

// reachableDir is a directory under /tmp that another account can pass
// through but not write: t.TempDir() may sit below a directory only this
// user can enter (macOS's per-user temporary directory).
func reachableDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "anet-peercred-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	if err := os.Chmod(d, 0o711); err != nil {
		t.Fatal(err)
	}
	return d
}

// ownerOf is the uid and gid that own fi.
func ownerOf(fi os.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1, false
	}
	return int(st.Uid), int(st.Gid), true
}
