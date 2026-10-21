//go:build !no_mcp

package agentwire

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const testZCode = "/usr/local/bin/zcode"

// fakeZCode stands for ZCode's command-line program: marketplaces and
// plugins in memory, the subcommands anet uses, and the JSON they print.
// No real ZCode runs and nothing under ~/.zcode is touched.
type fakeZCode struct {
	markets []string
	plugins map[string]bool // id → enabled
	// noop makes install, enable and uninstall report success and change
	// nothing; noise adds diagnostics around the JSON and exits with 1.
	noop, noise bool
}

func newFakeZCode() *fakeZCode { return &fakeZCode{plugins: map[string]bool{}} }

func (f *fakeZCode) run(name string, args []string) ([]byte, error) {
	if name != testZCode {
		return nil, errors.New("unexpected program " + name)
	}
	cmd := strings.Join(args, " ")
	switch {
	case cmd == "plugins list --json":
		var list []map[string]any
		ids := make([]string, 0, len(f.plugins))
		for id := range f.plugins {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			n, _, _ := strings.Cut(id, "@")
			list = append(list, map[string]any{"id": id, "name": n, "enabled": f.plugins[id], "mcpServerNames": []string{"plugin:" + n + ":" + n}})
		}
		if list == nil {
			list = []map[string]any{}
		}
		b, _ := json.MarshalIndent(list, "", "  ")
		if f.noise {
			return append(append([]byte("Warning: plugin x is broken\n"), b...), []byte("\nerror: manifest of x\n")...), errors.New("exit status 1")
		}
		return b, nil
	case cmd == "plugins marketplace list --json":
		var list []map[string]any
		for _, m := range f.markets {
			list = append(list, map[string]any{"id": m, "name": m, "pluginCount": 1})
		}
		if list == nil {
			list = []map[string]any{}
		}
		b, _ := json.Marshal(list)
		return b, nil
	case cmd == "plugins marketplace add "+zcodeMarketplaceURL:
		f.markets = append(f.markets, zcodeMarketplaceName)
		return []byte("Added marketplace agentnetwork (1 plugins)\n"), nil
	case cmd == "plugins install "+zcodePluginID:
		found := false
		for _, m := range f.markets {
			found = found || m == zcodeMarketplaceName
		}
		if !found {
			return []byte("Error: Unknown marketplace: agentnetwork\n"), errors.New("exit status 1")
		}
		if !f.noop {
			f.plugins[zcodePluginID] = true
		}
		return []byte("Installed plugin anet@agentnetwork (0.2.0) [enabled]\n"), nil
	case strings.HasPrefix(cmd, "plugins enable "):
		if !f.noop {
			f.plugins[args[2]] = true
		}
		return []byte("Enabled plugin " + args[2] + "\n"), nil
	case strings.HasPrefix(cmd, "plugins uninstall ") && strings.HasSuffix(cmd, " --force"):
		if !f.noop {
			delete(f.plugins, args[2])
		}
		return []byte("Uninstalled plugin " + args[2] + "\n"), nil
	}
	return []byte("Unknown plugins command\n"), errors.New("exit status 1")
}

func zcodeHost(t *testing.T) (*host, *fakeZCode) {
	h := newHost(t)
	f := newFakeZCode()
	h.path["zcode"] = testZCode
	h.run = f.run
	// ZCode's own directory holds the user's model credentials; it must
	// come out of every test exactly as it went in.
	h.write(".zcode/cli/config.json", `{"provider":{"x":{"options":{"apiKey":"SECRET"}}}}`+"\n", 0o600)
	return h, f
}

func (h *host) commands() []string {
	var out []string
	for _, c := range h.calls {
		out = append(out, strings.Join(c[1:], " "))
	}
	h.calls = nil
	return out
}

