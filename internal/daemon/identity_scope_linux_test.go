//go:build linux

package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// fileWatch reports every open of the files it watches (inotify): the
// run-time half of SI-38, which no source scan can give — a name built at
// run time, or a library that reads a directory, opens a file all the same.
type fileWatch struct {
	fd    int
	files map[int32]string
}

func watchFiles(t *testing.T, paths ...string) *fileWatch {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatalf("inotify: %v", err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	w := &fileWatch{fd: fd, files: map[int32]string{}}
	for _, p := range paths {
		wd, err := syscall.InotifyAddWatch(fd, p, syscall.IN_OPEN|syscall.IN_ACCESS|syscall.IN_MODIFY|
			syscall.IN_ATTRIB|syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF)
		if err != nil {
			t.Fatalf("inotify watch %s: %v", p, err)
		}
		w.files[int32(wd)] = p
	}
	return w
}

// events are what happened to the watched files since the last call.
func (w *fileWatch) events(t *testing.T) []string {
	t.Helper()
	var out []string
	buf := make([]byte, 64<<10)
	for {
		n, err := syscall.Read(w.fd, buf)
		if err == syscall.EAGAIN || n <= 0 {
			return out
		}
		if err != nil {
			t.Fatalf("inotify read: %v", err)
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			out = append(out, w.files[ev.Wd]+": "+inotifyMask(ev.Mask))
			off += syscall.SizeofInotifyEvent + int(ev.Len)
		}
	}
}

func inotifyMask(m uint32) string {
	var s []byte
	for _, b := range []struct {
		bit  uint32
		name string
	}{{syscall.IN_OPEN, "open"}, {syscall.IN_ACCESS, "read"}, {syscall.IN_MODIFY, "write"},
		{syscall.IN_ATTRIB, "attrib"}, {syscall.IN_DELETE_SELF, "delete"}, {syscall.IN_MOVE_SELF, "move"}} {
		if m&b.bit != 0 {
			s = append(s, b.name...)
			s = append(s, ' ')
		}
	}
	return string(bytes.TrimSpace(s))
}

func init() { runTimeRecoveryCheck = checkNoRecoveryFileOpened }

// checkNoRecoveryFileOpened is the run-time half of M44 (KEL D-2, C-36;
// SI-38): through a whole life — a join without a terminal, its notices,
// status, a poll, a restart and the identity status read from the data
// directory — the daemon process does not open the recovery file without a
// passphrase, its key, or the recovery file in the home directory. inotify
// sees every open, whoever makes it and however the name was built.
func checkNoRecoveryFileOpened(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	srv := newFakeHub(t)
	fh := fakeHubAt(t, srv.URL)
	fh.mu.Lock()
	fh.notices = true
	fh.mu.Unlock()
	d, _ := joinDaemon(t)
	d.join.hooks = witnessJoinHooks{d}
	aid := d.AID()
	tail := aid[len(aid)-12:]
	local := filepath.Join(d.layout.Root, "recovery_local.anetrec")
	key := filepath.Join(d.layout.Root, "recovery_local.key")
	elsewhere := filepath.Join(home, ".anet-recovery", tail+".anetrec")
	if err := os.MkdirAll(filepath.Dir(elsewhere), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{local, key, elsewhere} {
		if err := os.WriteFile(p, []byte("not for the daemon"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := watchFiles(t, local, key, elsewhere)
	if ev := w.events(t); len(ev) != 0 {
		t.Fatalf("events before the daemon did anything: %v", ev)
	}

	rs := newSeed(t)
	digest, err := identity.NextDigest(rs, aid, 2)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := controlCall(t, d.hHubRegister, "/hub-register", hubRegisterBody(srv.URL,
		`,"next_digest":"`+base64.StdEncoding.EncodeToString(digest[:])+`","unprotected":true`)); code != 200 || out["state"] != JoinRegistered {
		t.Fatalf("join: %d %v", code, out)
	}
	if s := identityStatusOf(t, d); s.State != IdentityOK || s.Recovery.State != RecoveryUnprotected {
		t.Fatalf("after the join: %+v", s)
	}
	if err := d.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.identityNote(a2ashape.Lang()) == "" {
		t.Fatal("no identity note for an unprotected identity")
	}
	if _, err := ReadIdentityStatus(d.layout, a2ashape.Lang()); err != nil {
		t.Fatal(err)
	}
	d.Close()
	again, err := New(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	again.IdentityStatus()
	time.Sleep(100 * time.Millisecond) // the loops of the restarted daemon run
	again.Close()
	if ev := w.events(t); len(ev) != 0 {
		t.Fatalf("the daemon touched a recovery file: %v", ev)
	}
	// The watch does see an open (the check would not pass by seeing
	// nothing).
	if _, err := os.ReadFile(key); err != nil {
		t.Fatal(err)
	}
	if ev := w.events(t); len(ev) == 0 {
		t.Fatal("inotify did not report a read of the key file")
	}
}
