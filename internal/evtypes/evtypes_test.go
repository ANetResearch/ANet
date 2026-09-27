package evtypes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestTheRegistryIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range All() {
		if !strings.HasPrefix(e.Type, "anet.") || strings.Count(e.Type, ".") < 2 {
			t.Errorf("%q is not an anet.<domain>.<event> name", e.Type)
		}
		if seen[e.Type] {
			t.Errorf("%q is registered twice", e.Type)
		}
		seen[e.Type] = true
		if e.Label == "" {
			t.Errorf("%q has no label", e.Type)
		}
		switch e.Source {
		case SourceNode, SourcePeer, SourceHub:
		default:
			t.Errorf("%q has source %q", e.Type, e.Source)
		}
		if got, ok := Lookup(e.Type); !ok || got != e {
			t.Errorf("Lookup(%q) = %+v, %v", e.Type, got, ok)
		}
	}
	if Registered("anet.not.an.event") {
		t.Error("an unregistered type is reported as registered")
	}
}

// Every evidence write in the source tree names a registered event type.
//
// The check reads the Go files of this repository (all build tags, tests
// excluded) and finds every call that appends to a node's chain: a
// RecordEvidence call, an Append on a ledger, and any function that passes
// one of its parameters on to either (a module's record helper). The event
// type argument must resolve to a string constant, and that constant must
// be in the registry. An event type computed at run time fails the test
// too: `anet audit` could not know it.
func TestEveryEvidenceWriteUsesARegisteredType(t *testing.T) {
	root := filepath.Join("..", "..")
	mod := readModulePath(t, root)
	sc := scanTree(t, root, mod)

	type sinkKey struct {
		dir, name string
		arg       int
	}
	sinks := map[sinkKey]bool{}
	isSink := func(dir, name string, recv ast.Expr) (int, bool) {
		switch {
		case name == "RecordEvidence":
			return 0, true
		case name == "Append":
			if sel, ok := recv.(*ast.SelectorExpr); ok && sel.Sel.Name == "ledger" {
				return 0, true
			}
		}
		for k := range sinks {
			if k.dir == dir && k.name == name {
				return k.arg, true
			}
		}
		return 0, false
	}

	var problems []string
	checked := map[string]int{}
	for pass := 0; ; pass++ {
		if pass > 10 {
			t.Fatal("evidence wrappers do not reach a fixed point")
		}
		before := len(sinks)
		problems, checked = nil, map[string]int{}
		for _, pf := range sc.files {
			for _, decl := range pf.file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					var name string
					var recv ast.Expr
					switch f := call.Fun.(type) {
					case *ast.Ident:
						name = f.Name
					case *ast.SelectorExpr:
						name, recv = f.Sel.Name, f.X
					default:
						return true
					}
					idx, sink := isSink(pf.dir, name, recv)
					if !sink {
						// An Append of an anet.* constant on any receiver is an
						// evidence write too (the ledger appending its own gap
						// marker).
						if name == "Append" && len(call.Args) > 0 {
							if s, ok := sc.resolve(pf, call.Args[0], 0); ok && strings.HasPrefix(s, "anet.") {
								idx, sink = 0, true
							}
						}
						if !sink {
							return true
						}
					}
					if idx >= len(call.Args) {
						return true
					}
					pos := sc.fset.Position(call.Pos())
					where := relPos(root, pos)
					arg := call.Args[idx]
					if s, ok := sc.resolve(pf, arg, 0); ok {
						checked[s]++
						if !Registered(s) {
							problems = append(problems, where+": event type "+strconv.Quote(s)+" is not registered in internal/evtypes")
						}
						return true
					}
					id, ok := arg.(*ast.Ident)
					if !ok {
						problems = append(problems, where+": event type is not a constant")
						return true
					}
					if p, ok := paramIndex(fd, id.Name); ok {
						sinks[sinkKey{pf.dir, fd.Name.Name, p}] = true
						return true
					}
					values := localAssignments(fd, id.Name)
					if len(values) == 0 {
						problems = append(problems, where+": event type "+id.Name+" is not a constant")
						return true
					}
					for _, v := range values {
						s, ok := sc.resolve(pf, v, 0)
						if !ok {
							problems = append(problems, where+": event type "+id.Name+" is not a constant")
							continue
						}
						checked[s]++
						if !Registered(s) {
							problems = append(problems, where+": event type "+strconv.Quote(s)+" is not registered in internal/evtypes")
						}
					}
					return true
				})
			}
		}
		if len(sinks) == before {
			break
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	// The scan must have found the writes it exists to check; a scan that
	// silently read nothing would pass.
	for _, must := range []string{CapabilityEffect, DelegationReceived, PolicyChanged, PaymentAuthorized,
		ShellCommand, EvidenceGap, MessageSent, MessageReceived, InteractionPruned} {
		if checked[must] == 0 {
			t.Errorf("no evidence write of %s was found; the scan is not reading the tree", must)
		}
	}
}

