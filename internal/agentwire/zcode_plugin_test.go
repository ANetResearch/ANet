//go:build !no_mcp

package agentwire

import (
	"os"
	"strings"
	"testing"
)

// The ZCode plugin (deploy/zcode/anet) ships the same operating guide as wire
// installs for Claude Code. Only its last section differs, because a plugin
// is repaired through its setup skill rather than through
// `anet agents wire --refresh`. The rest must not drift from skillMarkdown.
func TestZCodePluginCarriesTheWireGuide(t *testing.T) {
	const tail = "## When something is off"
	b, err := os.ReadFile("../../deploy/zcode/anet/skills/anet/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	plugin := string(b)
	i, j := strings.Index(skillMarkdown, tail), strings.Index(plugin, tail)
	if i < 0 || j < 0 {
		t.Fatalf("%q missing: guide %d, plugin %d", tail, i, j)
	}
	// UPDATE_ZCODE_GUIDE=1 go test -run TestZCodePlugin ./internal/agentwire
	// writes the guide into the plugin and keeps the plugin's last section.
	if os.Getenv("UPDATE_ZCODE_GUIDE") == "1" {
		plugin = strings.TrimLeft(skillMarkdown[:i], "\n") + plugin[j:]
		if err := os.WriteFile("../../deploy/zcode/anet/skills/anet/SKILL.md", []byte(plugin), 0o644); err != nil {
			t.Fatal(err)
		}
		j = strings.Index(plugin, tail)
	}
	if want, got := strings.TrimLeft(skillMarkdown[:i], "\n"), plugin[:j]; got != want {
		k := 0
		for k < len(want) && k < len(got) && want[k] == got[k] {
			k++
		}
		t.Errorf("deploy/zcode/anet/skills/anet/SKILL.md differs from skillMarkdown before %q at byte %d: guide %q, plugin %q; copy the guide again",
			tail, k, want[k:min(k+40, len(want))], got[k:min(k+40, len(got))])
	}
	if strings.Contains(plugin[j:], "agents wire") {
		t.Error("the plugin's last section names anet agents wire, which does not repair a plugin")
	}
}
