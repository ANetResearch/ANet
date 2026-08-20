package cas

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/ANetResearch/ANetCore/anetcid"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

// Put→Get round-trips, and the returned CID is exactly anetcid.Sum(body).
func TestPutGetRoundTrip(t *testing.T) {
	s := open(t)
	body := []byte("the canonical task contract bytes")
	cid, err := s.Put(body)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if want := anetcid.MustSum(body); cid != want {
		t.Fatalf("CID = %q, want anetcid.Sum = %q", cid, want)
	}
	got, err := s.Get(cid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, body)
	}
}

// Identical bytes are stored at most once; the CID is stable.
func TestPutIdempotent(t *testing.T) {
	s := open(t)
	body := []byte("dedup me")
	c1, err := s.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatalf("idempotent CID differs: %q vs %q", c1, c2)
	}
	names, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 {
		t.Fatalf("expected exactly 1 blob after duplicate Put, got %d: %v", len(names), names)
	}
}

// Distinct bodies → distinct CIDs and distinct blobs.
func TestPutDistinct(t *testing.T) {
	s := open(t)
	a, _ := s.Put([]byte("alpha"))
	b, _ := s.Put([]byte("beta"))
	if a == b {
		t.Fatal("distinct bodies collided on CID")
	}
	names, _ := s.List()
	if len(names) != 2 {
		t.Fatalf("want 2 blobs, got %d", len(names))
	}
}

func TestHasStatDelete(t *testing.T) {
	s := open(t)
	body := bytes.Repeat([]byte{0x5a}, 4096)
	cid, _ := s.Put(body)

	if ok, err := s.Has(cid); err != nil || !ok {
		t.Fatalf("Has present: ok=%v err=%v", ok, err)
	}
	if sz, err := s.Stat(cid); err != nil || sz != int64(len(body)) {
		t.Fatalf("Stat: size=%d err=%v (want %d)", sz, err, len(body))
	}
	if err := s.Delete(cid); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ok, err := s.Has(cid); err != nil || ok {
		t.Fatalf("Has after delete: ok=%v err=%v", ok, err)
	}
	// Delete is idempotent.
	if err := s.Delete(cid); err != nil {
		t.Fatalf("second delete must be nil, got %v", err)
	}
}

// Absent CID → ErrNotFound (a real CID that was never stored).
func TestGetMissing(t *testing.T) {
	s := open(t)
	never := anetcid.MustSum([]byte("never stored"))
	if _, err := s.Get(never); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.Stat(never); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat want ErrNotFound, got %v", err)
	}
}

// Malformed / unsafe CIDs are rejected without touching the filesystem (no traversal).
func TestBadCIDRejected(t *testing.T) {
	s := open(t)
	for _, bad := range []string{
		"",
		"not-a-cid",
		"../../etc/passwd",
		"blobs/x",
		"b../b",
	} {
		if _, err := s.Get(bad); err == nil {
			t.Fatalf("Get(%q) must error", bad)
		}
		if _, err := s.Has(bad); err == nil {
			t.Fatalf("Has(%q) must error", bad)
		}
	}
	// Path-traversal strings must never resolve to a real file even if some decoder is lenient.
	if _, err := s.Get("../../../../etc/passwd"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}

// Verify-on-read catches a corrupted blob (bytes no longer match the CID).
func TestVerifyOnReadCorrupt(t *testing.T) {
	s := open(t)
	body := []byte("integrity-protected payload")
	cid, _ := s.Put(body)

	// Tamper the on-disk blob out-of-band.
	canon, err := canonical(cid)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.dir, blobsDir, canon)
	if err := os.WriteFile(p, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(cid); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt on tampered blob, got %v", err)
	}
}

// A large body (1 MiB) round-trips and reports the right size.
func TestLargeBody(t *testing.T) {
	s := open(t)
	body := make([]byte, 1<<20)
	for i := range body {
		body[i] = byte(i * 7)
	}
	cid, err := s.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(cid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("1MiB round-trip mismatch")
	}
	if sz, _ := s.Stat(cid); sz != int64(len(body)) {
		t.Fatalf("size=%d want %d", sz, len(body))
	}
}

// Concurrent Put of identical and distinct bodies: no corruption, idempotent dedup,
// and no leftover temp files.
func TestConcurrentPut(t *testing.T) {
	s := open(t)
	const writers = 32
	shared := []byte("contended blob")
	wantShared := anetcid.MustSum(shared)

	var wg sync.WaitGroup
	cids := make([]string, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				c, err := s.Put(shared)
				if err != nil {
					t.Errorf("put shared: %v", err)
				}
				cids[i] = c
			} else {
				c, err := s.Put([]byte{byte(i), 'u', 'n', 'i', 'q'})
				if err != nil {
					t.Errorf("put uniq: %v", err)
				}
				cids[i] = c
			}
		}(i)
	}
	wg.Wait()

	// Every even writer agreed on the shared CID.
	for i := 0; i < writers; i += 2 {
		if cids[i] != wantShared {
			t.Fatalf("writer %d shared CID = %q want %q", i, cids[i], wantShared)
		}
	}
	// Shared blob is readable and intact.
	if got, err := s.Get(wantShared); err != nil || !bytes.Equal(got, shared) {
		t.Fatalf("shared blob read: err=%v", err)
	}
	// No leftover temp files.
	entries, _ := os.ReadDir(filepath.Join(s.dir, blobsDir))
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
	// Exactly 1 shared + (writers/2) unique blobs.
	names, _ := s.List()
	wantCount := 1 + writers/2
	uniq := map[string]bool{}
	for _, n := range names {
		uniq[n] = true
	}
	if len(uniq) != wantCount {
		sort.Strings(names)
		t.Fatalf("blob count = %d want %d: %v", len(uniq), wantCount, names)
	}
}

// Empty body produces a valid CID and round-trips (classic edge: empty preimage).
func TestEmptyBody(t *testing.T) {
	s := open(t)
	for _, body := range [][]byte{nil, {}} {
		cid, err := s.Put(body)
		if err != nil {
			t.Fatalf("put empty: %v", err)
		}
		if cid != anetcid.MustSum([]byte{}) {
			t.Fatalf("empty CID = %q, want %q", cid, anetcid.MustSum([]byte{}))
		}
		got, err := s.Get(cid)
		if err != nil {
			t.Fatalf("get empty: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("empty round-trip got %d bytes", len(got))
		}
	}
}

// A CIDv0 (non-suite) key is refused (keyspace restricted to CIDv1 dag-cbor sha2-256).
func TestNonSuiteCIDRejected(t *testing.T) {
	s := open(t)
	// A well-formed CIDv0 ("Qm…") decodes fine but is not a suite CID.
	const v0 = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
	if _, err := s.Has(v0); err == nil {
		t.Fatal("CIDv0 must be rejected as non-suite")
	}
	if _, err := s.Get(v0); err == nil {
		t.Fatal("CIDv0 Get must be rejected")
	}
}

// Reopening the same directory sees previously stored blobs (durability).
func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Open(dir)
	cid, _ := s1.Put([]byte("survives reopen"))

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s2.Has(cid); err != nil || !ok {
		t.Fatalf("blob lost across reopen: ok=%v err=%v", ok, err)
	}
}
