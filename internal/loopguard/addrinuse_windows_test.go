//go:build windows

package loopguard

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

// On Windows a held address is reported as WSAEADDRINUSE, which IsAddrInUse must recognize, as it
// must WSAEACCES (an excluded port range); WSAECONNREFUSED is not a bind conflict.
func TestWindowsIsAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = net.Listen("tcp", ln.Addr().String())
	if err == nil {
		t.Fatal("a second bind of a held address succeeded")
	}
	if !IsAddrInUse(err) {
		t.Fatalf("IsAddrInUse(%v) = false", err)
	}
	for _, c := range []struct {
		errno syscall.Errno
		want  bool
	}{{10048, true}, {10013, true}, {10061, false}} {
		e := fmt.Errorf("listen: %w", &os.SyscallError{Syscall: "bind", Err: c.errno})
		if got := IsAddrInUse(e); got != c.want {
			t.Errorf("IsAddrInUse(errno %d) = %v, want %v", uintptr(c.errno), got, c.want)
		}
	}
}
