//go:build !unix

package main

// sandboxSubcommand: there is no sandbox on this platform, so there are no internal sandbox commands.
func sandboxSubcommand(args []string) (int, bool) { return 0, false }
