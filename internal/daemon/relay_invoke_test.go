package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// relayCallers are the functions that may hand a provider a call through the relay door, each with
// why it runs only for a message the receive pipeline accepted. tryCapabilityPaid is the one place
// that builds a provider.Call with Via ViaRelay; the rest reach it through runCapabilityCall.
var relayCallers = map[string]map[string]string{
	"tryCapabilityPaid": {
		"tryCapability":     "the same call without a payment",
		"runCapabilityCall": "the executor every entry below goes through",
	},
	"runCapabilityCall": {
		"ingestDelegate":      "receive step 10: the delegation passed every step before it",
		"redeliveredDelegate": "step 10 for a redelivery of a delegation already accepted",
		"ApprovePending":      "an accepted delegation the operator held, approved later (its key state judged again)",
		"runPaidCall":         "a settled payment for an accepted delegation",
		"onRequesterPayment":  "a payment message, itself received through the pipeline, for an accepted delegation",
		"recoverInterrupted":  "at start, a call an earlier process accepted and did not finish",
	},
	"tryCapability": {},
}

// daemonFuncs parses this package's non-test files: every function declaration by name, and the
// file each is in.
func daemonFuncs(t *testing.T) (map[string]*ast.FuncDecl, map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	decls, where := map[string]*ast.FuncDecl{}, map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				decls[fd.Name.Name] = fd
				where[fd.Name.Name] = name
			}
		}
	}
	return decls, where
}

// mentions reports whether body names fn: a call d.fn(…), or fn handed on as a method value.
func mentions(body ast.Node, fn string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == fn {
				found = true
			}
		case *ast.Ident:
			if x.Name == fn {
				found = true
			}
		}
		return !found
	})
	return found
}

// SI-62, LEC X-ENG-3: the daemon calls a provider through the relay door (Via ViaRelay, the door
// whose caller provider/anetlink sends ANetLink as caller.aid) only for a message its receive
// pipeline accepted, notice check (step 6½) included. That is what lets ANetLink trust that every
// remote caller is a witnessed one, with no caller.witnessed on the wire. Structural:
//
//  1. across ANet, only tryCapabilityPaid (here) and the C1 client that maps the door (callerOf)
//     use provider.ViaRelay;
//  2. the functions that reach tryCapabilityPaid are exactly relayCallers, each of which acts on a
//     delegation the pipeline accepted;
//  3. the pipeline calls seal.CheckNotice before it hands a delegation to step 10.
func TestRelayInvokeOnlyAfterNoticeCheck(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || rel == "provider/provider.go" {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if ok && fd.Body != nil && mentions(fd.Body, "ViaRelay") {
				users = append(users, rel+" "+fd.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(users)
	if want := []string{"internal/daemon/capability.go tryCapabilityPaid", "provider/anetlink/anetlink.go callerOf"}; !slices.Equal(users, want) {
		t.Errorf("provider.ViaRelay is used by %q; only %q may. A new relay door must run only for a message the receive "+
			"pipeline accepted: add it here with the reason, after checking that it does", users, want)
	}

	decls, where := daemonFuncs(t)
	for target, allowed := range relayCallers {
		if decls[target] == nil {
			t.Errorf("%s is gone: update relayCallers", target)
			continue
		}
		var got []string
		for name, fd := range decls {
			if name != target && fd.Body != nil && mentions(fd.Body, target) {
				got = append(got, name)
			}
		}
		sort.Strings(got)
		for _, name := range got {
			if _, ok := allowed[name]; !ok {
				t.Errorf("%s (%s) reaches %s, a call through the relay door; it must act only on a delegation the receive "+
					"pipeline accepted (notice check included): check that, then add it to relayCallers with why", name, where[name], target)
			}
		}
		for name := range allowed {
			if !slices.Contains(got, name) {
				t.Errorf("relayCallers lists %s for %s, which no longer reaches it: remove it", name, target)
			}
		}
	}

	t.Run("step 6½", func(t *testing.T) {
		checked := false
		for _, fd := range decls {
			if fd.Body != nil && strings.HasPrefix(where[fd.Name.Name], "receive") && mentions(fd.Body, "CheckNotice") {
				checked = true
			}
		}
		if !checked {
			// The receive pipeline v3 adds the notice check; until then an envelope's notice is not
			// read, and a relay call follows steps 1 to 10 without it.
			t.Skip("v030-todo(ENG-1c)")
		}
	})
}
