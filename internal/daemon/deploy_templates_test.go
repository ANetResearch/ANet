package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"

	// The modules the templates configure. The kernel names none of them;
	// this test file registers them so the module blocks are checked by the
	// same code the daemon runs (CUX-V10).
	_ "github.com/ANetResearch/ANet/module/blackboard"
	_ "github.com/ANetResearch/ANet/module/cas"
	_ "github.com/ANetResearch/ANet/module/org"
	_ "github.com/ANetResearch/ANet/module/service"
	_ "github.com/ANetResearch/ANet/module/x402"
)

// deployTemplates is every daemon configuration template under deploy/.
func deployTemplates(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "deploy")
	var found []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if n := d.Name(); n == "config.json" || strings.HasSuffix(n, ".config.json") {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// checkTemplate runs the daemon's start-up check over raw as the data
// directory's config.json and returns the codes it refused with.
func checkTemplate(t *testing.T, raw []byte) (codes []string, other error) {
	t.Helper()
	l := NewLayout(t.TempDir())
	if err := os.WriteFile(l.ConfigPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckConfigForStart(l)
	if err == nil {
		return nil, nil
	}
	cr, ok := err.(*ConfigRefusal)
	if !ok {
		return nil, err
	}
	for _, r := range cr.Refusals {
		codes = append(codes, r.Code()+" "+templateRefusalKey(r))
	}
	return codes, nil
}

func templateRefusalKey(r *a2ashape.Refusal) string {
	if k, ok := r.Args["key"].(string); ok {
		return k
	}
	return ""
}

// TestDeployTemplatesLoad (CUX-V10): every template in deploy/ passes the
// check the daemon makes before it starts, and none relaxes peer_check.
func TestDeployTemplatesLoad(t *testing.T) {
	files := deployTemplates(t)
	if len(files) == 0 {
		t.Fatal("no deploy template found; the directory moved and this check would run on nothing")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		codes, other := checkTemplate(t, raw)
		if other != nil {
			t.Errorf("%s: %v", f, other)
		}
		for _, c := range codes {
			t.Errorf("%s: %s", f, c)
		}
		if strings.Contains(string(raw), `"peer_check": "path-only"`) || strings.Contains(string(raw), `"peer_check":"path-only"`) {
			t.Errorf("%s: peer_check path-only; the default is the strict check", f)
		}
	}
}

// The check itself: a template that differs from a valid one only by a
// removed key is refused with config_key_removed.
func TestDeployTemplateCheckCatchesRemovedKey(t *testing.T) {
	files := deployTemplates(t)
	if len(files) == 0 {
		t.Fatal("no deploy template found")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	in, _ := m["inbound"].(map[string]any)
	if in == nil {
		in = map[string]any{}
		m["inbound"] = in
	}
	in["allow_file"] = "peers.allow"
	bad, _ := json.Marshal(m)
	codes, other := checkTemplate(t, bad)
	if other != nil {
		t.Fatal(other)
	}
	for _, c := range codes {
		if c == "config_key_removed inbound.allow_file" {
			return
		}
	}
	t.Fatalf("a template with inbound.allow_file got %v", codes)
}

// TestDeployExamplesUseDefaultModules: every modules.* key a template under
// deploy/examples names is a module of the default build (not an additive
// one such as shell), and the service example exists.
func TestDeployExamplesUseDefaultModules(t *testing.T) {
	def := map[string]bool{}
	for _, k := range module.Known() {
		if !k.Additive {
			def[k.Name] = true
		}
	}
	found := false
	for _, f := range deployTemplates(t) {
		if !strings.Contains(filepath.ToSlash(f), "deploy/examples/") {
			continue
		}
		if strings.Contains(filepath.ToSlash(f), "examples/service-files/") {
			found = true
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Modules map[string]json.RawMessage `json:"modules"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name := range m.Modules {
			if !def[name] {
				t.Errorf("%s: modules.%s is not a module of the default build", f, name)
			}
		}
	}
	if !found {
		t.Fatal("deploy/examples/service-files has no config.json")
	}
}

// TestDeployExamplesCarryNoInternalNames: the public examples name no
// internal host, no operations port and no account identifier.
func TestDeployExamplesCarryNoInternalNames(t *testing.T) {
	banned := []string{"host-c", "host-d", "host-e", "host-f", "8093", "11434", "10.2.2."}
	aid := regexp.MustCompile(`(?i)\b(did:[a-z0-9]+:\S{8,}|anet1[a-z0-9]{20,}|[0-9a-f]{64})\b`)
	root := filepath.Join("..", "..", "deploy", "examples")
	n := 0
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		low := strings.ToLower(string(raw))
		for _, b := range banned {
			if strings.Contains(low, b) {
				t.Errorf("%s contains %q", p, b)
			}
		}
		if aid.Match(raw) {
			t.Errorf("%s contains an identifier literal", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("deploy/examples is empty")
	}
}

// TestServiceExampleAdapter: the example adapter refuses an image above its
// pixel limit from the header alone and describes a small one.
func TestServiceExampleAdapter(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	script := `
import importlib.util, struct, sys
spec = importlib.util.spec_from_file_location("adapter", sys.argv[1])
a = importlib.util.module_from_spec(spec); spec.loader.exec_module(a)
def png(w, h):
    return b"\x89PNG\r\n\x1a\n" + struct.pack(">I", 13) + b"IHDR" + struct.pack(">II", w, h) + b"\x08\x02\x00\x00\x00"
assert a.describe(png(100, 50)) == (200, {"width": 100, "height": 50, "format": "png"})
assert a.describe(png(40000, 40000))[0] == 413
assert a.describe(png(0, 5))[0] == 413
assert a.describe(b"not an image")[0] == 415
print("ok")
`
	adapter := filepath.Join("..", "..", "deploy", "examples", "service-files", "adapter.py")
	out, err := exec.Command(py, "-I", "-c", script, adapter).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("adapter check: %v\n%s", err, out)
	}
}
