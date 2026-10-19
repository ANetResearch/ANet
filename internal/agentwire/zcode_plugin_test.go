//go:build !no_mcp

package agentwire

import (
	"os"
	"os/exec"
	"path/filepath"
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

// zcodeRun runs deploy/zcode/anet/bin/<script> with HOME set to home, PATH set
// to path and the extra environment env, and returns its output and status.
func zcodeRun(t *testing.T, script, home, path string, env []string, args ...string) (string, int) {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "deploy", "zcode", "anet", "bin", script))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", append([]string{abs}, args...)...)
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + path}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// fakeAnet writes an executable that prints its own path and its arguments.
func fakeAnet(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "anet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho \"$0 $*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Every anet command of the plugin goes through bin/anet-run.sh, so that the
// MCP server, /anet-status and the setup skill reach the same executable and
// the same identity.
func TestZCodePluginRunnerAppliesTheSettings(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed to read the ZCode configuration")
	}
	sysPath := "/usr/bin:/bin"
	writeConfig := func(t *testing.T, home, options string) {
		t.Helper()
		dir := filepath.Join(home, ".zcode", "cli")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := `{"provider": {"x": {"options": {"apiKey": "SECRET-KEY"}}}, "plugins": {"options": ` + options + `}}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the MCP server takes the settings from its environment", func(t *testing.T) {
		home := t.TempDir()
		bin := fakeAnet(t, filepath.Join(t.TempDir(), "custom"))
		fakeAnet(t, filepath.Join(home, ".local", "bin"))
		out, code := zcodeRun(t, "anet-mcp.sh", home, sysPath, []string{"ANET_PLUGIN_BIN=" + bin, "ANET_PLUGIN_IDENTITY=work"})
		if code != 0 || strings.TrimSpace(out) != bin+" --id work mcp" {
			t.Errorf("anet-mcp.sh: status %d, output %q; want %q", code, out, bin+" --id work mcp")
		}
	})

	t.Run("an invalid anetPath is an error and no other copy is used", func(t *testing.T) {
		home := t.TempDir()
		fakeAnet(t, filepath.Join(home, ".local", "bin"))
		missing := filepath.Join(home, "nowhere", "anet")
		out, code := zcodeRun(t, "anet-mcp.sh", home, sysPath, []string{"ANET_PLUGIN_BIN=" + missing, "ANET_PLUGIN_IDENTITY="})
		if code != 1 || !strings.Contains(out, "anetPath") || strings.Contains(out, " mcp") {
			t.Errorf("anet-mcp.sh with a missing anetPath: status %d, output %q", code, out)
		}
		writeConfig(t, home, `{"anet@agentnetwork": {"anetPath": "`+missing+`"}}`)
		out, _ = zcodeRun(t, "anet-run.sh", home, sysPath, nil, "--resolve")
		if !strings.Contains(out, "executable: invalid setting anetPath="+missing) {
			t.Errorf("--resolve with a missing anetPath: %q", out)
		}
	})

	t.Run("outside the MCP server the settings come from the ZCode configuration", func(t *testing.T) {
		home := t.TempDir()
		bin := fakeAnet(t, filepath.Join(t.TempDir(), "custom"))
		fakeAnet(t, filepath.Join(home, ".local", "bin"))
		writeConfig(t, home, `{"other@x": {"anetPath": "/bin/false"}, "anet@zcode-plugins-official": {"anetPath": "`+bin+`", "identity": "work"}}`)
		out, code := zcodeRun(t, "anet-run.sh", home, sysPath, nil, "status")
		if code != 0 || strings.TrimSpace(out) != bin+" --id work status" {
			t.Errorf("anet-run.sh status: status %d, output %q", code, out)
		}
		out, code = zcodeRun(t, "anet-run.sh", home, sysPath, nil, "--resolve")
		for _, want := range []string{"executable: " + bin, "identity: work", "runner: sh "} {
			if code != 0 || !strings.Contains(out, want) {
				t.Errorf("--resolve: status %d, output %q lacks %q", code, out, want)
			}
		}
		if strings.Contains(out, "SECRET-KEY") {
			t.Error("--resolve printed another value of the ZCode configuration")
		}
	})

	t.Run("without settings PATH and the install locations are searched", func(t *testing.T) {
		home := t.TempDir()
		local := fakeAnet(t, filepath.Join(home, ".local", "bin"))
		out, code := zcodeRun(t, "anet-run.sh", home, sysPath, nil, "status")
		if code != 0 || strings.TrimSpace(out) != local+" status" {
			t.Errorf("anet-run.sh status: status %d, output %q; want %q", code, out, local+" status")
		}
		onPath := fakeAnet(t, filepath.Join(t.TempDir(), "p"))
		out, _ = zcodeRun(t, "anet-run.sh", home, filepath.Dir(onPath)+":"+sysPath, nil, "status")
		if strings.TrimSpace(out) != onPath+" status" {
			t.Errorf("anet on PATH: output %q; want %q", out, onPath+" status")
		}
	})

	t.Run("two anet entries in the configuration are ambiguous and ignored", func(t *testing.T) {
		home := t.TempDir()
		local := fakeAnet(t, filepath.Join(home, ".local", "bin"))
		writeConfig(t, home, `{"anet@a": {"identity": "one"}, "anet@b": {"identity": "two"}}`)
		out, _ := zcodeRun(t, "anet-run.sh", home, sysPath, nil, "status")
		if strings.TrimSpace(out) != local+" status" {
			t.Errorf("output %q; want %q", out, local+" status")
		}
	})

	t.Run("nothing installed", func(t *testing.T) {
		out, code := zcodeRun(t, "anet-run.sh", t.TempDir(), sysPath, nil, "status")
		if code != 1 || !strings.Contains(out, "anet-setup") {
			t.Errorf("status %d, output %q", code, out)
		}
		out, code = zcodeRun(t, "anet-run.sh", t.TempDir(), sysPath, nil, "--resolve")
		if code != 0 || !strings.Contains(out, "executable: not found") {
			t.Errorf("--resolve: status %d, output %q", code, out)
		}
	})
}
