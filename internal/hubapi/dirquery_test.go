package hubapi_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/dirbox"
	"github.com/ANetResearch/ANetCore/wire"
	"github.com/ANetResearch/ANetCore/wire/scan"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// SSR-D15a, M38: a query names at most DirQueryMaxBits bits of a hash,
// whatever dir_max_bits the hub reports; the small-bucket cut only ever
// makes it wider; nothing below the whole directory.
func TestDirQueryBits(t *testing.T) {
	if hubapi.DirQueryMaxBits != 10 || hubapi.DirMinBucket != 32 {
		t.Fatalf("DirQueryMaxBits %d, DirMinBucket %d; want 10 and 32 (sealed-sender §2.12, SSR-W18)",
			hubapi.DirQueryMaxBits, hubapi.DirMinBucket)
	}
	for _, c := range []struct{ maxBits, cut, want int }{
		{0, 0, 0}, {1, 0, 1}, {9, 0, 9}, {10, 0, 10}, {11, 0, 10}, {24, 0, 10}, {256, 0, 10}, {1 << 30, 0, 10},
		{24, 1, 9}, {24, 10, 0}, {24, 11, 0}, {4, 2, 2}, {-1, 0, 0}, {10, -5, 10},
	} {
		if got := hubapi.DirQueryBits(c.maxBits, c.cut); got != c.want {
			t.Errorf("DirQueryBits(%d, %d) = %d, want %d", c.maxBits, c.cut, got, c.want)
		}
	}
}

// A bucket query carries the prefix of the hash and nothing more: the
// first ceil(bits/8) bytes, the bits past bits zero; never the AID, never
// the hash. Wider than DirQueryMaxBits is refused here, before anything is
// sent.
func TestBucketQueryNamesAPrefixOnly(t *testing.T) {
	_, _, peer := testAIDs(t)
	h, err := dirbox.Hash(peer)
	if err != nil {
		t.Fatal(err)
	}
	for bits := 0; bits <= hubapi.DirQueryMaxBits; bits++ {
		q, gotH, err := hubapi.BucketQuery(peer, bits)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotH, h) || q.Bits != bits || q.Cursor != "" {
			t.Fatalf("bits %d: %+v", bits, q)
		}
		p, err := hubapi.DecodeB64(q.Prefix, -1)
		if err != nil || dirbox.CheckPrefix(p, bits) != nil || !dirbox.InBucket(h, p, bits) {
			t.Fatalf("bits %d: prefix %q (%v)", bits, q.Prefix, err)
		}
		body, _ := json.Marshal(q)
		if strings.Contains(string(body), peer) || strings.Contains(string(body), hubapi.B64(h)) {
			t.Fatalf("bits %d: the query names the agent: %s", bits, body)
		}
	}
	for _, bits := range []int{-1, hubapi.DirQueryMaxBits + 1, dirbox.HashBits} {
		if _, _, err := hubapi.BucketQuery(peer, bits); err == nil {
			t.Errorf("a query of %d bits was made", bits)
		}
	}
}

// The bucket bodies have exactly the members the registry gives POST
// /directory:bucket (core §2.12.2).
func TestTheBucketBodiesAreTheRegistrys(t *testing.T) {
	r, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	ep := r.Endpoint("POST", hubapi.DirectoryBucketPath)
	if ep == nil {
		t.Fatalf("POST %s is not a registered endpoint", hubapi.DirectoryBucketPath)
	}
	for _, c := range []struct {
		typ  reflect.Type
		resp bool
	}{
		{reflect.TypeFor[hubapi.DirBucketRequest](), false},
		{reflect.TypeFor[hubapi.DirBucketResponse](), true},
	} {
		body := ep.Request
		if c.resp {
			body = ep.Response
		}
		for _, d := range scan.JSONFields(c.typ, body) {
			t.Errorf("POST %s %s: %s", hubapi.DirectoryBucketPath, c.typ, d)
		}
	}
}

