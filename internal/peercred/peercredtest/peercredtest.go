// Package peercredtest puts a second account on the far end of a Unix
// socket, for the tests that check what internal/peercred reports for it
// (LEC PC-1, J10). Tests only: nothing outside a _test.go file imports it.
//
// A test process cannot listen or connect as another user by itself, so the
// other end is a few lines of python run through `sudo -n`: as root for a
// listener (which then hands the socket file to the test's user, the case
// where the path checks pass and only the listener's uid can tell), and as
// an unprivileged account for a client. Both need passwordless sudo and
// python3; the tests that use them skip without, saying which is missing,
// unless ANET_TEST_SUDO=1 says the run must have them (the CI peercred job
// sets it, and there a skip is a failure).
package peercredtest

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Env is the variable that says this run has passwordless sudo and python3
// and must use them.
const Env = "ANET_TEST_SUDO"

// OtherUserEnv names the unprivileged account ConnectAs uses; "nobody" when
// unset.
const OtherUserEnv = "ANET_TEST_OTHER_USER"

// Require skips the test unless this run can put another account on the far
// end of a socket (Env), and fails it when Env says so but sudo or python3 is
// missing. Every helper calls it; a test calls it first so that it skips
// before it sets anything up.
func Require(t *testing.T) { t.Helper(); python(t) }

// python is the interpreter the helpers run, checked usable through sudo.
func python(t *testing.T) string {
	t.Helper()
	if os.Getenv(Env) != "1" {
		t.Skip("needs passwordless sudo and python3 to put another account on the far end of a socket; set " + Env + "=1 where both are there")
	}
	if os.Getuid() == 0 {
		t.Skip("needs to run as a non-root user: root is the other account here")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("%s=1 but there is no python3 on PATH", Env)
	}
	if out, err := exec.Command("sudo", "-n", "true").CombinedOutput(); err != nil {
		t.Fatalf("%s=1 but sudo -n does not work: %v %s", Env, err, out)
	}
	return py
}

// OtherUser is the unprivileged account ConnectAs runs as, and its uid.
func OtherUser(t *testing.T) (name string, uid int) {
	t.Helper()
	Require(t)
	name = os.Getenv(OtherUserEnv)
	if name == "" {
		name = "nobody"
	}
	u, err := user.Lookup(name)
	if err != nil {
		t.Fatalf("the other account %q: %v", name, err)
	}
	n, err := strconv.ParseInt(u.Uid, 10, 64)
	if n < 0 && n >= -1<<31 {
		// macOS lists nobody as -2: uid_t 4294967294, as the kernel reports it.
		n = int64(uint32(int32(n)))
	}
	uid = int(n)
	if err != nil || uid == os.Getuid() {
		t.Fatalf("the other account %q has uid %q, which is not another account's", name, u.Uid)
	}
	return name, uid
}

// rootListener listens on argv[1] as root, gives the socket file to uid
// argv[2] and gid argv[3], answers every connection with a fixed HTTP
// response once it has read the request's head (or the client's EOF), and
// ends when its stdin closes.
const rootListener = `
import os, socket, sys, threading
path, uid, gid = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.bind(path)
os.chown(path, uid, gid)
os.chmod(path, 0o600)
s.listen(8)
def serve():
    while True:
        try:
            c, _ = s.accept()
        except OSError:
            return
        try:
            c.settimeout(10)
            data = b""
            while b"\r\n\r\n" not in data:
                d = c.recv(4096)
                if not d:
                    break
                data += d
            c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
        except OSError:
            pass
        finally:
            c.close()
threading.Thread(target=serve, daemon=True).start()
print("ready", flush=True)
sys.stdin.read()
try:
    os.unlink(path)
except OSError:
    pass
`

// RootListener runs a listener as root on path (which must not exist; its
// directory must be one root can create in), and hands the socket file to
// this test's user: the socket is this user's, the process listening on it
// is not. It answers each connection with "HTTP/1.1 200 OK … ok". It stops,
// and removes the socket, when the test ends.
func RootListener(t *testing.T, path string) {
	t.Helper()
	py := python(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sudo", "-n", py, "-c", rootListener, path,
		strconv.Itoa(os.Getuid()), strconv.Itoa(os.Getgid()))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// sudo runs the interpreter as root; the context cannot signal it, so
	// the helper ends on its stdin's EOF instead.
	cmd.Cancel = func() error { return stdin.Close() }
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("the root listener did not end: %s", stderr.String())
		}
		cancel()
	})
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "ready" {
			err = io.ErrUnexpectedEOF
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("the root listener did not start: %v %s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the root listener did not start in time: %s", stderr.String())
	}
}

// connectAs connects to argv[1], writes argv[2], reads until the server
// closes, and prints "closed <bytes read>"; a connection it cannot make is
// an error (exit 1), a server that neither answers nor closes in 10 s is
// "timeout" (exit 3).
const connectAs = `
import socket, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(10)
s.connect(sys.argv[1])
try:
    s.sendall(sys.argv[2].encode())
except OSError:
    pass
n = 0
try:
    while True:
        d = s.recv(4096)
        if not d:
            break
        n += len(d)
except socket.timeout:
    print("timeout", flush=True)
    sys.exit(3)
except OSError:
    pass
print("closed", n, flush=True)
`

// ConnectAs connects to the socket at path as the other account
// (OtherUser), writes send, and reports how many bytes came back before the
// far end closed. The socket and every directory above it must let that
// account connect.
func ConnectAs(t *testing.T, path, send string) (read int) {
	t.Helper()
	py := python(t)
	name, _ := OtherUser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sudo", "-n", "-u", name, py, "-c", connectAs, path, send).CombinedOutput()
	if err != nil {
		t.Fatalf("connecting as %s: %v %s", name, err, out)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 || f[0] != "closed" {
		t.Fatalf("connecting as %s: %q", name, out)
	}
	n, err := strconv.Atoi(f[1])
	if err != nil {
		t.Fatalf("connecting as %s: %q", name, out)
	}
	return n
}
