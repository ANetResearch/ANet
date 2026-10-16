package hubapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ANetResearch/ANetCore/dirbox"
)

// The directory query (sealed-sender §2.9, §4.3.7 SSR-D14, SSR-D15,
// SSR-D15a; core §2.12.2).
//
// A node never asks a hub about another agent by its AID, nor by the
// agent's full directory hash h: it asks POST /directory:bucket for every
// entry whose h starts with a short prefix of the h it wants, a bucket,
// and opens the one entry it can (dirbox.Open). The hub learns a bucket,
// never the agent. How wide the bucket is the hub reports (dir_max_bits,
// at most as wide as keeps 32 entries in every bucket, SSR-W18), and the
// node caps it: whatever the hub says, a query names at most
// DirQueryMaxBits bits of the hash it is after. The daemon and the CLI
// (anet verify without a running daemon) both take the width from
// DirQueryBits and from nothing else.

// DirectoryBucketPath is POST /directory:bucket, on the anonymous side.
const DirectoryBucketPath = "/directory:bucket"

// DirQueryMaxBits is the most bits of a directory hash a query names (at
// most 2^10 buckets): what this node can guarantee alone about what a
// query tells the hub, whatever the hub reports (SSR-D15a).
const DirQueryMaxBits = 10

// DirMinBucket is the fewest entries a bucket narrower than the whole
// directory holds at a hub that keeps SSR-W18. A smaller one makes the
// daemon widen its queries to that hub (SSR-D15a).
const DirMinBucket = 32

// DirQueryMaxPages bounds the pages one bucket query reads (each at most
// 8 MiB at an honest hub): a bucket longer than that is not read to its
// end, and the query fails rather than conclude anything from a part.
const DirQueryMaxPages = 64

// DirQueryBits is the width of a directory query in bits: the hub's
// dir_max_bits, capped at DirQueryMaxBits, less cut, the bits this node
// took off for the hub's small buckets (the CLI's cut is 0); never below
// 0, the whole directory. The only way a query's width is computed.
func DirQueryBits(dirMaxBits, cut int) int {
	return max(0, min(max(dirMaxBits, 0), DirQueryMaxBits)-max(cut, 0))
}

// DirBucketRequest is the POST /directory:bucket body: the first
// ceil(bits/8) bytes of a directory hash with the bits past bits set to
// zero (base64url), bits, and the next of the previous page of the same
// bucket. Mirrors ANetHub internal/aghub/directory.go.
type DirBucketRequest struct {
	Prefix string `json:"prefix"`
	Bits   int    `json:"bits"`
	Cursor string `json:"cursor,omitempty"`
}

// DirBucketEntry is one entry of a bucket: the directory hash h and the
// box, both base64url.
type DirBucketEntry struct {
	H   string `json:"h"`
	Box string `json:"box"`
}

// DirBucketResponse is the /directory:bucket answer: the hub's
// dir_max_bits, one page of the bucket, the cursor of the next page, and
// partial, set when a peer hub's part of the bucket is missing from it — a
// reader must not take an agent missing from a partial bucket for "no such
// agent" (SSR-W21).
type DirBucketResponse struct {
	MaxBits int              `json:"max_bits"`
	Entries []DirBucketEntry `json:"entries"`
	Next    string           `json:"next,omitempty"`
	Partial bool             `json:"partial,omitempty"`
}

// BucketQuery returns the first request of aid's bucket at bits bits and
// aid's directory hash h, which stays with the caller: only the prefix
// goes to the hub.
func BucketQuery(aid string, bits int) (DirBucketRequest, []byte, error) {
	if bits < 0 || bits > DirQueryMaxBits {
		return DirBucketRequest{}, nil, fmt.Errorf("hubapi: a directory query of %d bits; at most %d", bits, DirQueryMaxBits)
	}
	h, err := dirbox.Hash(aid)
	if err != nil {
		return DirBucketRequest{}, nil, err
	}
	prefix, err := dirbox.Prefix(h, bits)
	if err != nil {
		return DirBucketRequest{}, nil, err
	}
	return DirBucketRequest{Prefix: B64(prefix), Bits: bits}, h, nil
}

// BucketEntry is one decoded entry of a bucket page.
type BucketEntry struct {
	H, Box []byte
}

// BucketPage is one page of a bucket answer, decoded: the entries that are
// well formed and in the bucket asked for, how many were not (Skipped),
// and the rest of the answer as it came.
type BucketPage struct {
	MaxBits int
	Entries []BucketEntry
	Skipped int
	Next    string
	Partial bool
}

// ErrBucketAnswer is a bucket answer that is not one: not JSON of the
// shape, or a max_bits outside 0..256.
var ErrBucketAnswer = errors.New("hubapi: not a directory bucket answer")

// DecodeBucketPage reads one page of the answer to the bucket (prefix,
// bits). An entry whose h is not 32 bytes of strict base64url inside the
// bucket, or whose box is not strict base64url of at most
// dirbox.MaxBoxLen bytes, is left out and counted in Skipped: it cannot be
// any agent's entry of this bucket, and it does not count towards the
// bucket's size either.
func DecodeBucketPage(body []byte, prefix []byte, bits int) (BucketPage, error) {
	var a DirBucketResponse
	if err := json.Unmarshal(body, &a); err != nil {
		return BucketPage{}, fmt.Errorf("%w: %v", ErrBucketAnswer, err)
	}
	if a.MaxBits < 0 || a.MaxBits > dirbox.HashBits {
		return BucketPage{}, fmt.Errorf("%w: max_bits %d", ErrBucketAnswer, a.MaxBits)
	}
	p := BucketPage{MaxBits: a.MaxBits, Next: a.Next, Partial: a.Partial}
	for _, e := range a.Entries {
		h, herr := DecodeB64(e.H, dirbox.HashLen)
		box, berr := DecodeB64(e.Box, -1)
		if herr != nil || berr != nil || len(box) > dirbox.MaxBoxLen || !dirbox.InBucket(h, prefix, bits) {
			p.Skipped++
			continue
		}
		p.Entries = append(p.Entries, BucketEntry{H: h, Box: box})
	}
	return p, nil
}

// OpenEntries opens the entries filed under h, aid's directory hash, that
// open under aid (dirbox.Open): what a bucket holds of aid. failed counts
// the entries filed under h that do not open, or open to something that is
// not a directory entry of aid; a reader skips them (core §2.8).
func OpenEntries(aid string, h []byte, entries []BucketEntry) (opened []*dirbox.DirEntry, failed int) {
	for _, e := range entries {
		if !bytes.Equal(e.H, h) {
			continue
		}
		de, err := dirbox.Open(aid, h, e.Box)
		if err != nil {
			failed++
			continue
		}
		opened = append(opened, de)
	}
	return opened, failed
}