// DecodeBucketPage keeps the entries that can be in the bucket asked for
// and counts the rest: a hash of another length or outside the bucket, a
// value that is not strict base64url, a box larger than a directory
// carries. An answer that is not one is an error.
func TestDecodeBucketPage(t *testing.T) {
	_, _, peer := testAIDs(t)
	h, _ := dirbox.Hash(peer)
	const bits = 10
	prefix, _ := dirbox.Prefix(h, bits)
	other := bytes.Clone(h)
	other[0] ^= 0x80 // outside the bucket
	box := hubapi.B64(bytes.Repeat([]byte{1}, 40))
	answer := hubapi.DirBucketResponse{MaxBits: 12, Next: "n", Partial: true, Entries: []hubapi.DirBucketEntry{
		{H: hubapi.B64(h), Box: box},
		{H: hubapi.B64(other), Box: box},
		{H: hubapi.B64(h[:31]), Box: box},
		{H: hubapi.B64(h) + "=", Box: box},
		{H: hubapi.B64(h), Box: "+/"},
		{H: hubapi.B64(h), Box: hubapi.B64(make([]byte, dirbox.MaxBoxLen+1))},
	}}
	body, _ := json.Marshal(answer)
	p, err := hubapi.DecodeBucketPage(body, prefix, bits)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 || !bytes.Equal(p.Entries[0].H, h) || p.Skipped != 5 || p.MaxBits != 12 || p.Next != "n" || !p.Partial {
		t.Fatalf("page %+v", p)
	}
	for _, bad := range []string{``, `[]`, `{"max_bits":-1,"entries":[]}`, `{"max_bits":257,"entries":[]}`, `{"entries":"x"}`} {
		if _, err := hubapi.DecodeBucketPage([]byte(bad), prefix, bits); err == nil {
			t.Errorf("%q was read as a bucket answer", bad)
		}
	}
	// OpenEntries opens what is the agent's and counts what does not open.
	e := &dirbox.DirEntry{AID: peer, Home: peer, KEL: []byte{1}, Keys: []byte{2}}
	sealed, err := dirbox.Seal(peer, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	opened, failed := hubapi.OpenEntries(peer, h, []hubapi.BucketEntry{{H: h, Box: sealed}, {H: h, Box: []byte("no")}, {H: other, Box: sealed}})
	if len(opened) != 1 || opened[0].AID != peer || failed != 1 {
		t.Fatalf("opened %d, failed %d", len(opened), failed)
	}
}

// FuzzDecodeBucketPage: whatever a hub answers to a bucket query,
// DecodeBucketPage does not panic, and every entry it keeps is a 32-byte
// hash inside the bucket with a box a directory may carry.
func FuzzDecodeBucketPage(f *testing.F) {
	_, _, peer := testAIDs(f)
	h, _ := dirbox.Hash(peer)
	box := hubapi.B64([]byte("a box"))
	for _, s := range []string{
		`{"max_bits":0,"entries":[]}`,
		`{"max_bits":10,"entries":[{"h":"` + hubapi.B64(h) + `","box":"` + box + `"}],"next":"x","partial":true}`,
		`{"max_bits":10,"entries":[{"h":"` + hubapi.B64(h[:16]) + `","box":"` + box + `"}]}`,
		`{"max_bits":300}`, `null`, `{"entries":[{}]}`,
	} {
		f.Add([]byte(s), uint8(10))
		f.Add([]byte(s), uint8(0))
	}
	f.Fuzz(func(t *testing.T, body []byte, b uint8) {
		bits := int(b) % (hubapi.DirQueryMaxBits + 1)
		prefix, err := dirbox.Prefix(h, bits)
		if err != nil {
			t.Fatal(err)
		}
		p, err := hubapi.DecodeBucketPage(body, prefix, bits)
		if err != nil {
			return
		}
		if p.MaxBits < 0 || p.MaxBits > dirbox.HashBits {
			t.Fatalf("max_bits %d kept", p.MaxBits)
		}
		for _, e := range p.Entries {
			if len(e.H) != dirbox.HashLen || !dirbox.InBucket(e.H, prefix, bits) || len(e.Box) > dirbox.MaxBoxLen {
				t.Fatalf("an entry kept that cannot be in the bucket: %x (%d-byte box)", e.H, len(e.Box))
			}
		}
		hubapi.OpenEntries(peer, h, p.Entries)
	})
}
