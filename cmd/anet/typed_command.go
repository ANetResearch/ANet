package main

import (
	"os"
	"regexp"
	"strings"
)

// typed_command.go is how a sentence quotes the command the user typed, for the hints that ask them to
// run it again elsewhere (a terminal, another identity).

// commandLine is the command as the user typed it, after "anet"; tests replace it.
var commandLine = func() []string { return os.Args[1:] }

// shellSafe is an argument a shell takes as it is.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// typedCommand is the command line given, as a user would type it again: each argument quoted where a
// shell needs it, and the value of a key given on the command line left out.
func typedCommand(args []string) string {
	out := []string{"anet"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--api-key" && i+1 < len(args):
			out = append(out, a, "…")
			i++
			continue
		case strings.HasPrefix(a, "--api-key="):
			out = append(out, "--api-key=…")
			continue
		}
		if !shellSafe.MatchString(a) {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}
