package coredet

import "testing"

// C-R2 (_CONVENTIONS §2): the decoder MUST forbid CBOR tags. A tag-bearing byte
// sequence (here 0xc0 = tag 0, RFC 3339 date-time, wrapping a text string) MUST be
// rejected, not silently decoded.
func TestUnmarshalRejectsTags(t *testing.T) {
	// 0xc0 = major type 6, tag 0; 0x60 = empty text string. A well-formed tagged item.
	tagged := []byte{0xc0, 0x60}
	var v any
	if err := Unmarshal(tagged, &v); err == nil {
		t.Fatalf("Unmarshal of tag-bearing bytes must error (C-R2), got value %v", v)
	}
}

// Sanity: an untagged value still decodes.
func TestUnmarshalUntagged(t *testing.T) {
	b, err := Marshal(uint64(7))
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	if err := Unmarshal(b, &n); err != nil {
		t.Fatalf("untagged decode failed: %v", err)
	}
	if n != 7 {
		t.Fatalf("got %d want 7", n)
	}
}
