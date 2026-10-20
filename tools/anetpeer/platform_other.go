//go:build !windows

package main

// refuseUnsupportedPlatform: on unix, listenDaemon refuses a platform whose kernel cannot say who
// connects (peercred), with the reason.
func refuseUnsupportedPlatform() {}
