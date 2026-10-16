package main

// UX-T5 (ux UX-C5): the CLI words no reason of its own. A reason a person
// reads comes from the reason table (the control plane's error, or
// a2ashape.Sentence), in the language of the process; an error the CLI
// writes itself is a usage line or a fact about the command line, in
// English. So no string literal given to fmt.Errorf or errors.New in the
// non-test code of cmd/anet contains a CJK character: a Chinese literal
// there is a reason written outside the table, in one language only.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// cjk reports a character of the CJK scripts or the CJK punctuation and full-width forms.
func cjk(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) ||
		(r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

// errorLiterals is every string literal passed, directly or in a concatenation, to fmt.Errorf or
// errors.New in the Go files of dir that are not tests: file:line and its value.
func errorLiterals(t *testing.T, dir string) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || !((pkg.Name == "fmt" && sel.Sel.Name == "Errorf") || (pkg.Name == "errors" && sel.Sel.Name == "New")) {
				return true
			}
			for _, arg := range call.Args {
				ast.Inspect(arg, func(m ast.Node) bool {
					if lit, ok := m.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, err := strconv.Unquote(lit.Value)
						if err != nil {
							v = lit.Value
						}
						out[fset.Position(lit.Pos()).String()] = v
					}
					return true
				})
			}
			return true
		})
	}
	return out
}

func TestCLIErrorsHaveNoCJK(t *testing.T) {
	lits := errorLiterals(t, ".")
	if len(lits) < 50 {
		t.Fatalf("only %d error literals found in cmd/anet — the scan is probably broken", len(lits))
	}
	for pos, v := range lits {
		for _, r := range v {
			if cjk(r) {
				t.Errorf("%s: an error literal in Chinese, a reason worded outside the reason table (ux UX-C5): %q", pos, v)
				break
			}
		}
	}
}

// The scan finds what it is meant to find: a Chinese literal in each form, and nothing else.
func TestErrorLiteralScan(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import (
	"errors"
	"fmt"
)

var a = fmt.Errorf("对方拒绝了")
var b = errors.New("x" + "（未验证）")
var c = fmt.Errorf("%s: %w", "ok", nil)
var d = fmt.Sprintf("这不是错误")
`
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, v := range errorLiterals(t, dir) {
		if strings.ContainsFunc(v, cjk) {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("found %d Chinese error literals, want 2", got)
	}
}
