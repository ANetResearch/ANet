//go:build windows

package main

import (
	"fmt"
	"os"
)

// refuseUnsupportedPlatform ends anetpeer on Windows before it creates or listens on anything: the
// daemon wire is a Unix socket whose peer must be checked (peercred), which Windows cannot do.
func refuseUnsupportedPlatform() {
	fmt.Fprintln(os.Stderr, "anetpeer: not supported on Windows (it needs Unix socket peer credentials)")
	os.Exit(2)
}
