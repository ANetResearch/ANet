package a2ashape

// The code register (internal/a2ashape/codes.txt) is the list of every reason
// code ever published. A code is a stable public interface, so this test holds
// the register against the reason table both ways and refuses a retired code
// that comes back. The register is edited by hand; there is no -update flag.

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/version"
)

type codeLine struct {
	code, since, status, retired, replacement string
	n                                         int
}

var releaseRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// releaseNum orders a major.minor.patch version; ok is false for any other form.
func releaseNum(v string) (n [3]int, ok bool) {
	m := releaseRe.FindStringSubmatch(v)
	if m == nil {
		return n, false
	}
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	return n, true
}

func releaseAfter(a, b string) bool {
	x, _ := releaseNum(a)
	y, _ := releaseNum(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

// newestAllowed is the newest version a register line may name: the version
// constant, or the next patch while that release is being prepared (the
// constant is raised only when the release is cut).
func newestAllowed() string {
	n, _ := releaseNum(version.V)
	return fmt.Sprintf("%d.%d.%d", n[0], n[1], n[2]+1)
}

func readCodeRegister(t *testing.T) []codeLine {
	t.Helper()
	f, err := os.Open("codes.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []codeLine
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		l := strings.TrimRight(sc.Text(), " \r")
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) < 3 || len(f) > 5 {
			t.Fatalf("codes.txt:%d: want code, since, status[, retired[, replacement]] separated by tabs, got %q", n, l)
		}
		c := codeLine{code: f[0], since: f[1], status: f[2], n: n}
		if len(f) > 3 {
			c.retired = f[3]
		}
		if len(f) > 4 {
			c.replacement = f[4]
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tableCodes() map[string]bool {
	m := map[string]bool{}
	for _, r := range reasonTable {
		if codeRe.MatchString(r.Code) {
			m[r.Code] = true
		}
	}
	return m
}

func TestCodeRegisterMatchesTheTable(t *testing.T) {
	lines := readCodeRegister(t)
	table := tableCodes()
	active := map[string]bool{}
	seen := map[string]int{}
	for i, c := range lines {
		where := fmt.Sprintf("codes.txt:%d %s", c.n, c.code)
		if !codeRe.MatchString(c.code) {
			t.Errorf("%s: not a code", where)
		}
		if p, dup := seen[c.code]; dup {
			t.Errorf("%s: already listed at line %d", where, p)
		}
		seen[c.code] = c.n
		if i > 0 && lines[i-1].code >= c.code {
			t.Errorf("%s: lines are sorted by code; this one follows %s", where, lines[i-1].code)
		}
		if _, ok := releaseNum(c.since); !ok || releaseAfter(c.since, newestAllowed()) {
			t.Errorf("%s: since %q is not a version up to %s", where, c.since, newestAllowed())
		}
		switch c.status {
		case "active":
			active[c.code] = true
			if c.retired != "" || c.replacement != "" {
				t.Errorf("%s: an active code has no retired version or replacement", where)
			}
		case "retired":
			if _, ok := releaseNum(c.retired); !ok || releaseAfter(c.retired, newestAllowed()) {
				t.Errorf("%s: retired %q is not a version up to %s", where, c.retired, newestAllowed())
			} else if releaseAfter(c.since, c.retired) {
				t.Errorf("%s: retired in %s, before it was published in %s", where, c.retired, c.since)
			}
			if table[c.code] {
				t.Errorf("%s: a retired code has no row in reasons.go; a code is never reused", where)
			}
		default:
			t.Errorf("%s: status %q is neither active nor retired", where, c.status)
		}
		if c.replacement != "" && !codeRe.MatchString(c.replacement) {
			t.Errorf("%s: replacement %q is not a code", where, c.replacement)
		}
	}
	var missing []string
	for c := range table {
		if !active[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	for _, c := range missing {
		if _, listed := seen[c]; listed {
			t.Errorf("%s has a row in reasons.go but is not active in codes.txt", c)
			continue
		}
		t.Errorf("reasons.go has the code %s and codes.txt does not; add this line, in sorted place:\n%s\t%s\tactive",
			c, c, newestAllowed())
	}
	for c := range active {
		if !table[c] {
			t.Errorf("codes.txt lists %s as active and reasons.go has no row for it; if the code was removed, mark it retired with the version and a replacement if any", c)
		}
	}
}
