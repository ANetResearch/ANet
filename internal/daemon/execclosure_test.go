package daemon

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// execFiles are the files of the local-execution face of auto-reply. MOD-4 moves them, unchanged, into
// module/autoreply; until then this test is what makes that a mechanical git mv (MOD §7.1).
var execFiles = []string{
	"autoreply.go", "autoreply_config.go", "autoreply_control.go", "autoreply_exec.go", "autoreply_session.go", "autoreply_status.go",
	"agents.go", "agents_sandbox.go", "sandbox_budget.go", "sandbox_init.go", "sandbox_linux.go",
	"sandbox_darwin.go", "sandbox_seatbelt.go", "seatbelt_profile.go", "seccomp_linux.go",
	"sandbox_unix.go", "sandbox_selfcheck.go", "sandbox_other.go", "sandbox_checks_other.go",
	"sandbox_group_other.go", "egress.go", "egress_connect.go", "egress_policy.go",
	"egress_upstreams.go", "hostname.go", "clienthello.go",
}

// seamFiles are the kernel-side files that hold the seam adapter types the execution files may name.
var seamFiles = []string{"turnhost.go"}

// execImportAllowed is the set of non-standard-library packages the execution files may import.
var execImportAllowed = map[string]bool{
	"github.com/ANetResearch/ANet/module":            true,
	"github.com/ANetResearch/ANet/internal/a2ashape": true,
	"github.com/ANetResearch/ANet/internal/evtypes":  true,
	"github.com/ANetResearch/ANet/internal/localday": true,
	"github.com/ANetResearch/ANet/internal/execwork": true,
	"github.com/ANetResearch/ANet/internal/pathsafe": true,
	"github.com/ANetResearch/ANet/internal/anethome": true,
	"github.com/ANetResearch/ANetCore/anetcid":       true,
	"github.com/ANetResearch/ANetCore/delegation":    true,
	"golang.org/x/sys/unix":                          true,
}

// execClosureViolations type-checks the non-test files of the package for one GOOS and returns what the
// execution files reach that they must not: anything of the Daemon type, package-level identifiers
// declared outside the list (and outside the seam files), and imports outside the allowed set.
func execClosureViolations(goos string) ([]string, error) {
	ctxt := build.Default
	ctxt.GOOS = goos
	ctxt.CgoEnabled = false
	saved := build.Default
	build.Default = ctxt
	defer func() { build.Default = saved }()

	bp, err := ctxt.ImportDir(".", 0)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range append(append([]string{}, bp.GoFiles...), bp.CgoFiles...) {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	info := &types.Info{
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Types:      map[ast.Expr]types.TypeAndValue{},
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil), Error: func(error) {}}
	pkg, _ := conf.Check("daemon", fset, files, info)
	if pkg == nil {
		return nil, fmt.Errorf("type-check failed for %s", goos)
	}
	inList := map[string]bool{}
	for _, n := range execFiles {
		inList[n] = true
	}
	allowedDecl := map[string]bool{}
	for _, n := range execFiles {
		allowedDecl[n] = true
	}
	for _, n := range seamFiles {
		allowedDecl[n] = true
	}
	fileOf := func(p token.Pos) string { return filepath.Base(fset.Position(p).Filename) }
	var bad []string
	for _, f := range files {
		name := fileOf(f.Pos())
		if !inList[name] {
			continue
		}
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			if strings.HasPrefix(p, "github.com/ANetResearch/") || strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
				if !execImportAllowed[p] {
					bad = append(bad, fmt.Sprintf("%s/%s: imports %s", goos, name, p))
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj := info.Uses[id]
			if obj == nil || obj.Pkg() != pkg {
				return true
			}
			if tn, ok := obj.(*types.TypeName); ok && tn.Name() == "Daemon" {
				bad = append(bad, fmt.Sprintf("%s/%s:%d: uses Daemon", goos, name, fset.Position(id.Pos()).Line))
				return true
			}
			if obj.Parent() == pkg.Scope() { // a package-level identifier
				if !allowedDecl[fileOf(obj.Pos())] {
					bad = append(bad, fmt.Sprintf("%s/%s:%d: %s (declared in %s)", goos, name, fset.Position(id.Pos()).Line, obj.Name(), fileOf(obj.Pos())))
				}
				return true
			}
			if v, ok := obj.(*types.Var); ok && v.IsField() || isMethod(obj) {
				// a field or method of a package type: where is it declared?
				if !allowedDecl[fileOf(obj.Pos())] {
					bad = append(bad, fmt.Sprintf("%s/%s:%d: .%s (declared in %s)", goos, name, fset.Position(id.Pos()).Line, obj.Name(), fileOf(obj.Pos())))
				}
			}
			return true
		})
	}
	sort.Strings(bad)
	return bad, nil
}

func isMethod(o types.Object) bool {
	f, ok := o.(*types.Func)
	if !ok {
		return false
	}
	sig, _ := f.Type().(*types.Signature)
	return sig != nil && sig.Recv() != nil
}

// TestExecFilesReachKernelOnlyThroughSeams (MOD §7.1, three closed assertions): the execution files
// (1) never name the Daemon type, so no field or method of it is reachable; (2) use only package-level
// identifiers declared in the list or in the seam adapters of turnhost.go; (3) import only the allowed
// packages. A violation is a kernel dependency that would stop MOD-4's git mv from being mechanical.
func TestExecFilesReachKernelOnlyThroughSeams(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		bad, err := execClosureViolations(goos)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range bad {
			t.Error(b)
		}
	}
}
