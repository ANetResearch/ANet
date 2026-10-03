package main

// removed.go refuses the commands and flags 0.3.0 removed (ux §4.3) before
// anything else happens: the CLI prints cli_removed with what to do instead
// and exits 2, without reading the data directory or contacting the daemon.

import (
	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// removedCLI is one removed command, or one removed flag of a command.
type removedCLI struct {
	cmd  string // the verb as typed (aliases are listed on their own)
	flag string // "" for the whole command
	// what is {what}: the command as ux §4.3 names it.
	what string
	// instead is the id of its {instead} phrase (a2ashape, cli_removed).
	instead string
}

// removedCommands is ux §4.3.
var removedCommands []removedCLI

// refuseRemoved is the cli_removed sentence for an invocation that uses a
// removed command or flag, and true; "" and false for any other.
func refuseRemoved(cmd string, rest []string, lang a2ashape.Language) (string, bool) {
	return "", false // v030-todo(UX-5)
}
