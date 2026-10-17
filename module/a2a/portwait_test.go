//go:build !no_a2a

package a2a

// A recorded port held by a connection rather than a listener: an outgoing
// loopback connection takes its source port from the range the interface
// records its port in, and on a host with many identities and many local
// connections it can sit on an identity's port when that identity starts.
// Nobody can receive the token on such a port, so the interface waits for
// it to free instead of staying down with a new token.

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/localpeer"
)

// holdWithConnection connects from addr to a throwaway listener and
// returns a function that ends the connection (server side first, so the
// TIME_WAIT is not on addr).
func holdWithConnection(t *testing.T, addr string) (release func()) {
	t.Helper()
	srv, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := srv.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	local, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	// The stopped interface may still be closing its listener.
	var cli net.Conn
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		cli, err = (&net.Dialer{LocalAddr: local}).Dial("tcp", srv.Addr().String())
		if err == nil || time.Now().After(end) {
			break
		}
	}
	if err != nil {
		srv.Close()
		t.Skipf("cannot take %s as a source port here: %v", addr, err)
	}
	sc := <-accepted
	if ln, err := net.Listen("tcp", addr); err == nil {
		ln.Close()
		sc.Close()
		cli.Close()
		srv.Close()
		t.Skip("this kernel lets a listener share a port with a connection; nothing to test")
	}
	return func() { sc.Close(); time.Sleep(20 * time.Millisecond); cli.Close(); srv.Close() }
}

// recordFreshPort records a loopback port that has carried no connection
// (so no TIME_WAIT sits on it) as the interface's address.
func recordFreshPort(t *testing.T, stateDir string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := writePrivateFile(filepath.Join(stateDir, AddrFile), []byte(addr+"\n")); err != nil {
		t.Fatal(err)
	}
	return addr
}

func shortPortWait(t *testing.T, every, total time.Duration) {
	e, f := portRetryEvery, portRetryFor
	portRetryEvery, portRetryFor = every, total
	t.Cleanup(func() { portRetryEvery, portRetryFor = e, f })
}

func waitAddr(m *Module, want string, within time.Duration) bool {
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if m.Addr() == want {
			return true
		}
	}
	return false
}

func TestRecordedPortHeldByAConnectionIsAwaited(t *testing.T) {
	isolateHome(t)
	shortPortWait(t, 50*time.Millisecond, time.Minute)
	stateDir := t.TempDir()
	addr := recordFreshPort(t, stateDir)
	release := holdWithConnection(t, addr)
	m2, stop2 := startIn(t, stateDir)
	defer stop2()
	tok := readTok(t, stateDir)
	if a := m2.Addr(); a != "" {
		t.Fatalf("interface on %s while the recorded %s is held", a, addr)
	}
	release()
	if !waitAddr(m2, addr, 5*time.Second) {
		t.Fatalf("the interface did not start on %s once the connection ended (now %q)", addr, m2.Addr())
	}
	if readTok(t, stateDir) != tok {
		t.Fatal("the token was replaced although no process could have received it")
	}
	if _, err := os.Stat(filepath.Join(stateDir, ConflictFile)); err == nil {
		t.Fatal("a port-conflict record for a port only a connection held")
	}
}

// A listener that appears while the interface waits is a conflict as before.
func TestAwaitedPortTakenByAListenerIsAConflict(t *testing.T) {
	isolateHome(t)
	shortPortWait(t, 50*time.Millisecond, time.Minute)
	stateDir := t.TempDir()
	addr := recordFreshPort(t, stateDir)
	release := holdWithConnection(t, addr)
	defer release()
	m2, stop2 := startIn(t, stateDir)
	defer stop2()
	tok := readTok(t, stateDir)
	restore := localpeer.TreatAsForeignForTest(addr)
	defer restore()
	conflict := filepath.Join(stateDir, ConflictFile)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(conflict); err == nil {
			break
		}
	}
	if _, err := os.Stat(conflict); err != nil {
		t.Fatal("no port-conflict record once a listener of another user held the port")
	}
	if m2.Addr() != "" {
		t.Fatalf("interface on %s", m2.Addr())
	}
	if readTok(t, stateDir) == tok {
		t.Fatal("the token was kept although another user's listener may have received it")
	}
}

// The wait ends at portRetryFor with a conflict.
func TestAwaitedPortGivesUpWithAConflict(t *testing.T) {
	isolateHome(t)
	shortPortWait(t, 20*time.Millisecond, 200*time.Millisecond)
	stateDir := t.TempDir()
	addr := recordFreshPort(t, stateDir)
	release := holdWithConnection(t, addr)
	defer release()
	_, stop2 := startIn(t, stateDir)
	defer stop2()
	conflict := filepath.Join(stateDir, ConflictFile)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(conflict); err == nil {
			return
		}
	}
	t.Fatal("no port-conflict record after the wait ran out")
}
