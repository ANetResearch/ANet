//go:build shell && !windows

package shell

import (
	"context"
	"os/exec"
)

// resolveInterpreter is the configured interpreter, or /bin/sh. It accepts every value: what the
// operator configures is run as configured.
func resolveInterpreter(configured string) (interp, error) {
	path := configured
	if path == "" {
		path = "/bin/sh"
	}
	return interp{path: path, family: familyPosix}, nil
}

// command is `<path> -c <line>`.
func (i interp) command(ctx context.Context, line string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, i.path, "-c", line), nil
}

// quote renders caller-supplied arguments for the command line (quoteArgv).
func (i interp) quote(argv any) (string, error) {
	return quoteArgv(argv)
}

// commandDir is the working directory of a command: dir as configured; empty is the daemon's own.
func commandDir(dir string) (string, error) {
	return dir, nil
}
