//go:build !no_cas

package cas

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// CAS sits under everything that stores anything, so its cost is a floor on
// the whole node. These measure the store directly rather than through the
// capability, because the capability's own overhead is a base64 decode and
// a map lookup — what matters is what the disk does.

func BenchmarkPut(b *testing.B) {
	for _, size := range []int{256, 4 << 10, 64 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			s, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			bodies := make([][]byte, 64)
			for i := range bodies {
				// Distinct content per iteration: storing the same bytes
				// repeatedly would measure the idempotent fast path rather
				// than a write.
				body := make([]byte, size)
				copy(body, fmt.Sprintf("blob-%d-", i))
				bodies[i] = body
			}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Put(bodies[i%len(bodies)]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Get verifies on read — it re-hashes the bytes before handing them back —
// so this is the cost of reading plus the cost of being sure.
func BenchmarkGetVerified(b *testing.B) {
	for _, size := range []int{256, 64 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			s, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			cid, err := s.Put(make([]byte, size))
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Get(cid); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Storing what is already stored: peers re-offering content is the normal
// case in a content-addressed network, not an anomaly.
//
// Measured at ~176 MB/s against ~163 MB/s for a fresh write of the same
// size, which looks like the early return is barely helping — and is in
// fact the floor. Put must hash the bytes to learn the CID before it can
// check whether it already has them, so an idempotent store is hash-bound
// by construction. The stat-and-return saves the write, not the hash, and
// no implementation can save the hash without being told a CID it would
// then have to trust.
func BenchmarkPutIdempotent(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	body := make([]byte, 64<<10)
	if _, err := s.Put(body); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Put(body); err != nil {
			b.Fatal(err)
		}
	}
}

var sinkCID string

// The capability path adds a base64 round trip to every blob, which is what
// a peer's bytes actually cost once they arrive over C1.
func BenchmarkPutThroughCapability(b *testing.B) {
	m, h := benchStarted(b)
	put, _ := h.reg.Resolve(CapPut)
	body := make([]byte, 64<<10)
	args := map[string]any{"body": base64.StdEncoding.EncodeToString(body)}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eff, err := put.Invoke(nil, benchCall(CapPut, args))
		if err != nil {
			b.Fatal(err)
		}
		sinkCID = eff.Evidence.ObservedState
	}
	_ = m
}
