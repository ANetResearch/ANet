package main

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The build tags are written down in four places: the switch files here
// (module_<m>.go), the two lists in scripts/tagcheck.sh, the opt-in
// declaration in optin_tags.go, and the CI matrix. Each checks something
// the others cannot, and each goes quiet when it drifts from the rest: a
// module whose tag is missing from tagcheck.sh is never symbol-checked by
// `build.sh --check`; one with no CI row is never checked in CI; an
// additive tag nobody declared makes a configured node's refusal point at
// a `no_<m>` that does not exist; and a distribution row that stopped
// being built says nothing at all. These tests read the sources, not the
// build, so they give the same answer under every tag set CI runs them in.

var (
	repoRoot    = filepath.Join("..", "..")
	pluggableRE = regexp.MustCompile(`(?m)^\s*- "([a-z0-9_,]+)"\s*$`)
	optinRE     = regexp.MustCompile(`(?m)^\s*tag: \[([^\]]*)\]\s*$`)
	profileRE   = regexp.MustCompile(`(?m)^\s*\[([a-z]+)\]="([a-z0-9_,]+)"`)
)

// tagLists reads SUBTRACTIVE and ADDITIVE from scripts/tagcheck.sh.
func tagLists(t *testing.T) (sub, add map[string]bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "tagcheck.sh"))
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) map[string]bool {
		m := regexp.MustCompile(`(?m)^` + name + `="([^"]*)"`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("scripts/tagcheck.sh has no %s=\"…\" line", name)
		}
		out := map[string]bool{}
		for _, f := range strings.Fields(string(m[1])) {
			out[f] = true
		}
		return out
	}
	return read("SUBTRACTIVE"), read("ADDITIVE")
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Every switch file is `//go:build !no_<m>` or `//go:build <m>`, and its
// <m> is in the list for that direction.
func TestEveryModuleSwitchIsInTheTagCheckLists(t *testing.T) {
	sub, add := tagLists(t)
	files, err := filepath.Glob("module_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no module_*.go switch files found (%v)", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var expr constraint.Expr
		for _, line := range strings.Split(string(b), "\n") {
			if constraint.IsGoBuild(line) {
				if expr, err = constraint.Parse(line); err != nil {
					t.Fatalf("%s: %v", f, err)
				}
				break
			}
		}
		switch e := expr.(type) {
		case *constraint.NotExpr:
			tag, ok := e.X.(*constraint.TagExpr)
			if !ok || !strings.HasPrefix(tag.Tag, "no_") {
				t.Errorf("%s: //go:build %s is neither !no_<m> nor <m>", f, expr)
				continue
			}
			if m := strings.TrimPrefix(tag.Tag, "no_"); !sub[m] {
				t.Errorf("%s is switched by %s, but %q is not in SUBTRACTIVE in scripts/tagcheck.sh %v",
					f, tag.Tag, m, keys(sub))
			}
		case *constraint.TagExpr:
			if !add[e.Tag] {
				t.Errorf("%s is switched by the additive tag %s, but it is not in ADDITIVE in scripts/tagcheck.sh %v",
					f, e.Tag, keys(add))
			}
		default:
			t.Errorf("%s: a switch file needs //go:build !no_<m> or //go:build <m>, got %v", f, expr)
		}
	}
}

// optin_tags.go declares exactly the additive tags.
func TestTheOptInDeclarationNamesEveryAdditiveTag(t *testing.T) {
	_, add := tagLists(t)
	file, err := parser.ParseFile(token.NewFileSet(), "optin_tags.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "DeclareOptIn" {
			return true
		}
		for _, a := range call.Args {
			lit, ok := a.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("DeclareOptIn takes string literals here, got %T", a)
			}
			s, _ := strconv.Unquote(lit.Value)
			declared[s] = true
		}
		return true
	})
	if got, want := strings.Join(keys(declared), " "), strings.Join(keys(add), " "); got != want {
		t.Fatalf("optin_tags.go declares [%s]; scripts/tagcheck.sh ADDITIVE is [%s]", got, want)
	}
}

// CI checks every subtractive tag on a row of its own, every additive tag
// in the optin job, and names no tag the check does not know — a stale
// `no_taskboard` row would build the default binary and pass forever.
func TestCIChecksEveryTagOnARowOfItsOwn(t *testing.T) {
	sub, add := tagLists(t)
	b, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]bool{}
	for _, m := range pluggableRE.FindAllSubmatch(b, -1) {
		row := string(m[1])
		rows[row] = true
		for _, tag := range strings.Split(row, ",") {
			if !sub[strings.TrimPrefix(tag, "no_")] || !strings.HasPrefix(tag, "no_") {
				t.Errorf("pluggable row %q names %q, which is not a subtractive tag in scripts/tagcheck.sh", row, tag)
			}
		}
	}
	for _, m := range keys(sub) {
		if !rows["no_"+m] {
			t.Errorf("no pluggable row removes %s alone (want a row \"no_%s\")", m, m)
		}
	}
	// Rows compare as sets: the order of tags in a row is not a property.
	canon := func(set string) string {
		s := strings.Split(set, ",")
		sort.Strings(s)
		return strings.Join(s, ",")
	}
	built := map[string]bool{}
	for row := range rows {
		built[canon(row)] = true
	}
	all := make([]string, 0, len(sub))
	for _, m := range keys(sub) {
		all = append(all, "no_"+m)
	}
	if !built[canon(strings.Join(all, ","))] {
		t.Errorf("no pluggable row removes every subtractive module at once (%s)", strings.Join(all, ","))
	}
	// The profiles scripts/onboard.sh builds are the shipping variants of
	// docs/DISTRIBUTIONS-zh.md. `min` was the all-out row until no_a2a
	// joined that row, and then no row built it.
	ob, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "onboard.sh"))
	if err != nil {
		t.Fatal(err)
	}
	profiles := profileRE.FindAllSubmatch(ob, -1)
	if len(profiles) == 0 {
		t.Fatal(`scripts/onboard.sh has no [<profile>]="<tags>" lines`)
	}
	for _, p := range profiles {
		if !built[canon(string(p[2]))] {
			t.Errorf("the %s profile scripts/onboard.sh builds (%s) is no pluggable row", p[1], p[2])
		}
	}

	m := optinRE.FindSubmatch(b)
	if m == nil {
		t.Fatal(`ci.yml has no optin matrix line (tag: ["…"])`)
	}
	optin := map[string]bool{}
	for _, f := range strings.Split(string(m[1]), ",") {
		if f = strings.Trim(strings.TrimSpace(f), `"`); f != "" {
			optin[f] = true
		}
	}
	if got, want := strings.Join(keys(optin), " "), strings.Join(keys(add), " "); got != want {
		t.Errorf("the optin job checks [%s]; scripts/tagcheck.sh ADDITIVE is [%s]", got, want)
	}
}
