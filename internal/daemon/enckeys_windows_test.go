//go:build windows

package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/seal"
)

// FS-K12 on Windows: writeRingFile holds the old key ring file open across the rename that replaces
// it, to overwrite the replaced bytes afterwards. That handle shares delete access
// (fsperm.OpenReplaceable), so the rename succeeds at once and every write after the first succeeds;
// with os.OpenFile the second write failed. Each write reads back as written, the replaced file is
// overwritten with zeros (seen through a hard link to it), and no failure is counted.
func TestWindowsKeyRingWritesRepeatedly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enc_keys.cbor")
	const aid = "test-aid"
	now := func() uint64 { return uint64(time.Now().UnixMilli()) }
	r, err := openKeyRing(path, aid, now)
	if err != nil {
		t.Fatal(err)
	}
	var noted []error
	r.noteErr = func(_ string, err error) { noted = append(noted, err) }
	start := epochOf(now())
	for i := 1; i <= 4; i++ {
		s := start + uint64(i-1)*seal.EpochMS
		kp, err := seal.GenerateKeyPair(seal.SuiteX25519, s, s+seal.EpochMS)
		if err != nil {
			t.Fatal(err)
		}
		r.mu.Lock()
		r.keys = append(r.keys, &ringKey{Pair: *kp})
		r.lastSeq = uint64(i)
		r.mu.Unlock()

		var link string
		var oldBytes []byte
		if i > 1 {
			link = filepath.Join(filepath.Dir(path), "before-write")
			_ = os.Remove(link)
			if err := os.Link(path, link); err != nil {
				t.Fatalf("write %d: hard link to the ring file: %v", i, err)
			}
			if oldBytes, err = os.ReadFile(link); err != nil {
				t.Fatal(err)
			}
		}

		began := time.Now()
		if err := r.persist(); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if d := time.Since(began); d > time.Second {
			t.Errorf("write %d took %v: the rename waited for the writer's own handle", i, d)
		}

		back, err := openKeyRing(path, aid, now)
		if err != nil {
			t.Fatalf("write %d: read back: %v", i, err)
		}
		if back.lastSeq != uint64(i) || len(back.keys) != i {
			t.Fatalf("write %d: read back last_seq %d and %d keys", i, back.lastSeq, len(back.keys))
		}
		for j, k := range back.keys {
			if !bytes.Equal(k.Pair.Private, r.keys[j].Pair.Private) {
				t.Fatalf("write %d: key %d differs from the one written", i, j)
			}
		}

		if link != "" {
			got, err := os.ReadFile(link)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(oldBytes) || bytes.Count(got, []byte{0}) != len(got) {
				t.Errorf("write %d: the replaced ring file was not overwritten: %d bytes, %d zero",
					i, len(got), bytes.Count(got, []byte{0}))
			}
		}
		if _, err := os.Lstat(path + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("write %d: the temporary file remains: %v", i, err)
		}
	}
	if len(noted) != 0 {
		t.Errorf("failures counted: %v", noted)
	}
}
