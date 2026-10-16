//go:build !no_x402

package x402

import (
	"math"
	"testing"
)

func TestAsInt64RefusesWhatItCannotRepresent(t *testing.T) {
	for _, v := range []any{uint64(1 << 63), uint64(math.MaxUint64), 1e19, -1e19, 1.5, "7", nil} {
		if n, err := asInt64(v); err == nil {
			t.Errorf("asInt64(%v) = %d, want an error", v, n)
		}
	}
	for v, want := range map[any]int64{
		int64(-5): -5, uint64(math.MaxInt64): math.MaxInt64, 3: 3, float64(42): 42,
	} {
		if n, err := asInt64(v); err != nil || n != want {
			t.Errorf("asInt64(%v) = %d, %v; want %d", v, n, err, want)
		}
	}
}
