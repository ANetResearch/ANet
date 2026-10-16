package scripts

// LEC AV-6, the structural half (SI-60): no argument of any process anet starts comes from what must not
// be on a command line — an API key (an APIKey field), the conversation (the Goal of a replyContext, the
// Text of a chatTurn, the Conversation of an agent invocation) or a local token file (control_token.txt,
// a2a_token.txt). /proc/<pid>/cmdline is readable by every local user.
//
// The test reads ANet's code with go/ast (test files are not code that runs) and follows each argument
// of every exec.Command and exec.CommandContext call — and the Argv of a sandboxInitSpec, which init
// starts the agent with — backwards: through the local assignments of the function it is in (appends,
// writes into a builder, fields set one at a time, closures called with it), through the return values
// of the functions of the module that build it, and, where the argument is a parameter, to every caller
// that passes it, with the field of a struct parameter that the argument is made of. Any of the sources
// above found on the way is a failure, reported with where it was found and the call it reaches.
//
// It is an approximation that leans to reporting: names, not types, decide what a call is, and a value
// that goes through a function outside the module is taken to carry all its inputs. Its counterpart is
// the run-time canary (internal/daemon TestCmdlineCanary), which has no such leaning.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// argvForbiddenFields are the fields whose values must not reach a command line.
var argvForbiddenFields = map[string]string{
	"APIKey":       "an API key (auto_reply.api_key)",
	"Goal":         "the requester's goal",
	"Text":         "a conversation message",
	"Conversation": "the conversation",
}

// argvTokenNames are what reads or names a local token file.
var argvTokenNames = map[string]bool{
	"ControlTokenPath": true, "loadOrGenControlToken": true, "newControlToken": true, "A2ATokenFile": true,
	"TokenFile": true, "loadOrCreateToken": true, "rotateToken": true,
}

const anetModule = "github.com/ANetResearch/ANet/"

type argvFunc struct {
	pkg     string // the package's directory, relative to the module root
	name    string
	decl    *ast.FuncDecl
	file    *ast.File
	imports map[string]string // local name -> import path
	params  []string          // flattened; the receiver is not in it
	recv    string
	vararg  bool
	results []string // named results, "" for unnamed
	defs    map[string][]argvDef
}

// argvDef is one way a local name gets a value: name = expr (result: the index of a multi-value call),
// name.field = expr, or a mutation (name.Write(expr), f(&name, expr)).
type argvDef struct {
	expr   ast.Expr
	result int
	field  string
}

type argvParam struct {
	idx   int // -1: the receiver
	field string
}

type argvFlow struct {
	bad    []string
	params map[argvParam]bool
}

func (f *argvFlow) add(o argvFlow) {
	f.bad = append(f.bad, o.bad...)
	for p := range o.params {
		if f.params == nil {
			f.params = map[argvParam]bool{}
		}
		f.params[p] = true
	}
}

type argvChecker struct {
	fset     *token.FileSet
	root     string
	funcs    []*argvFunc
	byName   map[string][]*argvFunc // pkg + "." + name (functions, methods and the fields they are stored in)
	aliases  []argvAlias
	callers  map[*argvFunc][]argvCall
	retMemo  map[string]argvFlow
	retBusy  map[string]bool
	problems map[string]bool
}

func TestExecArgumentsCarryNoSecretAndNoConversation(t *testing.T) {
	c := newArgvChecker(t, "..")
	c.check()
	if len(c.funcs) < 500 {
		t.Fatalf("only %d functions parsed; the walk is probably broken", len(c.funcs))
	}
	var list []string
	for p := range c.problems {
		list = append(list, p)
	}
	sort.Strings(list)
	for _, p := range list {
		t.Error(p)
	}
}

