//go:build !no_p2p

package daemon

// haveP2P says whether this test build contains the p2p module: the
// pluggable CI rows (-tags no_p2p) run the same tests, and a test of the
// module's behaviour skips itself where the module is not there.
const haveP2P = true
