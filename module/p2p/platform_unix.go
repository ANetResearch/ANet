//go:build !no_p2p && !windows

package p2p

// platformRefusal is nil where the module can check its peer process's listener.
func platformRefusal() error { return nil }
