package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"

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
