package provider_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// A provider that implements Priced takes money for its work, so it is the
// kind with the most reason to tie what it does to who asked. Call.CallerAID
// is an authenticated caller only on the relay door, and a priced provider
// learns the caller through Call.VerifiedCaller, never by reading CallerAID:
// the one accessor answers only for a caller the daemon authenticated, and
// a door added later (the hub's voucher door was one, until 0.3.0) cannot
// hand such a provider an identity it did not check.
//
// The check is structural: every package in this module that declares both
// an Invoke method and a Price(string) (uint64, bool) method is a priced
// provider, and its non-test sources must not select a field named
// CallerAID (TestVerifiedCallerIsOnlyTheRelayCaller).
func TestNoPricedProviderReadsCallerAID(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	pkgs := map[string][]*ast.File{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		dir := filepath.Dir(path)
		pkgs[dir] = append(pkgs[dir], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	priced := 0
	for dir, files := range pkgs {
		if !declaresPricedProvider(files) {
			continue
		}
		priced++
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "CallerAID" {
					rel, _ := filepath.Rel(root, dir)
					t.Errorf("%s: priced provider package %s reads CallerAID; it is an authenticated caller only on the relay door (use VerifiedCaller)",
						fset.Position(sel.Pos()), rel)
				}
				return true
			})
		}
	}
	if priced == 0 {
		// The service module is priced today. Finding none means the
		// detection stopped working, not that the rule holds.
		t.Fatal("no priced provider package found; the detection is broken")
	}
}

// declaresPricedProvider reports whether the files declare a method named
// Invoke and a method Price(string) (uint64, bool).
func declaresPricedProvider(files []*ast.File) bool {
	var invoke, price bool
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			switch fn.Name.Name {
			case "Invoke":
				invoke = true
			case "Price":
				if isPriceSig(fn.Type) {
					price = true
				}
			}
		}
	}
	return invoke && price
}

func isPriceSig(ft *ast.FuncType) bool {
	if ft.Params == nil || ft.Results == nil || ft.Params.NumFields() != 1 || ft.Results.NumFields() != 2 {
		return false
	}
	name := func(e ast.Expr) string {
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		return ""
	}
	return name(ft.Params.List[0].Type) == "string" &&
		name(ft.Results.List[0].Type) == "uint64" &&
		name(ft.Results.List[len(ft.Results.List)-1].Type) == "bool"
}
