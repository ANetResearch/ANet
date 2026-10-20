//go:build !windows

package loopguard

// addrInUseOS adds nothing outside Windows: EADDRINUSE is the one code a held address returns.
func addrInUseOS(error) bool { return false }