func TestZCodeWireAndUnwire(t *testing.T) {
	h, f := zcodeHost(t)
	orig := h.snapshot()
	r := one(t, h.wire(ToolZCode), Written)
	want := []string{"plugins list --json", "plugins marketplace list --json", "plugins marketplace add " + zcodeMarketplaceURL,
		"plugins install " + zcodePluginID, "plugins list --json"}
	if got := h.commands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("commands %q, want %q", got, want)
	}
	if !f.plugins[zcodePluginID] {
		t.Fatal("the plugin is not installed")
	}
	notes := notesText(r.Notes)
	for _, s := range []string{"zcode plugins marketplace add " + zcodeMarketplaceURL + " printed: Added marketplace agentnetwork",
		"zcode plugins install anet@agentnetwork printed: Installed plugin anet@agentnetwork (0.2.0) [enabled]"} {
		if !strings.Contains(notes, s) {
			t.Fatalf("notes lack %q:\n%s", s, notes)
		}
	}
	if len(r.Backups) != 0 {
		t.Fatalf("backups %v", r.Backups)
	}

	one(t, h.wire(ToolZCode), UpToDate)
	if got := h.commands(); !reflect.DeepEqual(got, []string{"plugins list --json"}) {
		t.Fatalf("a second wire ran %q", got)
	}

	one(t, h.unwire(ToolZCode), Removed)
	if got, want := h.commands(), []string{"plugins list --json", "plugins uninstall " + zcodePluginID + " --force", "plugins list --json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unwire ran %q, want %q", got, want)
	}
	one(t, h.unwire(ToolZCode), NotWired)
	if got := h.snapshot(); !reflect.DeepEqual(got, orig) {
		t.Fatalf("ZCode's files changed:\n%s", diffSnap(orig, got))
	}
}

func TestZCodeKnownMarketplaceIsNotAddedAgain(t *testing.T) {
	h, f := zcodeHost(t)
	f.markets = []string{"zcode-plugins-official", zcodeMarketplaceName}
	one(t, h.wire(ToolZCode), Written)
	for _, c := range h.commands() {
		if strings.Contains(c, "marketplace add") {
			t.Fatalf("the marketplace was added again: %q", c)
		}
	}
}

func TestZCodeDisabledPluginIsEnabled(t *testing.T) {
	h, f := zcodeHost(t)
	f.plugins[zcodePluginID] = false
	one(t, h.wire(ToolZCode), Written)
	if got := h.commands(); !reflect.DeepEqual(got, []string{"plugins list --json", "plugins enable " + zcodePluginID, "plugins list --json"}) {
		t.Fatalf("commands %q", got)
	}
}

// The anet plugin of ZCode's official marketplace is the same plugin: wire
// leaves it in place, unwire removes it.
func TestZCodeOfficialPluginCounts(t *testing.T) {
	h, f := zcodeHost(t)
	f.plugins["anet@zcode-plugins-official"] = true
	one(t, h.wire(ToolZCode), UpToDate)
	h.commands()
	one(t, h.unwire(ToolZCode), Removed)
	if len(f.plugins) != 0 {
		t.Fatalf("plugins %v", f.plugins)
	}
}

func TestZCodeCLIThatDoesNothingFails(t *testing.T) {
	h, f := zcodeHost(t)
	f.noop = true
	r := one(t, h.wire(ToolZCode), Failed)
	if m, _ := AsMsg(r.Err); m.Key != "wire_verify_failed.zcode_add" {
		t.Fatalf("err %v", r.Err)
	}
	f.noop = false
	f.plugins[zcodePluginID] = true
	f.noop = true
	r = one(t, h.unwire(ToolZCode), Failed)
	if m, _ := AsMsg(r.Err); m.Key != "wire_verify_failed.zcode_remove" {
		t.Fatalf("err %v", r.Err)
	}
}