// The check finds what it is for: an agent invocation that puts the conversation back on argv, a key
// passed as an argument, a token file read into one — each is reported. (The mutations in
// scripts/mutations/unit/exec.py do the same against the real code.)
func TestExecArgumentCheckerFindsTheFlows(t *testing.T) {
	dir := t.TempDir()
	src := `package daemon

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

type execInvokeOpts struct{ Instructions, Conversation, Model string }
type replyContext struct{ Role, Goal string }
type chatTurn struct{ Role, Text string }
type cfg struct{ APIKey string }

func runCmd(ctx context.Context, o execInvokeOpts, stdin []byte, bin string, args ...string) {
	_ = exec.CommandContext(ctx, bin, args...).Run()
}

func invokeA(ctx context.Context, o execInvokeOpts) {
	args := []string{"-p"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	args = append(args, o.Conversation)
	runCmd(ctx, o, nil, "a", args...)
}

func invokeB(ctx context.Context, o execInvokeOpts) {
	runCmd(ctx, o, []byte(o.Conversation), "b", "--sys", o.Instructions)
}

func prompt(rc replyContext, turns []chatTurn) string {
	var b strings.Builder
	b.WriteString(rc.Role)
	for _, t := range turns {
		b.WriteString(t.Text)
	}
	return b.String()
}

func reply(ctx context.Context, rc replyContext, turns []chatTurn) {
	var o execInvokeOpts
	o.Instructions = prompt(rc, turns)
	invokeB(ctx, o)
}

func key(c cfg) { _ = exec.Command("x", "--key="+c.APIKey).Run() }

func token(p string) {
	b, _ := os.ReadFile(p + "/control_token.txt")
	_ = exec.Command("x", string(b)).Run()
}
`
	if err := os.MkdirAll(filepath.Join(dir, "internal", "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "daemon", "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newArgvChecker(t, dir)
	c.check()
	all := strings.Join(keys(c.problems), "\n")
	for _, want := range []string{
		"x.go:24", // invokeA: o.Conversation appended to args
		"x.go:36", // prompt: t.Text, into Instructions, into invokeB's argv
		"x.go:47", // key: c.APIKey
		"x.go:50", // token: control_token.txt
	} {
		if !strings.Contains(all, want) {
			t.Errorf("not reported: %s\nreported:\n%s", want, all)
		}
	}
	if strings.Contains(all, "x.go:29") {
		t.Errorf("invokeB's stdin was reported as an argument:\n%s", all)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func newArgvChecker(t *testing.T, root string) *argvChecker {
	t.Helper()
	c := &argvChecker{fset: token.NewFileSet(), root: root, byName: map[string][]*argvFunc{},
		retMemo: map[string]argvFlow{}, retBusy: map[string]bool{}, problems: map[string]bool{}}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p != root && (n == "testdata" || n == "vendor" || strings.HasPrefix(n, ".") || n == "scripts") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(c.fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(p))
		c.addFile(filepath.ToSlash(rel), f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *argvChecker) addFile(pkg string, f *ast.File) {
	imports := map[string]string{}
	for _, im := range f.Imports {
		path, _ := strconv.Unquote(im.Path.Value)
		name := path[strings.LastIndex(path, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = path
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fn := &argvFunc{pkg: pkg, name: fd.Name.Name, decl: fd, file: f, imports: imports}
		if fd.Recv != nil && len(fd.Recv.List) > 0 && len(fd.Recv.List[0].Names) > 0 {
			fn.recv = fd.Recv.List[0].Names[0].Name
		}
		for _, fld := range fd.Type.Params.List {
			_, variadic := fld.Type.(*ast.Ellipsis)
			if len(fld.Names) == 0 {
				fn.params = append(fn.params, "_")
			}
			for _, n := range fld.Names {
				fn.params = append(fn.params, n.Name)
			}
			fn.vararg = variadic
		}
		if fd.Type.Results != nil {
			for _, fld := range fd.Type.Results.List {
				if len(fld.Names) == 0 {
					fn.results = append(fn.results, "")
				}
				for _, n := range fld.Names {
					fn.results = append(fn.results, n.Name)
				}
			}
		}
		fn.defs = collectDefs(fd.Body, imports)
		c.funcs = append(c.funcs, fn)
		c.byName[pkg+"."+fn.name] = append(c.byName[pkg+"."+fn.name], fn)
	}
	// A function stored in a field (invoke: invokeClaude) is called by that field's name.
	ast.Inspect(f, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		k, ok1 := kv.Key.(*ast.Ident)
		v, ok2 := kv.Value.(*ast.Ident)
		if ok1 && ok2 {
			c.aliases = append(c.aliases, argvAlias{pkg: pkg, field: k.Name, fn: v.Name})
		}
		return true
	})
}

type argvAlias struct{ pkg, field, fn string }

// argvCall is one call of a function of the module: the function it is in, and the call.
type argvCall struct {
	in   *argvFunc
	call *ast.CallExpr
}

// fieldHead splits a field path ("RC.Goal") into its first field and the rest.
func fieldHead(path string) (string, string) {
	head, rest, _ := strings.Cut(path, ".")
	return head, rest
}

// fieldJoin is the path of field rest inside field head, at most four deep: a deeper path is cut, which
// stands for more of the value, never less.
func fieldJoin(head, rest string) string {
	p := head
	if rest != "" {
		p += "." + rest
	}
	if parts := strings.Split(p, "."); len(parts) > 4 {
		p = strings.Join(parts[:4], ".")
	}
	return p
}

// collectDefs records how each local name gets a value in body (closures included).
func collectDefs(body *ast.BlockStmt, imports map[string]string) map[string][]argvDef {
	defs := map[string][]argvDef{}
	target := func(lhs ast.Expr) (string, string) {
		for {
			switch x := lhs.(type) {
			case *ast.Ident:
				return x.Name, ""
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					return id.Name, x.Sel.Name
				}
				lhs = x.X
			case *ast.IndexExpr:
				lhs = x.X
			case *ast.StarExpr:
				lhs = x.X
			case *ast.ParenExpr:
				lhs = x.X
			default:
				return "", ""
			}
		}
	}
	add := func(lhs ast.Expr, e ast.Expr, result int) {
		if name, field := target(lhs); name != "" && name != "_" {
			defs[name] = append(defs[name], argvDef{expr: e, result: result, field: field})
		}
	}
	funcLits := map[string]*ast.FuncLit{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Lhs) == len(s.Rhs) {
				for i := range s.Lhs {
					add(s.Lhs[i], s.Rhs[i], 0)
					if fl, ok := s.Rhs[i].(*ast.FuncLit); ok {
						if id, ok := s.Lhs[i].(*ast.Ident); ok {
							funcLits[id.Name] = fl
						}
					}
				}
			} else if len(s.Rhs) == 1 {
				for i := range s.Lhs {
					add(s.Lhs[i], s.Rhs[0], i)
				}
			}
		case *ast.ValueSpec:
			for i, n := range s.Names {
				switch {
				case len(s.Values) == len(s.Names):
					add(n, s.Values[i], 0)
				case len(s.Values) == 1:
					add(n, s.Values[0], i)
				}
			}
		case *ast.RangeStmt:
			if s.Value != nil {
				add(s.Value, s.X, 0)
			}
		case *ast.CallExpr:
			// x.M(args) on a local x, and f(&x, args): the args flow into x.
			if sel, ok := s.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] == "" {
					for _, a := range s.Args {
						add(id, a, 0)
					}
				}
			}
			for _, a := range s.Args {
				if u, ok := a.(*ast.UnaryExpr); ok && u.Op == token.AND {
					for _, b := range s.Args {
						if b != a {
							add(u.X, b, 0)
						}
					}
				}
			}
		}
		return true
	})
	// A closure called by name: its parameters get the call's arguments.
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || funcLits[id.Name] == nil {
			return true
		}
		var names []string
		for _, f := range funcLits[id.Name].Type.Params.List {
			for _, n := range f.Names {
				names = append(names, n.Name)
			}
		}
		for i, a := range call.Args {
			if i < len(names) {
				defs[names[i]] = append(defs[names[i]], argvDef{expr: a})
			}
		}
		return true
	})
	return defs
}

