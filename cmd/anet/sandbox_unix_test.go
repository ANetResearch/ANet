//go:build unix

package main

import (
	"strings"
	"testing"
)

// LEC CL-8: the sandbox's internal commands run before anything else and no help mentions them.
func TestSandboxCommandsAreHidden(t *testing.T) {
	needModule(t, "autoreply")
	for name, text := range map[string]string{"help --all": usageAllText(), "banner": guideBanner()} {
		if strings.Contains(text, "__sandbox") {
			t.Errorf("%s mentions an internal sandbox command", name)
		}
	}
	for _, c := range []string{"__sandbox-init", "__sandbox-probe"} {
		if _, ok := knownFlags[c]; ok {
			t.Errorf("%s is listed among the commands", c)
		}
	}
	// Without its spec descriptor each is an internal command: exit 2.
	for _, c := range []string{"__sandbox-init", "__sandbox-probe"} {
		if code, ok := sandboxSubcommand([]string{c}); !ok || code != 2 {
			t.Errorf("%s without a spec: (%d, %v)", c, code, ok)
		}
	}
	if _, ok := sandboxSubcommand([]string{"status"}); ok {
		t.Error("an ordinary command was taken for a sandbox command")
	}
	if _, ok := sandboxSubcommand(nil); ok {
		t.Error("no arguments were taken for a sandbox command")
	}
}