func TestZCodeFailedCommandIsReported(t *testing.T) {
	h, f := zcodeHost(t)
	f.markets = []string{zcodeMarketplaceName}
	h.run = func(name string, args []string) ([]byte, error) {
		if len(args) > 1 && args[1] == "install" {
			return []byte("Error: download failed: 404\nmore"), errors.New("exit status 1")
		}
		return f.run(name, args)
	}
	r := one(t, h.wire(ToolZCode), Failed)
	m, _ := AsMsg(r.Err)
	if m.Key != "wire_cli_failed" || !strings.Contains(m.String(), "download failed: 404") {
		t.Fatalf("err %v", r.Err)
	}
	if !strings.Contains(notesText(r.Notes), "printed: Error: download failed: 404 | more") {
		t.Fatalf("notes %v", r.Notes)
	}
}

// Diagnostics about other plugins around the JSON, and exit status 1, do
// not hide the list.
func TestZCodeListWithDiagnostics(t *testing.T) {
	h, f := zcodeHost(t)
	f.plugins[zcodePluginID] = true
	f.noise = true
	one(t, h.wire(ToolZCode), UpToDate)
	h.run = func(string, []string) ([]byte, error) {
		return []byte("Segmentation fault\n"), errors.New("signal: segmentation fault")
	}
	r := one(t, h.wire(ToolZCode), Failed)
	if m, _ := AsMsg(r.Err); m.Key != "wire_cli_failed" || !strings.Contains(m.String(), "Segmentation fault") {
		t.Fatalf("err %v", r.Err)
	}
}

// Without the program nothing is run and nothing is written: an explicit
// wire fails with the steps of the desktop application, --all skips ZCode
// and gives the steps as a note when the application has its directory.
func TestZCodeWithoutCLI(t *testing.T) {
	h := newHost(t)
	r := one(t, h.wire(ToolZCode), Failed)
	m, _ := AsMsg(r.Err)
	if m.Key != "wire_cli_failed.zcode_missing" || !strings.Contains(m.String(), zcodeMarketplaceURL) || len(r.Notes) != 0 {
		t.Fatalf("err %v, notes %v", r.Err, r.Notes)
	}
	one(t, h.unwire(ToolZCode), NotWired)
	if len(h.calls) != 0 || len(h.snapshot()) != 0 {
		t.Fatalf("calls %v, files %v", h.calls, keys(h.snapshot()))
	}

	var zr Result
	for _, r := range h.wire() {
		if r.Tool == ToolZCode {
			zr = r
		}
	}
	if zr.Status != Skipped || len(zr.Notes) != 0 {
		t.Fatalf("--all without ZCode: %+v", zr)
	}
	h.write(".zcode/cli/config.json", "{}\n", 0o600)
	for _, r := range h.wire() {
		if r.Tool == ToolZCode {
			zr = r
		}
	}
	if zr.Status != Skipped || !strings.Contains(notesText(zr.Notes), zcodeMarketplaceURL) {
		t.Fatalf("--all with the desktop application only: %+v", zr)
	}
	for _, r := range h.unwire() {
		if r.Tool == ToolZCode && (r.Status != NotWired || !strings.Contains(notesText(r.Notes), "uninstall it there")) {
			t.Fatalf("unwire --all with the desktop application only: %+v", r)
		}
	}
	if len(h.calls) != 0 {
		t.Fatalf("commands ran without the program: %v", h.calls)
	}
}

func TestZCodeInspect(t *testing.T) {
	h := newHost(t)
	st := inspectOne(t, h, ToolZCode)
	if st.Detected || st.Wired || st.Err != "" || st.Conflict != "" {
		t.Fatalf("without the program: %+v", st)
	}
	h, f := zcodeHost(t)
	if st = inspectOne(t, h, ToolZCode); !st.Detected || st.Wired || st.Current {
		t.Fatalf("before wire: %+v", st)
	}
	f.plugins[zcodePluginID] = true
	if st = inspectOne(t, h, ToolZCode); !st.Wired || !st.Current {
		t.Fatalf("after wire: %+v", st)
	}
	for _, c := range h.commands() {
		if c != "plugins list --json" && c != "plugins marketplace list --json" {
			t.Fatalf("Inspect changed something: %q", c)
		}
	}
}
