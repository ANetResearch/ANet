package coredet

import "testing"

// A canonical CoreDet encoding (RFC 8949 §4.2) never carries duplicate map keys; a decoder that
// verifies the profile MUST reject them rather than silently keep last-wins.
func TestUnmarshalRejectsDuplicateMapKeys(t *testing.T) {
	// {1:1, 1:2} — map with a duplicate int key (a2 0101 0102).
	dup := []byte{0xa2, 0x01, 0x01, 0x01, 0x02}
	var v map[int]int
	if err := Unmarshal(dup, &v); err == nil {
		t.Fatalf("duplicate map key must be rejected, got %v", v)
	}
	// the same map without the duplicate decodes fine.
	ok := []byte{0xa1, 0x01, 0x01}
	if err := Unmarshal(ok, &v); err != nil {
		t.Fatalf("well-formed map must decode: %v", err)
	}
}
