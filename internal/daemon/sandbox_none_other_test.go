//go:build !linux && !darwin

package daemon

import "testing"

// noSandbox: there is no sandbox on this platform, so every sandboxed run is refused with
// platform_unsupported (LEC OP-1).
func noSandbox(t *testing.T) string { return sandboxCodePlatformUnsupported }
