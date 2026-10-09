//go:build !linux

package daemon

import "testing"

// noSandbox: there is no sandbox on this platform yet, so every sandboxed run is refused, without a
// local reason code.
func noSandbox(t *testing.T) string { return "" }
