//go:build windows

package autoreply

import (
	"path/filepath"
	"strings"
)

// agentBinIs reports whether the agent binary bin is the program name: its base name without regard to
// case, after one .exe, .cmd or .bat extension is removed (cursor.cmd is cursor).
func agentBinIs(bin, name string) bool {
	b := strings.ToLower(filepath.Base(bin))
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		if t, ok := strings.CutSuffix(b, ext); ok {
			b = t
			break
		}
	}
	return b == strings.ToLower(name)
}