// resolve lists the module's functions a call may run.
func (c *argvChecker) resolve(fn *argvFunc, call *ast.CallExpr) []*argvFunc {
	var pkg, name string
	switch f := call.Fun.(type) {
	case *ast.Ident:
		pkg, name = fn.pkg, f.Name
	case *ast.SelectorExpr:
		name = f.Sel.Name
		if id, ok := f.X.(*ast.Ident); ok && fn.imports[id.Name] != "" {
			path := fn.imports[id.Name]
			if !strings.HasPrefix(path, anetModule) {
				return nil
			}
			pkg = strings.TrimPrefix(path, anetModule)
		} else {
			pkg = fn.pkg
		}
	default:
		return nil
	}
	out := append([]*argvFunc(nil), c.byName[pkg+"."+name]...)
	for _, a := range c.aliases {
		if a.pkg == pkg && a.field == name {
			out = append(out, c.byName[pkg+"."+a.fn]...)
		}
	}
	return out
}

// isExecCall reports exec.Command and exec.CommandContext, and which of the arguments are the program and
// its arguments.
func isExecCall(fn *argvFunc, call *ast.CallExpr) ([]ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || fn.imports[id.Name] != "os/exec" {
		return nil, false
	}
	switch sel.Sel.Name {
	case "Command":
		return call.Args, true
	case "CommandContext":
		if len(call.Args) > 0 {
			return call.Args[1:], true
		}
	}
	return nil, false
}

