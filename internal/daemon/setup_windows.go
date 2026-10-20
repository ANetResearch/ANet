//go:build windows

package daemon

import "github.com/ANetResearch/ANet/internal/fsperm"

// narrowConfigMode gives config.json the private access control list (this user, SYSTEM,
// Administrators) when its access control list grants any other account access, and reports the
// change. A private config.json is left alone, so the change is reported only when one was made.
func narrowConfigMode(path string) (InitChange, bool) {
	info, err := fsperm.Inspect(path)
	if err != nil || !info.OthersRead {
		return InitChange{}, false
	}
	if err := fsperm.Protect(path); err != nil {
		return InitChange{}, false
	}
	return InitChange{Key: "config.json", Action: "mode", Note: "not private → private"}, true
}
