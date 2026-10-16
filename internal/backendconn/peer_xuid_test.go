//go:build unix

package backendconn

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/ANetResearch/ANet/internal/peercred"
	"github.com/ANetResearch/ANet/internal/peercred/peercredtest"
)

// J10: a socket backend whose listener runs as another account is refused, and sent nothing; set to
// peer_check "path-only" it is accepted, the path being all that is checked. Here the socket file is
// this user's, in this user's directory — every path check passes — and root listens on it, so only
// the kernel's word on the listener can tell. Runs where peercredtest can put root on the far end (the
// CI peercred job, on Linux and macOS, where a skip is a failure).
func TestAListenerOfAnotherAccountIsRefusedUnlessPathOnly(t *testing.T) {
	if !peercred.Supported() {
		t.Skip("the listening account is not readable on " + runtime.GOOS)
	}
	peercredtest.Require(t)
	sock := filepath.Join(privateDir(t), "b.sock")
	peercredtest.RootListener(t, sock)
	if _, owner, err := (Rules{expected: -1, group: -1, self: os.Getuid()}).CheckPath(sock); err != nil || owner != os.Getuid() {
		t.Fatalf("the path checks: owner %d, %v; want this user's socket, passing", owner, err)
	}
	r, _ := Policy{}.Resolve()
	_, err := get(t, r, "unix://"+sock)
	var re *RefusedError
	if !errors.As(err, &re) || re.Code != CodePeerUIDMismatch || re.Args["n"] != "0" || re.Args["name"] != "uid "+strconv.Itoa(os.Getuid()) {
		t.Fatalf("root listening on this user's socket: %v", err)
	}
	// Expecting this user by name changes nothing: the listener is still root.
	r, _ = Policy{ExpectedUser: strconv.Itoa(os.Getuid())}.Resolve()
	if _, err := get(t, r, "unix://"+sock); !errors.As(err, &re) || re.Code != CodePeerUIDMismatch {
		t.Fatalf("expected_user this user, root listening: %v", err)
	}
	r, _ = Policy{PeerCheck: PeerCheckPathOnly}.Resolve()
	if body, err := get(t, r, "unix://"+sock); err != nil || body != "ok" {
		t.Fatalf("path-only: %q %v", body, err)
	}
	// What the client reads is the listener's account, not the file's.
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if cr, err := peercred.Of(c); err != nil || cr.UID != 0 {
		t.Fatalf("peercred of the listener: %+v %v", cr, err)
	}
}
