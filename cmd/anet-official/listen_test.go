package main

import (
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseListen(t *testing.T) {
	for in, want := range map[string]listenSpec{
		"unix:/run/anet-official/anet-tools/backend.sock":   {"unix", "/run/anet-official/anet-tools/backend.sock"},
		"unix:///run/anet-official/anet-tools/backend.sock": {"unix", "/run/anet-official/anet-tools/backend.sock"},
		"127.0.0.1:8611": {"tcp", "127.0.0.1:8611"},
		"localhost:8611": {"tcp", "127.0.0.1:8611"},
	} {
		got, err := parseListen(in)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v, want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "unix:", "unix:relative.sock", "unix://run/x.sock", "unix:/run/../x.sock", "unix:/run/x:y.sock",
		"unix:/run/x y.sock", "unix:/" + strings.Repeat("a", 120), "0.0.0.0:8611", "unix:/"} {
		if _, err := parseListen(in); err == nil {
			t.Errorf("%q must be refused", in)
		}
	}
	for in, want := range map[string]string{
		"unix:/run/a/backend.sock": "unix:///run/a/backend.sock",
		"127.0.0.1:8611":           "http://127.0.0.1:8611",
	} {
		if got, err := daemonURL(in); err != nil || got != want {
			t.Errorf("daemonURL(%s) = %q %v, want %q", in, got, err, want)
		}
	}
}

// serve's socket: 0660 with the group asked for, a stale socket replaced, and anything else at the
// path — a live socket, a file — left alone.
func TestListenUnix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backend.sock")

	// A socket a dead process left behind.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&fs.ModeSocket == 0 {
		t.Fatalf("no stale socket to test with: %v", err)
	}

	gid := os.Getgid()
	ln, err := listenUnix(path, strconv.Itoa(gid))
	if err != nil {
		t.Fatalf("over a stale socket: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o660 {
		t.Errorf("socket mode %04o, want 0660", fi.Mode().Perm())
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Host) })}
	go srv.Serve(ln)

	// A second instance does not take the path over from a live one.
	if _, err := listenUnix(path, ""); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("a live socket: %v", err)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("the first instance stopped answering: %v", err)
	}
	c.Close()

	// Closing removes the socket.
	ln.Close()
	srv.Close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("the socket outlived its listener: %v", err)
	}

	// A file that is not a socket is not removed.
	f := filepath.Join(dir, "file")
	os.WriteFile(f, []byte("x"), 0o600)
	if _, err := listenUnix(f, ""); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("a regular file: %v", err)
	}
	if b, _ := os.ReadFile(f); string(b) != "x" {
		t.Error("the file was touched")
	}
	if _, err := listenUnix(filepath.Join(dir, "g.sock"), "no-such-group-anet-test"); err == nil {
		t.Error("an unknown group was accepted")
	}
}
