//go:build !no_p2p && windows

package p2p

import "errors"

// platformRefusal refuses the module on Windows: it reaches its peer process over a Unix socket, and on
// Windows anet cannot verify which account listens on one (internal/peercred), so every connection
// would be refused before anything was sent.
func platformRefusal() error {
	return errors.New("the p2p module is not available on Windows, because it reaches its peer process over a Unix socket " +
		"and anet cannot verify which account listens on one there; remove modules.p2p (delivery then goes through the hub)")
}
