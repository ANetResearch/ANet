//go:build !no_p2p

package daemon

// The p2p module is linked into this package's tests: the kernel's decisions
// about it (p2pAdvertise) depend on it being compiled in, and the tests that
// show it absent do so with `without`, not by leaving the import out.
import _ "github.com/ANetResearch/ANet/module/p2p"
