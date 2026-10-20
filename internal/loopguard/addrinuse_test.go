package loopguard

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
)

// A second bind of an address that is held is recognized; other errors and nil are not.
func TestIsAddrInUse(t *testing.T) {
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
	if IsAddrInUse(nil) {
		t.Error("IsAddrInUse(nil) = true")
	}
	if IsAddrInUse(errors.New("address already in use")) {
		t.Error("an error that only reads like the condition was accepted")
	}
	if IsAddrInUse(fmt.Errorf("listen: %w", &os.SyscallError{Syscall: "bind", Err: syscall.EINVAL})) {
		t.Error("EINVAL accepted")
	}
	if !IsAddrInUse(fmt.Errorf("listen: %w", &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE})) {
		t.Error("a wrapped EADDRINUSE was not recognized")
	}
}
