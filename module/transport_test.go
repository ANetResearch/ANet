package module

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/seal"
)

// A transport carries the recipient's delivery confirmation as opaque bytes
// of the length the seal package makes (core CORE-K11).
func TestDAckLenIsSealDAckLen(t *testing.T) {
	if DAckLen != seal.DAckLen {
		t.Fatalf("module.DAckLen = %d, seal.DAckLen = %d: a transport would cut or pad confirmations", DAckLen, seal.DAckLen)
	}
}

// v2Names are the identifiers of frame v2's delivery claims: a transport
// saying on its own that a message reached nobody (not_delivered), and a
// peer process answering for whether an AID can be reached (the reach
// frame). In v3 only the recipient's confirmation says a message arrived,
// and a transport says only whether bytes left this process.
var v2Names = map[string]bool{
	"ErrNotDelivered": true, "NotDelivered": true, "notDelivered": true, "isNotDelivered": true,
	"reachable": true,
}

// v2Remnants lists what of frame v2's delivery claims one Go file still
// has: an identifier above, the string "reach" (the op), a string with
// not_delivered in it, or a struct field encoded as reachable or
// not_delivered.
func v2Remnants(fset *token.FileSet, f *ast.File) []string {
	var out []string
	tags := map[*ast.BasicLit]bool{} // struct tags, checked as tags and not as strings
	at := func(n ast.Node, what string) {
		out = append(out, fset.Position(n.Pos()).String()+": "+what)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if v2Names[n.Name] || strings.Contains(strings.ToLower(n.Name), "notdelivered") {
				at(n, "identifier "+n.Name)
			}
		case *ast.Field:
			if n.Tag == nil {
				return true
			}
			tags[n.Tag] = true
			tag, err := strconv.Unquote(n.Tag.Value)
			if err != nil {
				return true
			}
			for _, key := range []string{"json", "cbor"} {
				name, _, _ := strings.Cut(reflect.StructTag(tag).Get(key), ",")
				if name == "reachable" || name == "not_delivered" {
					at(n.Tag, "field encoded as "+name)
				}
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING || tags[n] {
				return true
			}
			s, err := strconv.Unquote(n.Value)
			if err != nil {
				return true
			}
			if s == "reach" || strings.Contains(s, "not_delivered") {
				at(n, "string "+strconv.Quote(s))
			}
		}
		return true
	})
	return out
}

// The acceptance of p2p frame v3 (core V12, §1.2): frame v2's delivery
// claims are gone from all code of this repository that is not a test —
// module.ErrNotDelivered, the not_delivered field, the reach frame and its
// reachable answer. Tests may still name them, to say they are gone.
func TestNoFrameV2DeliveryClaims(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the repository root: %v", root, err)
	}
	fset := token.NewFileSet()
	var found []string
	files := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		found = append(found, v2Remnants(fset, f)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("only %d Go files under %s: the walk did not see the repository", files, root)
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("frame v2 remnant: %s", f)
	}
}

// The check above finds each kind of remnant, and leaves alone words that
// only look like them.
func TestNoFrameV2DeliveryClaimsCatches(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{`var ErrNotDelivered = 1`, 1},
		{`type f struct{ NotDelivered bool }`, 1},
		{`type notDelivered struct{}`, 1},
		{`func isNotDelivered() {}`, 1},
		{`func (p *peer) reachable() {}`, 1},
		{`var x = "reach"`, 1},
		{`var x = "send not_delivered"`, 1},
		{"type f struct{ R bool `json:\"reachable,omitempty\"` }", 1},
		{"type f struct{ N bool `json:\"not_delivered\"` }", 1},
		{"type f struct{ NotDelivered bool `json:\"not_delivered,omitempty\"` }", 2},
		// Not remnants.
		{`func Reachable() {}`, 0},
		{`var x = "reachable over the hub"`, 0},
		{`var x = "home_unreachable"`, 0},
		{`var unreachable = 1`, 0},
		{"type f struct{ R bool `json:\"reach_ok\"` }", 0},
		{`// reachable, not_delivered and "reach" in a comment`, 0},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", "package x\n"+tc.src+"\n", parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		if got := v2Remnants(fset, f); len(got) != tc.want {
			t.Errorf("%s: found %v, want %d", tc.src, got, tc.want)
		}
	}
}
