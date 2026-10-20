//go:build windows

package autoreply

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// agentBinIs reports whether the agent binary bin is the program name: its base name without regard to
// case, after one .exe, .cmd or .bat extension is removed (cursor.cmd is cursor).
func agentBinIs(bin, name string) bool {
	b := strings.ToLower(filepath.Base(bin))
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		if t, ok := strings.CutSuffix(b, ext); ok {
			b = t
			break
		}
	}
	return b == strings.ToLower(name)
}

// errBatchAgent is refuseBatchAgent's error.
var errBatchAgent = fmt.Errorf("the agent program is a batch file (.cmd or .bat), which Windows runs " +
	"through cmd.exe: cmd.exe ends the command line at the first line break and interprets \" %% & | < > ^ " +
	"in the arguments, so the instructions would not reach the agent intact; install the agent's native " +
	"executable (.exe), or set auto_reply.command to the path of that executable")

// refuseBatchAgent refuses an agent program that is a batch file, such as the claude.cmd or codex.cmd
// launchers npm installs. bin is resolved as exec.Command resolves it (a name through PATH and PATHEXT,
// a path without an extension through PATHEXT), and the program found is refused when its extension is
// .cmd or .bat, in any case. A program that cannot be found is left to the start itself to report.
func refuseBatchAgent(bin string) error {
	p := bin
	if found, err := exec.LookPath(bin); err == nil || found != "" {
		p = found
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".cmd", ".bat":
		return fmt.Errorf("%w: %s", errBatchAgent, p)
	}
	return nil
}
