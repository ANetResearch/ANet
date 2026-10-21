//go:build !no_mcp

package agentwire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The tests of this package use the Linux layout of Trae unless a test sets
// another; the Windows and macOS layouts are checked by
// TestTraeUserDataPerOS, which computes paths only.
func init() { traeGOOS = "linux" }

func setTraeGOOS(t *testing.T, goos string) {
	t.Helper()
	old := traeGOOS
	traeGOOS = goos
	t.Cleanup(func() { traeGOOS = old })
}

func TestTraeUserDataPerOS(t *testing.T) {
	h := newHost(t)
	o := h.opts()
	cases := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"linux", nil, filepath.Join(h.home, ".config", "Trae", "User", "mcp.json")},
		{"linux", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, filepath.Join("/xdg", "Trae", "User", "mcp.json")},
		{"linux", map[string]string{"XDG_CONFIG_HOME": "relative"}, filepath.Join(h.home, ".config", "Trae", "User", "mcp.json")},
		{"darwin", nil, filepath.Join(h.home, "Library", "Application Support", "Trae", "User", "mcp.json")},
		{"windows", map[string]string{"APPDATA": `C:\Users\A B\AppData\Roaming`},
			filepath.Join(`C:\Users\A B\AppData\Roaming`, "Trae", "User", "mcp.json")},
		{"windows", map[string]string{"APPDATA": `\\server\profiles\ab`}, filepath.Join(`\\server\profiles\ab`, "Trae", "User", "mcp.json")},
		{"windows", map[string]string{"APPDATA": `Roaming`}, filepath.Join(h.home, "AppData", "Roaming", "Trae", "User", "mcp.json")},
		{"windows", nil, filepath.Join(h.home, "AppData", "Roaming", "Trae", "User", "mcp.json")},
	}
	for _, c := range cases {
		setTraeGOOS(t, c.goos)
		h.env = map[string]string{}
		for k, v := range c.env {
			h.env[k] = v
		}
		if got := traeMCP(&o, traeProducts[0]); got != c.want {
			t.Errorf("%s %v: %s, want %s", c.goos, c.env, got, c.want)
		}
	}
	setTraeGOOS(t, "windows")
	h.env = map[string]string{"APPDATA": `C:\Users\A B\AppData\Roaming`}
	if got, want := traeMCP(&o, traeProducts[1]), filepath.Join(`C:\Users\A B\AppData\Roaming`, "Trae CN", "User", "mcp.json"); got != want {
		t.Errorf("Trae CN: %s, want %s", got, want)
	}
	if got, want := traeRulePath(&o, traeProducts[1]), filepath.Join(h.home, ".trae-cn", "user_rules", "anet.md"); got != want {
		t.Errorf("Trae CN rule: %s, want %s", got, want)
	}
}

// Trae and Trae CN on one machine: both are wired, and unwire takes out both.
func TestTraeBothProducts(t *testing.T) {
	h := newHost(t)
	h.write(".trae/skills/other/SKILL.md", "---\nname: other\n---\n", 0o644)
	h.write(".trae-cn/user_rules/mine.md", "Answer in Chinese.\n", 0o644)
	orig := h.snapshot()
	r := one(t, h.wire(ToolTrae), Written)
	if len(r.Changes) != 4 || len(r.Backups) != 0 {
		t.Fatalf("changes %v, backups %v", r.Changes, r.Backups)
	}
	for _, rel := range []string{".config/Trae/User/mcp.json", ".config/Trae CN/User/mcp.json",
		".trae/user_rules/anet.md", ".trae-cn/user_rules/anet.md"} {
		if !h.exists(rel) {
			t.Fatalf("wire did not write %s", rel)
		}
	}
	one(t, h.wire(ToolTrae), UpToDate)
	one(t, h.unwire(ToolTrae), Removed)
	if got := h.snapshot(); !reflect.DeepEqual(got, orig) {
		t.Fatalf("unwire did not restore the files: %v", keys(got))
	}
	if h.exists(".trae/user_rules") {
		t.Fatal("unwire left the user_rules directory that wire created")
	}
	if !h.exists(".trae-cn/user_rules/mine.md") {
		t.Fatal("unwire removed a rule of the user")
	}
}

// The entry of the plugin bundle for Trae starts /bin/sh with its launcher;
// wire does not replace it, and nothing is written.
func TestTraeBundleEntryIsAConflict(t *testing.T) {
	h := newHost(t)
	bundle := `{"mcpServers":{"anet":{"command":"/bin/sh","args":["/home/u/.trae/anet/bin/anet-mcp.sh"],"env":{"RUN_MCP_TIMEOUT_MS":"900000"}}}}` + "\n"
	h.write(".config/Trae/User/mcp.json", bundle, 0o644)
	before := h.snapshot()
	r := one(t, h.wire(ToolTrae), Conflict)
	if m, _ := AsMsg(r.Err); m.Key != "wire_conflict" {
		t.Fatalf("err %v", r.Err)
	}
	if got := h.snapshot(); len(got) != len(before) || got[".config/Trae/User/mcp.json"] != bundle {
		t.Fatal("a conflict changed files")
	}
	r = one(t, h.unwire(ToolTrae), NotWired)
	if !strings.Contains(notesText(r.Notes), "was not written by anet") {
		t.Fatalf("notes %v", r.Notes)
	}
}

