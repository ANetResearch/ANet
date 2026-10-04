package daemon

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"unicode"
)

// SI-38 and KEL C-58 for ANet's code, read from its source.

// recoveryOnlyCLI are the identity functions that hold a recovery seed
// (KEL C-35): only the anet CLI may name them (ANetCore's
// TestRecoveryAPIScope checks the same of all three repositories; this is
// the daemon's own check, M22, M44).
var recoveryOnlyCLI = map[string]bool{
	"OpenRecoveryFile":      true,
	"RecoverySeed":          true,
	"OpenRecoveryFileLocal": true,
	"SealRecoveryFileLocal": true,
}

// recoveryNames are the file names of a recovery file and of the key of
// the one without a passphrase (KEL §2.4, C-36). Code outside the CLI that
// spells them would be about to open one.
var recoveryNames = []string{"recovery_local", ".anetrec", ".anet-recovery"}

// recoveryNameExempt are the files outside the CLI that may spell a name,
// and why.
var recoveryNameExempt = map[string]string{
	// The sentence identity.recovery_file_note_unprotected names
	// recovery_local.key for the user (ux §1.4 M).
	"internal/a2ashape/reasons.go": "a sentence for the user",
}

const identityPkg = "github.com/ANetResearch/ANetCore/identity"

// recoveryScopeProblems is every reference to a function of recoveryOnlyCLI,
// and every string literal holding a name of recoveryNames, in the non-test
// Go files of fsys (the ANet repository) outside cmd/anet.
func recoveryScopeProblems(fsys fs.FS) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := e.Name()
		if e.IsDir() {
			if p != "." && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return fs.SkipDir
			}
			if p == "cmd/anet" {
				return fs.SkipDir // the CLI: KEL C-35 lets it
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("%s: %v", p, err)
		}
		local := ""
		for _, im := range f.Imports {
			if path, _ := strconv.Unquote(im.Path.Value); path == identityPkg {
				local = "identity"
				if im.Name != nil {
					local = im.Name.Name
				}
			}
		}
		if local == "." {
			out = append(out, p+" dot-imports identity")
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && local != "" && id.Name == local && recoveryOnlyCLI[x.Sel.Name] {
					out = append(out, fmt.Sprintf("%s: identity.%s, which only the anet CLI may use (KEL C-35, SI-38)",
						fset.Position(x.Pos()), x.Sel.Name))
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(x.Value)
				if err != nil {
					return true
				}
				for _, nm := range recoveryNames {
					if strings.Contains(s, nm) && recoveryNameExempt[filepath.ToSlash(p)] == "" {
						out = append(out, fmt.Sprintf("%s: the string %q names a recovery file (KEL D-2: the daemon never opens one)",
							fset.Position(x.Pos()), s))
					}
				}
			}
			return true
		})
		return nil
	})
	return out, err
}

// runTimeRecoveryCheck is the run-time half of
// TestDaemonNeverOpensRecoveryFile, where the system can watch file opens
// (identity_scope_linux_test.go).
var runTimeRecoveryCheck func(t *testing.T)

// M22, M44 (KEL D-2, SI-38): nothing in ANet but the CLI names a function
// that holds a recovery seed, or the name of a recovery file (source); and
// through a whole life the daemon process opens none of the recovery files
// (run time).
func TestDaemonNeverOpensRecoveryFile(t *testing.T) {
	t.Run("source", func(t *testing.T) {
		probs, err := recoveryScopeProblems(os.DirFS(anetRoot(t)))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range probs {
			t.Error(p)
		}
	})
	t.Run("run time", func(t *testing.T) {
		if runTimeRecoveryCheck == nil {
			t.Skip("watching file opens needs inotify, which only Linux has")
		}
		runTimeRecoveryCheck(t)
	})
}

