//go:build !windows

package autoreply

import "path/filepath"

// agentBinIs reports whether the agent binary bin is the program name (its base name, exactly).
func agentBinIs(bin, name string) bool { return filepath.Base(bin) == name }
