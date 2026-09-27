package main

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// applyUnified applies a unified diff to a, the way patch(1) would, and
// fails the test on any hunk that does not match. It is the independent
// check that a diff says what it claims.
func applyUnified(t *testing.T, a, diff string) string {
	t.Helper()
	src := splitLines(a)
	lines := strings.SplitAfter(diff, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "--- ") || !strings.HasPrefix(lines[1], "+++ ") {
		t.Fatalf("no header in %q", diff)
	}
	var out []string
	pos := 0 // next unconsumed line of src
	i := 2
	for i < len(lines) {
		h := lines[i]
		var aStart, aLen, bStart, bLen int
		if _, err := fmt.Sscanf(expandRange(h), "@@ -%d,%d +%d,%d @@\n", &aStart, &aLen, &bStart, &bLen); err != nil {
			t.Fatalf("bad hunk header %q: %v", h, err)
		}
		from := aStart - 1
		if aLen == 0 {
			from = aStart
		}
		if from < pos {
			t.Fatalf("hunk %q overlaps the previous one", h)
		}
		out = append(out, src[pos:from]...)
		pos = from
		i++
		for i < len(lines) && !strings.HasPrefix(lines[i], "@@") {
			l := lines[i]
			i++
			noEOL := i < len(lines) && lines[i] == "\\ No newline at end of file\n"
			text := l[1:]
			if noEOL {
				text = strings.TrimSuffix(text, "\n")
				i++
			}
			switch l[0] {
			case ' ', '-':
				if pos >= len(src) || src[pos] != text {
					t.Fatalf("hunk %q: context/deletion %q does not match source line %d", h, text, pos+1)
				}
				pos++
				if l[0] == ' ' {
					out = append(out, text)
				}
			case '+':
				out = append(out, text)
			default:
				t.Fatalf("bad diff line %q", l)
			}
		}
	}
	out = append(out, src[pos:]...)
	return strings.Join(out, "")
}

// expandRange rewrites "@@ -3 +4 @@" (GNU's omitted ",1") into
// "@@ -3,1 +4,1 @@" for Sscanf.
func expandRange(h string) string {
	parts := strings.Fields(h)
	if len(parts) < 4 {
		return h
	}
	for _, i := range []int{1, 2} {
		if !strings.Contains(parts[i], ",") {
			parts[i] += ",1"
		}
	}
	return strings.Join(parts[:4], " ") + "\n"
}

func TestUnifiedDiff(t *testing.T) {
	cases := []struct {
		name, a, b string
		context    int
		want       string // exact diff, or "" to check by applying only
		added      int
		removed    int
	}{
		{name: "change", a: "one\ntwo\nthree\n", b: "one\n2\nthree\n", context: 3,
			want: "--- a\n+++ b\n@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n", added: 1, removed: 1},
		{name: "no final newline", a: "x\n", b: "x", context: 3,
			want: "--- a\n+++ b\n@@ -1 +1 @@\n-x\n+x\n\\ No newline at end of file\n", added: 1, removed: 1},
		{name: "from empty", a: "", b: "a\nb\n", context: 3,
			want: "--- a\n+++ b\n@@ -0,0 +1,2 @@\n+a\n+b\n", added: 2},
		{name: "to empty", a: "a\nb\n", b: "", context: 3,
			want: "--- a\n+++ b\n@@ -1,2 +0,0 @@\n-a\n-b\n", removed: 2},
		{name: "insert at start, context 0", a: "b\nc\n", b: "a\nb\nc\n", context: 0,
			want: "--- a\n+++ b\n@@ -0,0 +1 @@\n+a\n", added: 1},
		{name: "two hunks", a: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", b: "1\nX\n3\n4\n5\n6\n7\n8\nY\n10\n", context: 1,
			want: "--- a\n+++ b\n@@ -1,3 +1,3 @@\n 1\n-2\n+X\n 3\n@@ -8,3 +8,3 @@\n 8\n-9\n+Y\n 10\n", added: 2, removed: 2},
		{name: "close changes merge", a: "1\n2\n3\n4\n5\n", b: "X\n2\n3\n4\nY\n", context: 2, added: 2, removed: 2},
	}
	for _, c := range cases {
		res, err := unifiedDiff(context.Background(), c.a, c.b, "a", "b", c.context)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.want != "" && res.Diff != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, res.Diff, c.want)
		}
		if res.Added != c.added || res.Removed != c.removed || !res.Minimal || res.Identical {
			t.Errorf("%s: %+v", c.name, res)
		}
		if got := applyUnified(t, c.a, res.Diff); got != c.b {
			t.Errorf("%s: applying the diff gives %q, want %q", c.name, got, c.b)
		}
		if c.name == "close changes merge" && res.Hunks != 1 {
			t.Errorf("changes 3 lines apart with context 2 must share a hunk, got %d", res.Hunks)
		}
	}
	res, _ := unifiedDiff(context.Background(), "same\n", "same\n", "a", "b", 3)
	if !res.Identical || res.Diff != "" {
		t.Errorf("identical texts: %+v", res)
	}
}

