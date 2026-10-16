package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// The identity file, the key state notice and hub_join.json go through writeFileAtomic, and after a
// power cut the new name must not point at an empty or old file: the bytes are synced before the
// rename (KEL §2.9, D-35; T-1 RT-F2).
func TestWriteFileAtomicSyncsBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.kel")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var synced, atRename int
	prev := atomicSync
	atomicSync = func(f *os.File) error {
		synced++
		if b, _ := os.ReadFile(path); string(b) != "old" {
			t.Errorf("the name already points at %q while the file is synced", b)
		}
		if st, err := f.Stat(); err != nil || st.Size() != int64(len("new contents")) {
			t.Errorf("synced before the bytes were written: %v", err)
		}
		return prev(f)
	}
	t.Cleanup(func() { atomicSync = prev })
	if err := writeFileAtomic(path, []byte("new contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	atRename = synced
	if atRename != 1 {
		t.Fatalf("the file was synced %d times, want once", atRename)
	}
	if b, _ := os.ReadFile(path); string(b) != "new contents" {
		t.Fatalf("file holds %q", b)
	}
}
