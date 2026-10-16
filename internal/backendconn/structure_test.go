package backendconn

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// unixDialExempt are the files outside backendconn and tools/ that may dial a Unix socket themselves,
// how many times, and why none of them hands a backend or a peer process anything this package would
// have to vouch for. Every other dial is a way around the path and listener checks (LEC PC-3).
var unixDialExempt = map[string]struct {
	n   int
	why string
}{
	// Inside the sandbox, not the daemon: the bridge from the agent's 127.0.0.1:3128 to the run's
	// egress socket, which the daemon itself made in the run's own directory. It carries the agent's
	// bytes; the daemon's key and its decisions stay on the other side (EXE-2).
	"module/autoreply/sandbox_init.go": {1, "the sandbox's egress bridge"},
	// Inside the sandbox: the isolation probe, which must fail to reach the host's abstract socket.
	"module/autoreply/sandbox_linux.go": {1, "the sandbox probe"},
	// removeStaleSocket asks whether anyone still listens on a path before replacing it, and writes
	// nothing to what answers.
	"cmd/anet-official/main.go": {1, "a stale-socket probe"},
}

// PC-3: everything in ANet that dials a Unix socket goes through DialUnix (or Transport, which uses
// it) — the service module's and module/a2a's backends, the C1 client, the p2p module's peer process —
// so that no one of them is handed to a socket whose path or listener nobody checked. Outside this
// package only tools/ (test fixtures and the peer process, which is the listener) and tests dial
// directly. A dial is any Dial, DialTimeout, DialContext or DialUnix call given a "unix" network
// literal; every file is read, whatever its build tags.
func TestNothingDialsAUnixSocketAroundBackendconn(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor"),
				rel == "tools", rel == "internal/backendconn":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		for _, at := range unixDials(t, path) {
			found[rel] = append(found[rel], at)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0, len(found))
	for f := range found {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		if ex, ok := unixDialExempt[f]; ok && len(found[f]) == ex.n {
			continue
		}
		t.Errorf("%s dials a Unix socket itself at %s: dial through backendconn.Rules.DialUnix, whose path and listener checks it would skip (LEC PC-3)",
			f, strings.Join(found[f], ", "))
	}
	for f, ex := range unixDialExempt {
		if len(found[f]) != ex.n {
			t.Errorf("%s (%s) is exempt for %d dial(s) and has %d: shrink the exemption", f, ex.why, ex.n, len(found[f]))
		}
	}
}

// unixDials are the lines of path that call a Dial function with a Unix network literal.
func unixDials(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var at []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			name = fn.Name
		}
		switch name {
		case "Dial", "DialTimeout", "DialContext", "DialUnix":
		default:
			return true
		}
		for _, a := range call.Args {
			lit, ok := a.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && (v == "unix" || v == "unixgram" || v == "unixpacket") {
				at = append(at, "line "+strconv.Itoa(fset.Position(call.Pos()).Line))
				break
			}
		}
		return true
	})
	return at
}
