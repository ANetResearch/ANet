//go:build !unix || no_autoreply

package main

// sandboxSubcommand: there is no sandbox on this platform or in this build (no_autoreply), so there are no internal sandbox commands.
func sandboxSubcommand(args []string) (int, bool) { return 0, false }