// The scan finds what it must and leaves alone what it must.
func TestRecoveryScopeScanCatches(t *testing.T) {
	file := func(imp, body string) []byte {
		return []byte("package x\n\nimport " + imp + "\n\nfunc f() { " + body + " }\n")
	}
	q := strconv.Quote(identityPkg)
	for _, c := range []struct {
		path string
		src  []byte
		bad  bool
	}{
		{"internal/daemon/x.go", file(q, "identity.OpenRecoveryFile(nil, nil)"), true},
		{"internal/daemon/x.go", file(q, "_ = identity.RecoverySeed"), true},
		{"internal/daemon/x.go", file("id "+q, "_ = id.OpenRecoveryFileLocal"), true},
		{"module/a2a/x.go", file(q, "_ = identity.SealRecoveryFileLocal"), true},
		{"internal/daemon/x.go", file(". "+q, "_ = 1"), true},
		{"internal/daemon/x.go", file(`"os"`, `os.ReadFile("recovery_local.key")`), true},
		{"internal/daemon/x.go", file(`"os"`, `os.ReadFile("x/y.anetrec")`), true},
		{"internal/daemon/x.go", file(`"os"`, `os.ReadFile("/h/.anet-recovery/a")`), true},
		{"cmd/anet/x.go", file(q, "identity.OpenRecoveryFileLocal(nil, [32]byte{})"), false},
		{"cmd/anet/x.go", file(`"os"`, `os.ReadFile("recovery_local.key")`), false},
		{"internal/daemon/x_test.go", file(q, "identity.OpenRecoveryFile(nil, nil)"), false},
		{"internal/daemon/x.go", file(q, "_ = identity.ParseRecoveryFile"), false},
		{"internal/a2ashape/reasons.go", file(`"fmt"`, `fmt.Print("recovery_local.key")`), false},
	} {
		probs, err := recoveryScopeProblems(fstest.MapFS{c.path: &fstest.MapFile{Data: c.src}})
		if err != nil {
			t.Fatal(err)
		}
		if bad := len(probs) > 0; bad != c.bad {
			t.Errorf("%s %s: %q, want bad = %v", c.path, c.src, probs, c.bad)
		}
	}
}

// identWords splits an identifier at underscores and case changes, as
// ANetCore's KEL C-58 check does: ksnCache, KSNCache and ksn_cache hold the
// word ksn; ksnow does not.
func identWords(name string) []string {
	var out []string
	rs := []rune(name)
	start := 0
	flush := func(i int) {
		if i > start {
			out = append(out, string(rs[start:i]))
		}
		start = i
	}
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '_':
			flush(i)
			start = i + 1
		case i > start && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]) ||
			i+1 < len(rs) && unicode.IsUpper(rs[i-1]) && unicode.IsLower(rs[i+1])):
			flush(i)
		}
	}
	flush(len(rs))
	return out
}

// ksnProblems lists the identifiers holding the word ksn, and the JSON or
// CBOR fields tagged ksn, in the non-test Go files of fsys.
func ksnProblems(fsys fs.FS) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, ".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if n := e.Name(); p != "." && (strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") || n == "testdata" || n == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("%s: %v", p, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				for _, w := range identWords(x.Name) {
					if strings.EqualFold(w, "ksn") {
						out = append(out, fmt.Sprintf("%s: identifier %s", fset.Position(x.Pos()), x.Name))
						break
					}
				}
			case *ast.Field:
				if x.Tag == nil {
					return true
				}
				tag, err := strconv.Unquote(x.Tag.Value)
				if err != nil {
					return true
				}
				for _, key := range []string{"json", "cbor"} {
					if name, _, _ := strings.Cut(reflect.StructTag(tag).Get(key), ","); strings.EqualFold(name, "ksn") {
						out = append(out, fmt.Sprintf("%s: %s field tagged %q", fset.Position(x.Pos()), key, name))
					}
				}
			}
			return true
		})
		return nil
	})
	return out, err
}

// KEL C-58 (core §0.3, G8) for ANet: no identifier of the non-test code is
// named ksn or has ksn as one of its words, and no JSON or CBOR field is
// tagged ksn. The term is "witness notice" in code, "key confirmation" for
// users; a key_state_seq is a key state seq.
func TestNoKSNIdentifiers(t *testing.T) {
	probs, err := ksnProblems(os.DirFS(anetRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probs {
		t.Error(p)
	}
}

// The KSN scan finds what it must.
func TestKSNScanCatches(t *testing.T) {
	for src, bad := range map[string]bool{
		"package x\nvar ksn = 1\n":                                  true,
		"package x\nfunc f(lastKSN int) {}\n":                       true,
		"package x\ntype T struct{ ksnCache int }\n":                true,
		"package x\ntype T struct{ A int `json:\"ksn\"` }":          true,
		"package x\ntype T struct{ A int `cbor:\"KSN,keyasint\"` }": true,
		"package x\nvar ksnow, keySeq = 1, 2\n":                     false,
	} {
		probs, err := ksnProblems(fstest.MapFS{"x.go": &fstest.MapFile{Data: []byte(src)}})
		if err != nil {
			t.Fatal(err)
		}
		if (len(probs) > 0) != bad {
			t.Errorf("%q: %q, want bad = %v", src, probs, bad)
		}
	}
	if probs, _ := ksnProblems(fstest.MapFS{"x_test.go": &fstest.MapFile{Data: []byte("package x\nvar ksn = 1\n")}}); len(probs) != 0 {
		t.Errorf("a test file was scanned: %q", probs)
	}
}