func (c *argvChecker) pos(n ast.Node) string {
	p := c.fset.Position(n.Pos())
	rel, err := filepath.Rel(c.root, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), p.Line)
}

func (c *argvChecker) paramIndex(fn *argvFunc, name string) int {
	if name == fn.recv && name != "" {
		return -1
	}
	for i, p := range fn.params {
		if p == name && p != "_" {
			return i
		}
	}
	return -2
}

// expand is what e, in fn, is made of: the forbidden sources found and the parameters (with the field
// of each that e takes, "" for all of it) it comes from. result is the value of a multi-value call that
// e stands for.
func (c *argvChecker) expand(fn *argvFunc, e ast.Expr, field string, result int, seen map[string]bool) argvFlow {
	var out argvFlow
	switch x := e.(type) {
	case nil:
	case *ast.Ident:
		switch x.Name {
		case "nil", "true", "false", "_":
			return out
		}
		if i := c.paramIndex(fn, x.Name); i >= -1 {
			out.params = map[argvParam]bool{{idx: i, field: field}: true}
			return out
		}
		key := x.Name + "." + field
		if seen[key] {
			return out
		}
		seen[key] = true
		head, rest := fieldHead(field)
		for _, d := range fn.defs[x.Name] {
			switch {
			case d.field != "" && field == "":
				out.add(c.expand(fn, d.expr, "", 0, seen))
			case d.field != "" && d.field == head:
				out.add(c.expand(fn, d.expr, rest, 0, seen))
			case d.field == "":
				out.add(c.expand(fn, d.expr, field, d.result, seen))
			}
		}
	case *ast.SelectorExpr:
		if what, ok := argvForbiddenFields[x.Sel.Name]; ok {
			out.bad = append(out.bad, c.pos(x)+": "+what+" ("+x.Sel.Name+")")
			return out
		}
		if argvTokenNames[x.Sel.Name] {
			out.bad = append(out.bad, c.pos(x)+": a local token ("+x.Sel.Name+")")
			return out
		}
		if id, ok := x.X.(*ast.Ident); ok && fn.imports[id.Name] != "" {
			return out // a package's name, constant or variable
		}
		out.add(c.expand(fn, x.X, fieldJoin(x.Sel.Name, field), 0, seen))
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && argvTokenNames[id.Name] {
			out.bad = append(out.bad, c.pos(x)+": a local token ("+id.Name+")")
			return out
		}
		if id, ok := x.Fun.(*ast.Ident); ok {
			switch id.Name {
			case "len", "cap":
				return out
			case "append":
				for _, a := range x.Args {
					out.add(c.expand(fn, a, field, 0, seen))
				}
				return out
			}
		}
		if callees := c.resolve(fn, x); len(callees) > 0 {
			for _, g := range callees {
				rf := c.returnFlow(g, result, field)
				out.bad = append(out.bad, rf.bad...)
				for p := range rf.params {
					arg := callArg(x, g, p.idx)
					for _, a := range arg {
						out.add(c.expand(fn, a, p.field, 0, seen))
					}
				}
			}
			return out
		}
		for _, a := range x.Args {
			out.add(c.expand(fn, a, "", 0, seen))
		}
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			out.add(c.expand(fn, sel.X, "", 0, seen))
		}
	case *ast.CompositeLit:
		head, rest := fieldHead(field)
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && field != "" {
					if k.Name != head {
						continue
					}
					out.add(c.expand(fn, kv.Value, rest, 0, seen))
					continue
				}
				out.add(c.expand(fn, kv.Value, "", 0, seen))
				continue
			}
			out.add(c.expand(fn, el, field, 0, seen))
		}
	case *ast.BasicLit:
		if x.Kind == token.STRING && (strings.Contains(x.Value, "_token.txt")) {
			out.bad = append(out.bad, c.pos(x)+": a local token file ("+x.Value+")")
		}
	case *ast.BinaryExpr:
		out.add(c.expand(fn, x.X, field, 0, seen))
		out.add(c.expand(fn, x.Y, field, 0, seen))
	case *ast.UnaryExpr:
		out.add(c.expand(fn, x.X, field, result, seen))
	case *ast.StarExpr:
		out.add(c.expand(fn, x.X, field, result, seen))
	case *ast.ParenExpr:
		out.add(c.expand(fn, x.X, field, result, seen))
	case *ast.IndexExpr:
		out.add(c.expand(fn, x.X, field, 0, seen))
	case *ast.SliceExpr:
		out.add(c.expand(fn, x.X, field, 0, seen))
	case *ast.TypeAssertExpr:
		out.add(c.expand(fn, x.X, field, 0, seen))
	case *ast.KeyValueExpr:
		out.add(c.expand(fn, x.Value, field, 0, seen))
	}
	return out
}

