//go:build !windows

package autoreply

import "path/filepath"

// agentBinIs reports whether the agent binary bin is the program name (its base name, exactly).
func agentBinIs(bin, name string) bool { return filepath.Base(bin) == name }

// refuseBatchAgent refuses an agent program that Windows would run through cmd.exe. Nothing on unix.
func refuseBatchAgent(string) error { return nil }
