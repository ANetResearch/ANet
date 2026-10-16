//go:build no_autoreply

package main

import "github.com/ANetResearch/ANet/internal/daemon"

// The build has no auto-reply module. `anet autoreply` is refused before it reaches this point
// (requireModule in main.go); the function exists so that the command table still compiles.

// runAutoReply never runs: the module check has already refused the command.
func runAutoReply(c *client, rest []string) error {
	return requireModule(daemon.Layout{Root: c.dataDir}, "autoreply")
}

// agentChoices renders the coding agents the help text names.
func agentChoices() string { return fixedAgentChoices }
