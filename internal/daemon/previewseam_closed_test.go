package daemon

import (
	"bufio"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// previewImplFiles are the files of the preview implementation: they may
// reach the node only through module.PreviewSeam (11-modularity §7.1). The
// seam's own adapter, previewseam.go, is the kernel's and is not on the list.
var previewImplFiles = []string{"preview.go"}

// previewImplImports are the non-standard-library imports an implementation
// file may have.
var previewImplImports = []string{
	"github.com/ANetResearch/ANet/module",
	"github.com/ANetResearch/ANet/internal/a2ashape",
	"github.com/ANetResearch/ANet/internal/evtypes",
	"github.com/ANetResearch/ANet/internal/localday",
	"github.com/ANetResearch/ANet/internal/execwork",
	"github.com/ANetResearch/ANet/internal/pathsafe",
	"github.com/ANetResearch/ANet/internal/anethome",
	"github.com/ANetResearch/ANetCore/anetcid",
	"github.com/ANetResearch/ANetCore/delegation",
	"golang.org/x/sys/unix",
}

// previewForbidden are the calls an implementation file never makes:
// previews are never written (SI-73).
var previewForbidden = map[string]bool{"os.Create": true, "os.OpenFile": true, "os.WriteFile": true, "os.MkdirAll": true}

// exportLookup finds the compiler export data of every dependency of this
// package, so that go/types can check it without the source of its imports.
func exportLookup(t *testing.T) func(path string) (io.ReadCloser, error) {
	t.Helper()
	out, err := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}", ".").Output()
	if err != nil {
		t.Skipf("go list -export: %v", err)
	}
	files := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if p, f, ok := strings.Cut(sc.Text(), "\t"); ok && f != "" {
			files[p] = f
		}
	}
	return func(path string) (io.ReadCloser, error) {
		f, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return os.Open(f)
	}
}

// checkPackage type-checks the non-test files of this package.
func checkPackage(t *testing.T) (*token.FileSet, []*ast.File, *types.Info, *types.Package) {
	t.Helper()
	fset := token.NewFileSet()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if constrained(string(b)) {
			continue // a file of another build; the default build is what is judged
		}
		f, err := parser.ParseFile(fset, m, b, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", exportLookup(t)), Error: func(error) {}}
	pkg, _ := conf.Check("github.com/ANetResearch/ANet/internal/daemon", fset, files, info)
	if pkg == nil {
		t.Fatal("the package did not type-check")
	}
	return fset, files, info, pkg
}

// constrained reports whether a file has a //go:build line (it belongs to a
// tagged build, not to the default one this test judges).
func constrained(src string) bool {
	for _, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(l, "//go:build") {
			return true
		}
		if strings.HasPrefix(l, "package ") {
			return false
		}
	}
	return false
}

// TestPreviewFilesReachKernelOnlyThroughSeams (MOD-5; 11-modularity §7.1,
// mutations mpv-*): the files of the preview implementation (1) select
// nothing of a Daemon, and name the Daemon type nowhere — as a receiver,
// parameter, field or conversion; (2) refer to no identifier of this
// package declared outside the listed files; (3) import nothing of the
// kernel beyond the allowed list, in particular neither the interactions
// store nor the sealing code; and (4) never create, open for writing or
// write a file. What they do to the node they do through module.PreviewSeam.
func TestPreviewFilesReachKernelOnlyThroughSeams(t *testing.T) {
	fset, files, info, pkg := checkPackage(t)
	inList := map[string]bool{}
	for _, f := range previewImplFiles {
		inList[f] = true
	}
	var judged []*ast.File
	for _, f := range files {
		if inList[filepath.Base(fset.Position(f.Pos()).Filename)] {
			judged = append(judged, f)
		}
	}
	if len(judged) != len(previewImplFiles) {
		t.Fatalf("%d of the %d implementation files found", len(judged), len(previewImplFiles))
	}
	daemonType := pkg.Scope().Lookup("Daemon")
	allowed := func(path string) bool {
		if !strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
			return true // the standard library
		}
		for _, a := range previewImplImports {
			if path == a {
				return true
			}
		}
		return false
	}
	var bad []string
	report := func(pos token.Pos, format string, a ...string) {
		bad = append(bad, fset.Position(pos).String()+": "+format+" "+strings.Join(a, " "))
	}
	for _, f := range judged {
		for _, im := range f.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); !allowed(p) {
				report(im.Pos(), "imports", p)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if sel := info.Selections[n]; sel != nil && namesDaemon(sel.Recv(), daemonType) {
					report(n.Pos(), "selects", n.Sel.Name, "of a Daemon")
				}
				if id, ok := n.X.(*ast.Ident); ok {
					if pn, ok := info.Uses[id].(*types.PkgName); ok && previewForbidden[pn.Imported().Name()+"."+n.Sel.Name] {
						report(n.Pos(), "calls", pn.Imported().Name()+"."+n.Sel.Name)
					}
				}
			case *ast.Ident:
				obj := info.Uses[n]
				if obj == nil {
					return true
				}
				if obj == daemonType {
					report(n.Pos(), "names the Daemon type")
				}
				if obj.Pkg() == pkg && obj.Parent() == pkg.Scope() && !inList[filepath.Base(fset.Position(obj.Pos()).Filename)] {
					report(n.Pos(), "refers to", n.Name, "declared in", filepath.Base(fset.Position(obj.Pos()).Filename))
				}
			}
			return true
		})
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

// namesDaemon reports whether typ is the Daemon type or a pointer to it.
func namesDaemon(typ types.Type, daemon types.Object) bool {
	if p, ok := typ.(*types.Pointer); ok {
		typ = p.Elem()
	}
	n, ok := typ.(*types.Named)
	return ok && daemon != nil && n.Obj() == daemon
}
