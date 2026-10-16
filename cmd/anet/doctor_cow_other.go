//go:build !linux && !darwin

package main

// copyOnWriteFS is not looked into on this platform.
func copyOnWriteFS(string) bool { return false }
