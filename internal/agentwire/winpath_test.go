//go:build !no_mcp

package agentwire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A Windows install puts anet under a profile whose name may hold a space,
// and every path has backslashes. The entries written into each tool's
// file must carry such paths exactly: JSON and YAML escape the backslash,
// TOML basic strings too, and nothing is split at the space. The paths are
// injected, so this runs on every platform.
const (
	winBin  = `C:\Users\A B\AppData\Local\Programs\anet\anet.exe`
	winData = `C:\Users\A B\.anet`
)

func winPathOptions(t *testing.T) Options {
	h := newHost(t)
	o := h.opts()
	// normalize is not called: filepath.IsAbs refuses a drive path off Windows.
	o.Bin, o.DataDir = winBin, winData
	return o
}

func TestGoldenEntriesDrivePath(t *testing.T) {
	o := winPathOptions(t)
	cursorWant, _ := marshalIndented(cursorTool{}.server(&o).want)
	opencodeWant, _ := marshalIndented(opencodeTool{}.server(&o).want)
	claudeWant, _ := marshalIndented(claudeWant(&o))
	workbuddyWant, _ := marshalIndented(workbuddyTool{}.server(&o).want)
	cases := map[string]string{
		"codex.toml": joinLines(codexBlock(&o)),
		"hermes-mcp.yaml": joinLines(indentLines(append(append([]string{hermesMCP.begin}, hermesMCPBody(&o)...),
			hermesMCP.markers.end), 2)),
		"cursor.json":    cursorWant,
		"opencode.json":  opencodeWant,
		"claude.json":    claudeWant,
		"workbuddy.json": workbuddyWant,
	}
	for name, got := range cases {
		path := filepath.Join("testdata", name+".winpath.golden")
		if *update {
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to create)", err)
		}
		if got != string(want) {
			t.Errorf("%s changed:\n--- want\n%s--- got\n%s", name, want, got)
		}
	}
}

// What each writer produces reads back as the same two paths.
func TestDrivePathRoundTrip(t *testing.T) {
	o := winPathOptions(t)

	for name, v := range map[string]any{
		"claude": claudeWant(&o), "cursor": cursorTool{}.server(&o).want, "workbuddy": workbuddyTool{}.server(&o).want,
	} {
		b, err := marshalNoEscape(v)
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		}
		if err := json.Unmarshal(b, &e); err != nil || e.Command != winBin || e.Env["ANET_DATA_DIR"] != winData {
			t.Fatalf("%s: %s: %+v %v", name, b, e, err)
		}
	}
	b, err := marshalNoEscape(opencodeTool{}.server(&o).want)
	if err != nil {
		t.Fatal(err)
	}
	var oc struct {
		Command     []string          `json:"command"`
		Environment map[string]string `json:"environment"`
	}
	if err := json.Unmarshal(b, &oc); err != nil || len(oc.Command) != 2 || oc.Command[0] != winBin ||
		oc.Environment["ANET_DATA_DIR"] != winData {
		t.Fatalf("opencode: %s: %+v %v", b, oc, err)
	}

	var cmd, data string
	for _, l := range codexBlock(&o) {
		if v, ok := strings.CutPrefix(l, "command = "); ok {
			cmd = tomlUnescape(strings.TrimSuffix(strings.TrimPrefix(v, `"`), `"`))
		}
		if v, ok := strings.CutPrefix(l, "env = { ANET_DATA_DIR = "); ok {
			data = tomlUnescape(strings.TrimSuffix(strings.TrimPrefix(v, `"`), `" }`))
		}
	}
	if cmd != winBin || data != winData {
		t.Fatalf("codex: %q %q", cmd, data)
	}

	cmd, data = "", ""
	for _, l := range hermesMCPBody(&o) {
		l = strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(l, "command: "); ok {
			cmd = yamlUnquote(v)
		}
		if v, ok := strings.CutPrefix(l, "ANET_DATA_DIR: "); ok {
			data = yamlUnquote(v)
		}
	}
	if cmd != winBin || data != winData {
		t.Fatalf("hermes: %q %q", cmd, data)
	}
}
