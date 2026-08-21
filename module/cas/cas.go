// Package cas is the v3 runtime content-addressed store: it maps an anetcid CID
// to the exact bytes whose preimage hashes to that CID, and back.
//
// Normative source: design3/spec/_CONVENTIONS §3 (CID construction) — the CID of a
// stored blob is anetcid.Sum(blob), i.e. the multibase CIDv1 dag-cbor sha2-256 of the
// EXACT bytes handed to Put. The caller decides what those bytes are (a CoreDet-CBOR
// object preimage, a cogunit body, an artifact); CAS only guarantees CID↔bytes.
//
// Layering: this is the pure content layer (filesystem-backed: blobs/<cid>, atomic +
// idempotent writes, verify-on-read). Searchable artifact metadata (logical paths,
// names, FTS) is a SEPARATE concern owned by the layer that needs it (the org-store,
// Batch C5). CAS itself stays schema-free so both the org runtime and the p2p runtime
// reuse it unchanged.
//
// Follow-up (documented, not built): Put/Get are []byte-only. Large-artifact transfer
// will want streaming variants — PutStream(io.Reader)->CID (hash-on-the-fly into a temp)
// and Open(CID)->io.ReadCloser (verifying reader) — mirroring orgstore's PutStream so a
// blob never need be fully buffered. The []byte core stays the minimal primitive.
package cas

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ANetResearch/ANetCore/anetcid"
	cid "github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// ErrCorrupt is returned by Get when the on-disk blob's content no longer hashes to
// its CID (verify-on-read failure) — disk corruption or out-of-band tampering.
var ErrCorrupt = errors.New("cas: blob content does not match its CID (corrupt)")

// ErrNotFound is returned when a CID is absent from the store.
var ErrNotFound = errors.New("cas: blob not found")

const (
	blobsDir   = "blobs"
	tmpPattern = ".cas-*" // temp prefix; never enumerated as a blob (it starts with '.')
)

// Store is a handle to one content-addressed store directory. Safe for concurrent use:
// Put is atomic (temp + rename) and idempotent, so concurrent writers of identical
// bytes converge on the same blob without corruption.
type Store struct {
	dir string // root; blobs live under <dir>/blobs/
}

// Open opens (creating if needed) a content-addressed store rooted at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("cas: dir required")
	}
	if err := os.MkdirAll(filepath.Join(dir, blobsDir), 0o755); err != nil {
		return nil, fmt.Errorf("cas: mkdir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the store root.
func (s *Store) Dir() string { return s.dir }

// canonical validates a CID string and returns its canonical multibase form, which is
// the safe on-disk filename (base32-lower, no separators). It rejects anything that is
// not a well-formed CID — closing path-traversal and non-canonical-key aliasing.
func canonical(cidStr string) (string, error) {
	if cidStr == "" {
		return "", fmt.Errorf("cas: empty CID")
	}
	c, err := cid.Decode(cidStr)
	if err != nil {
		return "", fmt.Errorf("cas: bad CID %q: %w", cidStr, err)
	}
	// Restrict the on-disk keyspace to EXACTLY the suite CID space (_CONVENTIONS §3,
	// frozen prefix 0x01 0x71 0x12 0x20): CIDv1, dag-cbor, sha2-256. Anything else
	// (CIDv0 "Qm…", other codecs/hashes) is not a suite CID and is refused — anetcid.Sum
	// only ever emits this form, so no legitimate Put/Get is affected.
	p := c.Prefix()
	if p.Version != 1 || p.Codec != cid.DagCBOR || p.MhType != mh.SHA2_256 {
		return "", fmt.Errorf("cas: non-suite CID %q (want CIDv1 dag-cbor sha2-256)", cidStr)
	}
	canon := c.String()
	// Defence in depth: a canonical CIDv1 base32 string can never contain a path
	// separator or '.'; if it somehow does, refuse rather than touch the filesystem.
	if strings.ContainsAny(canon, "/\\.") {
		return "", fmt.Errorf("cas: unsafe CID %q", cidStr)
	}
	return canon, nil
}

func (s *Store) blobPath(canonCID string) string {
	return filepath.Join(s.dir, blobsDir, canonCID)
}

// Put writes body to the content-addressed layer and returns its CID. Idempotent:
// identical bytes → identical CID, written at most once. The write is atomic (temp +
// rename) so a reader never observes a partial blob.
func (s *Store) Put(body []byte) (string, error) {
	cidStr, err := anetcid.Sum(body)
	if err != nil {
		return "", fmt.Errorf("cas: cid: %w", err)
	}
	canon, err := canonical(cidStr)
	if err != nil {
		return "", err
	}
	final := s.blobPath(canon)
	if _, err := os.Stat(final); err == nil {
		return canon, nil // already present (idempotent)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("cas: stat: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, blobsDir), tmpPattern)
	if err != nil {
		return "", fmt.Errorf("cas: temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("cas: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("cas: close: %w", err)
	}
	// Rename is atomic and replaces any existing target with identical content, so a
	// concurrent writer that won the race is harmless.
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("cas: rename: %w", err)
	}
	return canon, nil
}

// Get returns the bytes for a CID, verifying on read that the content still hashes to
// the CID (verify-before-use). Returns ErrNotFound if absent, ErrCorrupt on mismatch.
func (s *Store) Get(cidStr string) ([]byte, error) {
	canon, err := canonical(cidStr)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(s.blobPath(canon))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("cas: read: %w", err)
	}
	got, err := anetcid.Sum(body)
	if err != nil {
		return nil, fmt.Errorf("cas: cid: %w", err)
	}
	if got != canon {
		return nil, ErrCorrupt
	}
	return body, nil
}

// Has reports whether a CID is present. A malformed CID is an error, not a false.
func (s *Store) Has(cidStr string) (bool, error) {
	canon, err := canonical(cidStr)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(s.blobPath(canon))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("cas: stat: %w", err)
}

// Stat returns the byte size of a stored blob, or ErrNotFound if absent.
func (s *Store) Stat(cidStr string) (int64, error) {
	canon, err := canonical(cidStr)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(s.blobPath(canon))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("cas: stat: %w", err)
	}
	return fi.Size(), nil
}

// Delete removes a blob. Idempotent: deleting an absent CID is not an error (so GC can
// run repeatedly). Returns an error only on a real filesystem failure or bad CID.
func (s *Store) Delete(cidStr string) error {
	canon, err := canonical(cidStr)
	if err != nil {
		return err
	}
	if err := os.Remove(s.blobPath(canon)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cas: remove: %w", err)
	}
	return nil
}

// List enumerates the CIDs currently stored. In-progress temp files (".cas-*") are
// never returned. Order is unspecified.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, blobsDir))
	if err != nil {
		return nil, fmt.Errorf("cas: readdir: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}
