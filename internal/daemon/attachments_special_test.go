//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A path that is not a regular file is refused before it is read: /dev/zero reports a size of 0 and
// never ends, so reading it ran the daemon out of memory, and a named pipe blocked the open.
func TestAttachmentFromPathRefusesSpecialFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/dev/zero", fifo} {
		done := make(chan error, 1)
		go func() {
			_, err := attachmentFromPath(p)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("attachmentFromPath(%q) = %v; want a refusal as not a regular file", p, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("attachmentFromPath(%q) did not return", p)
		}
	}
}

func TestAttachmentFromPathReadsRegularFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := attachmentFromPath(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Data) != "hello" || a.Size != 5 || a.Name != "note.txt" || a.CID == "" {
		t.Errorf("attachment = %+v", a)
	}
}
