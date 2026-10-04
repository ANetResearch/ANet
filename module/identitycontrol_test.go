package module_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the directory holding go.mod above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = up
	}
}

// parseTree parses the Go files under dir (tests included when withTests), skipping testdata
// and hidden directories. The keys are paths relative to dir.
func parseTree(t *testing.T, dir string, withTests bool) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || !withTests && strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fset, out
}

// mentionsController reports whether n names the type identity.Controller.
func mentionsController(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Controller" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "identity" {
				found = true
			}
		}
		return !found
	})
	return found
}

// KEL O-4: a module gets no way to change the node's identity. No interface the module package
// exports (the host and its seams) has a method named Controller or Build*, or a method whose
// signature carries an identity.Controller; and (O-1) no module's code holds an
// identity.Controller at all: modules sign through identity.Signer or a seam.
func TestModulesGetNoIdentityControl(t *testing.T) {
	root := repoRoot(t)
	fset, files := parseTree(t, filepath.Join(root, "module"), false)
	interfaces := 0
	for rel, f := range files {
		if filepath.Dir(rel) == "." {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, sp := range gd.Specs {
					ts := sp.(*ast.TypeSpec)
					it, ok := ts.Type.(*ast.InterfaceType)
					if !ok || !ts.Name.IsExported() {
						continue
					}
					interfaces++
					for _, m := range it.Methods.List {
						for _, name := range m.Names {
							if name.Name == "Controller" || strings.HasPrefix(name.Name, "Build") {
								t.Errorf("%s: module.%s has method %s, an identity control", fset.Position(name.Pos()), ts.Name.Name, name.Name)
							}
						}
						if mentionsController(m.Type) {
							t.Errorf("%s: module.%s has a method that carries an identity.Controller", fset.Position(m.Pos()), ts.Name.Name)
						}
					}
				}
			}
		}
		if mentionsController(f) {
			t.Errorf("module/%s names identity.Controller: a module signs through identity.Signer (KEL O-1)", rel)
		}
	}
	if interfaces == 0 {
		t.Fatal("found no exported interface in package module: the check looks at nothing")
	}
}

// KEL C-23: Controller.CurrentPrivateKey is deleted; no identifier of that name exists anywhere in
// this repository, tests included.
func TestNoCurrentPrivateKey(t *testing.T) {
	fset, files := parseTree(t, repoRoot(t), true)
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "CurrentPrivateKey" {
				t.Errorf("%s: identifier CurrentPrivateKey (KEL C-23 deletes it)", fset.Position(id.Pos()))
			}
			return true
		})
	}
}
