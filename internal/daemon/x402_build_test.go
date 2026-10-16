package daemon

import (
	"slices"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// x402Compiled says whether this test binary has the payment module (a build
// with -tags no_x402 leaves it out).
func x402Compiled() bool { return slices.Contains(module.Compiled(), "x402") }

// needX402 skips a test that stands on the payment module being there.
func needX402(t *testing.T) {
	t.Helper()
	if !x402Compiled() {
		t.Skip("this build leaves out the payment module (-tags no_x402)")
	}
}
