//go:build !unix || no_autoreply

package main

import (
	"fmt"
	"os"
)

// sandboxSubcommand: there is no sandbox on this platform or in this build (no_autoreply), so the internal
// sandbox commands are unknown ones: exit 2 at once, without looking for a daemon (11-modularity, MOD-4
// acceptance 5). A daemon of another build that starts this binary as the sandbox's init gets that exit and
// answers sandbox_start_failed.
func sandboxSubcommand(args []string) (int, bool) {
	if len(args) > 0 && (args[0] == "__sandbox-init" || args[0] == "__sandbox-probe") {
		fmt.Fprintln(os.Stderr, "anet: internal command, not part of this build")
		return 2, true
	}
	return 0, false
}