func TestTraeForeignRuleFileIsAConflict(t *testing.T) {
	h := newHost(t)
	h.write(".trae/user_rules/anet.md", "My own notes about anet.\n", 0o644)
	r := one(t, h.wire(ToolTrae), Conflict)
	if m, _ := AsMsg(r.Err); m.Key != "wire_conflict.rule_file" {
		t.Fatalf("err %v", r.Err)
	}
	if h.exists(".config/Trae/User/mcp.json") {
		t.Fatal("a conflict wrote the MCP file")
	}
	one(t, h.unwire(ToolTrae), NotWired)
	if h.read(".trae/user_rules/anet.md") != "My own notes about anet.\n" {
		t.Fatal("unwire removed a rule anet did not write")
	}
}

// An earlier guide is replaced, and a rule the user edited is backed up.
func TestTraeRuleIsUpdatedWithABackup(t *testing.T) {
	h := newHost(t)
	h.write(".trae/user_rules/anet.md", "## AgentNetwork (anet)\n\nAn older text.\n", 0o644)
	r := one(t, h.wire(ToolTrae), Written)
	if len(r.Backups) != 1 || h.read(".trae/user_rules/anet.md") != traeRule {
		t.Fatalf("backups %v, rule:\n%s", r.Backups, h.read(".trae/user_rules/anet.md"))
	}
}

func TestTraeNotesAPathWithASpace(t *testing.T) {
	h := newHost(t)
	o := h.opts()
	o.Bin = "/opt/My Tools/anet"
	r := one(t, h.wireWith(o, ToolTrae), Written)
	if !strings.Contains(notesText(r.Notes), "must not contain one") {
		t.Fatalf("notes %v", r.Notes)
	}
	var f struct {
		MCPServers map[string]stdioEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(h.read(".config/Trae/User/mcp.json")), &f); err != nil || f.MCPServers["anet"].Command != o.Bin {
		t.Fatalf("entry %+v (%v)", f.MCPServers["anet"], err)
	}
}

// The Windows layout, written for real into a temporary directory that
// stands for %APPDATA% and %USERPROFILE%: the entry carries the drive path
// of anet exactly.
func TestTraeWindowsLayout(t *testing.T) {
	setTraeGOOS(t, "windows")
	h := newHost(t)
	appData := filepath.Join(h.home, "AppData", "Roaming")
	h.env["APPDATA"] = appData
	o := h.opts()
	o.Bin, o.DataDir = winBin, winData
	if err := os.MkdirAll(filepath.Join(appData, "Trae CN"), 0o755); err != nil {
		t.Fatal(err)
	}
	// normalize is not called: filepath.IsAbs refuses a drive path off Windows.
	changes, err := traeTool{}.planWire(&o)
	r := finish(&o, Result{Tool: ToolTrae}, changes, nil, err, Written, UpToDate)
	r.Notes = append(r.Notes, traeTool{}.wireNotes(&o, r.Status)...)
	if r.Status != Written || len(r.Changes) != 2 {
		t.Fatalf("status %s (%v), changes %v", r.Status, r.Err, r.Changes)
	}
	var f struct {
		MCPServers map[string]stdioEntry `json:"mcpServers"`
	}
	mustJSON(t, h.read("AppData/Roaming/Trae CN/User/mcp.json"), &f)
	if e := f.MCPServers["anet"]; e.Command != winBin || e.Env["ANET_DATA_DIR"] != winData {
		t.Fatalf("entry %+v", e)
	}
	if !h.exists(".trae-cn/user_rules/anet.md") || h.exists(".trae/user_rules/anet.md") {
		t.Fatal("only Trae CN is installed; only its rule is written")
	}
	if !strings.Contains(notesText(r.Notes), "must not contain one") {
		t.Fatalf("the drive path has a space; notes %v", r.Notes)
	}
	// Unwire recognises the entry by the base name of its command, which a
	// drive path has only on Windows (os_windows_test.go runs there); here
	// only the rule file is checked.
	un, notes, err := traeTool{}.planUnwire(&o)
	if r := finish(&o, Result{Tool: ToolTrae}, un, notes, err, Removed, NotWired); r.Status != Removed {
		t.Fatalf("unwire: %+v", r)
	}
	if h.exists(".trae-cn/user_rules/anet.md") {
		t.Fatal("unwire left the rule file")
	}
}

func TestTraeInspect(t *testing.T) {
	h := newHost(t)
	h.write(".trae/user_rules/mine.md", "x\n", 0o644)
	st := inspectOne(t, h, ToolTrae)
	if !st.Detected || st.Wired || st.Config != h.p(".config/Trae/User/mcp.json") {
		t.Fatalf("before wire: %+v", st)
	}
	one(t, h.wire(ToolTrae), Written)
	if st = inspectOne(t, h, ToolTrae); !st.Wired || !st.Current {
		t.Fatalf("after wire: %+v", st)
	}
}

func inspectOne(t *testing.T, h *host, tool string) State {
	t.Helper()
	states, err := Inspect(h.opts())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.Tool == tool {
			return s
		}
	}
	t.Fatalf("Inspect has no %s", tool)
	return State{}
}