// --- a small constant resolver over the parsed tree ---

type parsedFile struct {
	dir  string // slash path relative to the module root
	file *ast.File
}

type constDef struct {
	value ast.Expr
	pf    *parsedFile
}

type tree struct {
	fset   *token.FileSet
	mod    string
	files  []*parsedFile
	consts map[string]map[string]constDef // dir -> name -> definition
}

func readModulePath(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return f[1]
		}
	}
	t.Fatal("go.mod names no module")
	return ""
}

func scanTree(t *testing.T, root, mod string) *tree {
	t.Helper()
	sc := &tree{fset: token.NewFileSet(), mod: mod, consts: map[string]map[string]constDef{}}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch n := d.Name(); {
			case path == root:
			case strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_"),
				n == "testdata", n == "vendor", n == "node_modules", n == "corpus":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(sc.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pf := &parsedFile{dir: filepath.ToSlash(rel), file: f}
		sc.files = append(sc.files, pf)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if sc.consts[pf.dir] == nil {
							sc.consts[pf.dir] = map[string]constDef{}
						}
						sc.consts[pf.dir][name.Name] = constDef{vs.Values[i], pf}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// resolve evaluates e, read in pf, to a string constant.
func (sc *tree) resolve(pf *parsedFile, e ast.Expr, depth int) (string, bool) {
	if depth > 16 {
		return "", false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return sc.resolve(pf, x.X, depth+1)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		a, ok1 := sc.resolve(pf, x.X, depth+1)
		b, ok2 := sc.resolve(pf, x.Y, depth+1)
		return a + b, ok1 && ok2
	case *ast.Ident:
		if c, ok := sc.consts[pf.dir][x.Name]; ok {
			return sc.resolve(c.pf, c.value, depth+1)
		}
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		dir, ok := sc.importDir(pf, pkg.Name)
		if !ok {
			return "", false
		}
		if c, ok := sc.consts[dir][x.Sel.Name]; ok {
			return sc.resolve(c.pf, c.value, depth+1)
		}
	}
	return "", false
}

// importDir maps a package name used in pf to the directory of a package
// of this module.
func (sc *tree) importDir(pf *parsedFile, name string) (string, bool) {
	for _, im := range pf.file.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil || !strings.HasPrefix(p, sc.mod+"/") {
			continue
		}
		local := p[strings.LastIndex(p, "/")+1:]
		if im.Name != nil {
			local = im.Name.Name
		}
		if local == name {
			return strings.TrimPrefix(p, sc.mod+"/"), true
		}
	}
	return "", false
}

// paramIndex is the position of the parameter called name in fd.
func paramIndex(fd *ast.FuncDecl, name string) (int, bool) {
	i := 0
	for _, field := range fd.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if n.Name == name {
				return i, true
			}
			i++
		}
	}
	return 0, false
}

// localAssignments returns every value assigned to the local variable name
// in fd's body.
func localAssignments(fd *ast.FuncDecl, name string) []ast.Expr {
	var out []ast.Expr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) != len(s.Rhs) {
				return true
			}
			for i, l := range s.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name {
					out = append(out, s.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, n := range s.Names {
				if n.Name == name && i < len(s.Values) {
					out = append(out, s.Values[i])
				}
			}
		}
		return true
	})
	return out
}

func relPos(root string, pos token.Position) string {
	if rel, err := filepath.Rel(root, pos.Filename); err == nil {
		return filepath.ToSlash(rel) + ":" + strconv.Itoa(pos.Line)
	}
	return pos.String()
}