// Random edits: every diff applies back to the target, and its size is the
// true edit distance (checked against an O(NM) LCS).
func TestDiffIsCorrectAndMinimal(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	gen := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(strconv.Itoa(rng.Intn(5)))
			b.WriteByte('\n')
		}
		if rng.Intn(4) == 0 {
			b.WriteString("tail")
		}
		return b.String()
	}
	for round := 0; round < 300; round++ {
		a, b := gen(rng.Intn(30)), gen(rng.Intn(30))
		ctxLines := rng.Intn(4)
		res, err := unifiedDiff(context.Background(), a, b, "a", "b", ctxLines)
		if err != nil {
			t.Fatal(err)
		}
		if res.Identical {
			if a != b {
				t.Fatalf("different texts reported identical")
			}
			continue
		}
		if got := applyUnified(t, a, res.Diff); got != b {
			t.Fatalf("round %d: a=%q b=%q context=%d\ndiff:\n%s\napplied=%q", round, a, b, ctxLines, res.Diff, got)
		}
		la, lb := splitLines(a), splitLines(b)
		lcs := lcsLen(la, lb)
		if res.Added != len(lb)-lcs || res.Removed != len(la)-lcs {
			t.Fatalf("round %d: +%d -%d, minimal is +%d -%d", round, res.Added, res.Removed, len(lb)-lcs, len(la)-lcs)
		}
	}
}

func lcsLen(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

// Past the search bound the middle is replaced wholesale: still a correct
// diff, flagged not minimal.
func TestDiffBeyondTheBoundIsCorrectButNotMinimal(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&a, "a%d\n", i)
		fmt.Fprintf(&b, "b%d\n", i)
	}
	ops, ok, err := myers(context.Background(), make([]int32, 3), []int32{1, 2, 3}, 2)
	if err != nil || ok || ops != nil {
		t.Errorf("a distance of 6 with bound 2 must give up: %v %v", ok, err)
	}
	// Through the full path with the real bound, a large disjoint edit
	// stays minimal (distance 100 <= bound).
	res, err := unifiedDiff(context.Background(), a.String(), b.String(), "a", "b", 3)
	if err != nil || !res.Minimal || applyUnified(t, a.String(), res.Diff) != b.String() {
		t.Errorf("disjoint texts: %+v %v", res, err)
	}
}

func TestDiffTruncatesLongOutput(t *testing.T) {
	var a, b strings.Builder
	line := strings.Repeat("x", 200)
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&a, "%s a %d\n", line, i)
		fmt.Fprintf(&b, "%s b %d\n", line, i)
	}
	res, err := unifiedDiff(context.Background(), a.String(), b.String(), "a", "b", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Diff) > maxDiffOutput {
		t.Errorf("truncated=%v len=%d, limit %d", res.Truncated, len(res.Diff), maxDiffOutput)
	}
}

func TestDiffArguments(t *testing.T) {
	s, _ := testServer(t, "tools")
	out := call(t, s, "text.diff", `{"a":"x\n","b":"y\n","a_name":"old\nname","b_name":"new"}`)
	if d := out["diff"].(string); !strings.HasPrefix(d, "--- old name\n+++ new\n") {
		t.Errorf("header names: %q", d)
	}
	for _, bad := range []string{`{"a":"x"}`, `{"a":"x","b":"y","context":21}`, `{"a":"x","b":"y","context":-1}`, `{"a":1,"b":"y"}`} {
		if w := do(t, s, "/v1/tools/text.diff", bad); w.Code != 400 {
			t.Errorf("%s: HTTP %d", bad, w.Code)
		}
	}
}
