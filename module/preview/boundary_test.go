package preview

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// previewForbiddenCalls are the calls the module never makes: previews are
// never written (SI-73).
var previewForbiddenCalls = map[string]bool{"os.Create": true, "os.OpenFile": true, "os.WriteFile": true, "os.MkdirAll": true}

// TestPreviewModuleImportBoundary (MOD-6; 11-modularity §7.1): the module
// reaches the node only through module.PreviewSeam. Its dependency closure
// holds neither the kernel (internal/daemon), nor the interactions store,
// nor the sealing code, nor the SQL driver, nor the auto-reply module (the
// two modules do not import one another, MOD-V1); and no file of it creates,
// opens for writing or writes a file.
func TestPreviewModuleImportBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range []string{
			"github.com/ANetResearch/ANet/internal/daemon",
			"github.com/ANetResearch/ANet/internal/runtime/interactions",
			"github.com/ANetResearch/ANetCore/seal",
			"modernc.org/sqlite",
			"github.com/ANetResearch/ANet/module/autoreply",
		} {
			if dep == bad || strings.HasPrefix(dep, bad+"/") {
				t.Errorf("the module's dependency closure holds %s", dep)
			}
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]string{} // local name -> import path
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			n := p[strings.LastIndex(p, "/")+1:]
			if im.Name != nil {
				n = im.Name.Name
			}
			names[n] = p
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && names[id.Name] == "os" && previewForbiddenCalls["os."+sel.Sel.Name] {
					bad = append(bad, fset.Position(n.Pos()).String()+": os."+sel.Sel.Name)
				}
			}
			return true
		})
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("writes a file: %s", b)
	}
}
