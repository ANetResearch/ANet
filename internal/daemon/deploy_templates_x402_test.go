//go:build !no_x402

package daemon

// The module the deploy templates configure. The kernel names none of
// them; this file registers it, so its blocks are checked by the same code
// the daemon runs (CUX-V10), in every build that contains it.
import _ "github.com/ANetResearch/ANet/module/x402"
