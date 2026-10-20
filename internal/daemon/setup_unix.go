//go:build !windows

package daemon

import (
	"fmt"
	"os"
)

// narrowConfigMode narrows config.json to 0600 when its mode grants group or other access, and reports
// the change.
func narrowConfigMode(path string) (InitChange, bool) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err == nil {
			return InitChange{Key: "config.json", Action: "mode",
				Note: fmt.Sprintf("%04o → 0600", fi.Mode().Perm())}, true
		}
	}
	return InitChange{}, false
}
