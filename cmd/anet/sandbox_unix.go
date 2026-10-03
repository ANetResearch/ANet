//go:build unix

package main

import "github.com/ANetResearch/ANet/internal/daemon"

// sandboxSubcommand runs the internal commands a sandbox starts this binary as (LEC SB-5, SB-2):
// `anet __sandbox-init` and `anet __sandbox-probe`. They are dispatched before anything else reads the
// command line or the environment, and no help lists them (CL-8).
func sandboxSubcommand(args []string) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "__sandbox-init":
		return daemon.SandboxInitMain(args[1:]), true
	case "__sandbox-probe":
		return daemon.SandboxProbeMain(args[1:]), true
	}
	return 0, false
}
