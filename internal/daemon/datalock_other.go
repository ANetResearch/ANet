//go:build !unix && !windows

package daemon

import "os"

// lockData is not available on this platform: the daemon runs without the data directory lock, and
// the CLI's offline branches rely on their probe of the control plane alone.
func lockData(f *os.File) error { return nil }
