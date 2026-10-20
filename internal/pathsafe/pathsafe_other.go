//go:build !windows

package pathsafe

import (
	"path/filepath"
	"strings"
)

// osSafeName is SafeName's last, platform-specific step: nothing outside Windows.
func osSafeName(name string) string { return name }

func pathWithin(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