// callArg is the argument expression(s) of call for parameter idx of g (the receiver for -1).
func callArg(call *ast.CallExpr, g *argvFunc, idx int) []ast.Expr {
	if idx == -1 {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			return []ast.Expr{sel.X}
		}
		return nil
	}
	if g.vararg && idx == len(g.params)-1 && !call.Ellipsis.IsValid() {
		if idx < len(call.Args) {
			return call.Args[idx:]
		}
		return nil
	}
	if idx < len(call.Args) {
		return []ast.Expr{call.Args[idx]}
	}
	return nil
}

// returnFlow is what result `result` of g, field `field` of it, is made of.
func (c *argvChecker) returnFlow(g *argvFunc, result int, field string) argvFlow {
	key := fmt.Sprintf("%p/%d/%s", g, result, field)
	if f, ok := c.retMemo[key]; ok {
		return f
	}
	if c.retBusy[key] {
		return argvFlow{}
	}
	c.retBusy[key] = true
	defer delete(c.retBusy, key)
	var out argvFlow
	ast.Inspect(g.decl.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		r, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		switch {
		case len(r.Results) == 0 && result < len(g.results) && g.results[result] != "":
			out.add(c.expand(g, ast.NewIdent(g.results[result]), field, 0, map[string]bool{}))
		case len(r.Results) == 1 && len(g.results) > 1:
			out.add(c.expand(g, r.Results[0], field, result, map[string]bool{}))
		case result < len(r.Results):
			out.add(c.expand(g, r.Results[result], field, 0, map[string]bool{}))
		}
		return true
	})
	c.retMemo[key] = out
	return out
}

type argvSink struct {
	fn    *argvFunc
	param argvParam
	via   string // the exec call this parameter reaches
}

func (c *argvChecker) check() {
	var queue []argvSink
	seenSink := map[string]bool{}
	push := func(fn *argvFunc, f argvFlow, via string) {
		for _, b := range f.bad {
			c.problems[b+" reaches the command line of "+via] = true
		}
		for p := range f.params {
			k := fmt.Sprintf("%p/%d/%s", fn, p.idx, p.field)
			if !seenSink[k] {
				seenSink[k] = true
				queue = append(queue, argvSink{fn: fn, param: p, via: via})
			}
		}
	}
	for _, fn := range c.funcs {
		ast.Inspect(fn.decl.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if args, ok := isExecCall(fn, x); ok {
					for _, a := range args {
						push(fn, c.expand(fn, a, "", 0, map[string]bool{}), c.pos(x))
					}
				}
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok && id.Name == "sandboxInitSpec" {
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Argv" {
								push(fn, c.expand(fn, kv.Value, "", 0, map[string]bool{}), c.pos(x)+" (the agent's argv)")
							}
						}
					}
				}
			}
			return true
		})
	}
	c.callers = map[*argvFunc][]argvCall{}
	for _, h := range c.funcs {
		ast.Inspect(h.decl.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				for _, g := range c.resolve(h, call) {
					c.callers[g] = append(c.callers[g], argvCall{in: h, call: call})
				}
			}
			return true
		})
	}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, cs := range c.callers[s.fn] {
			for _, a := range callArg(cs.call, s.fn, s.param.idx) {
				push(cs.in, c.expand(cs.in, a, s.param.field, 0, map[string]bool{}), s.via)
			}
		}
	}
}
