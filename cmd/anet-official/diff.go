package main

import (
	"context"
	"fmt"
	"strings"
)

type diffArgs struct {
	A       *string `json:"a"`
	B       *string `json:"b"`
	Context *int    `json:"context"`
	AName   string  `json:"a_name"`
	BName   string  `json:"b_name"`
}

type diffResult struct {
	Identical bool   `json:"identical"`
	Diff      string `json:"diff"`
	Hunks     int    `json:"hunks"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	// Minimal is false when the edit distance exceeded the search bound and
	// the middle of the texts was replaced wholesale. The diff is still
	// correct (applying it to a gives b); it is only longer than needed.
	Minimal   bool `json:"minimal"`
	Truncated bool `json:"truncated"`
}

const (
	defaultDiffContext = 3
	maxDiffContext     = 20
	// maxDiffOutput bounds the diff text. The service module takes replies
	// up to 1 MiB; a quarter of that is more diff than any reader wants.
	maxDiffOutput = 256 << 10
	// maxEditDistance bounds the Myers search. Its trace costs D² words of
	// memory; past this bound the remaining middle is replaced wholesale.
	maxEditDistance = 2000
)

func handleTextDiff(ctx context.Context, _ *env, body []byte) (any, error) {
	var a diffArgs
	if err := decodeArgs(body, &a); err != nil {
		return nil, err
	}
	if a.A == nil || a.B == nil {
		return nil, badArgs("\"a\" and \"b\" are required")
	}
	c := defaultDiffContext
	if a.Context != nil {
		c = *a.Context
	}
	if c < 0 || c > maxDiffContext {
		return nil, badArgs("\"context\" must be 0-%d", maxDiffContext)
	}
	return unifiedDiff(ctx, *a.A, *a.B, headerName(a.AName, "a"), headerName(a.BName, "b"), c)
}

// headerName makes a label safe for a one-line diff header.
func headerName(s, def string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" {
		return def
	}
	return truncateUTF8(s, 256)
}

// splitLines splits s into lines, each keeping its "\n". A final line
// without one is kept as is, so "x\n" and "x" do not compare equal.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

type opKind uint8

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

// editOp is one step of the edit script: an equal pair (a[ai], b[bi]), a
// deletion of a[ai], or an insertion of b[bi].
type editOp struct {
	kind   opKind
	ai, bi int
}

func unifiedDiff(ctx context.Context, sa, sb, aName, bName string, context int) (*diffResult, error) {
	res := &diffResult{Minimal: true}
	if sa == sb {
		res.Identical = true
		return res, nil
	}
	a, b := splitLines(sa), splitLines(sb)
	ops, minimal, err := diffLines(ctx, a, b)
	if err != nil {
		return nil, err
	}
	res.Minimal = minimal

	var out strings.Builder
	out.WriteString("--- " + aName + "\n+++ " + bName + "\n")
	for _, h := range hunksOf(ops, context) {
		var hb strings.Builder
		aStart, aLen, bStart, bLen := h.span(ops)
		fmt.Fprintf(&hb, "@@ -%s +%s @@\n", rangeOf(aStart, aLen), rangeOf(bStart, bLen))
		added, removed := 0, 0
		for _, op := range ops[h.from:h.to] {
			switch op.kind {
			case opEqual:
				writeDiffLine(&hb, ' ', a[op.ai])
			case opDelete:
				writeDiffLine(&hb, '-', a[op.ai])
				removed++
			case opInsert:
				writeDiffLine(&hb, '+', b[op.bi])
				added++
			}
		}
		if out.Len()+hb.Len() > maxDiffOutput {
			res.Truncated = true
			break
		}
		out.WriteString(hb.String())
		res.Hunks++
		res.Added += added
		res.Removed += removed
	}
	res.Diff = out.String()
	return res, nil
}

func writeDiffLine(w *strings.Builder, prefix byte, line string) {
	w.WriteByte(prefix)
	w.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		w.WriteString("\n\\ No newline at end of file\n")
	}
}

// rangeOf formats a hunk range as GNU diff does: "start,len", with ",1"
// omitted, and an empty range starting at the line before it.
func rangeOf(start, n int) string {
	if n == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	if n == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, n)
}

type hunk struct{ from, to int } // half-open range of ops

// span returns the 0-based start and length of the hunk in a and in b.
func (h hunk) span(ops []editOp) (aStart, aLen, bStart, bLen int) {
	aStart, bStart = -1, -1
	for _, op := range ops[h.from:h.to] {
		if op.kind != opInsert {
			if aStart < 0 {
				aStart = op.ai
			}
			aLen++
		}
		if op.kind != opDelete {
			if bStart < 0 {
				bStart = op.bi
			}
			bLen++
		}
	}
	// An empty side starts where the other side's position says it would.
	if aStart < 0 {
		aStart = posBefore(ops, h.from, true)
	}
	if bStart < 0 {
		bStart = posBefore(ops, h.from, false)
	}
	return
}

// posBefore is the index in a (or b) that the op at i would occupy: one
// past the last line of that side before it.
func posBefore(ops []editOp, i int, inA bool) int {
	for j := i - 1; j >= 0; j-- {
		op := ops[j]
		if inA && op.kind != opInsert {
			return op.ai + 1
		}
		if !inA && op.kind != opDelete {
			return op.bi + 1
		}
	}
	return 0
}

// hunksOf groups changes with up to context equal lines around each, and
// merges changes separated by at most 2*context equal lines.
func hunksOf(ops []editOp, context int) []hunk {
	var out []hunk
	i := 0
	for i < len(ops) {
		for i < len(ops) && ops[i].kind == opEqual {
			i++
		}
		if i == len(ops) {
			break
		}
		from := max(i-context, 0)
		// Extend through changes and short runs of equal lines.
		end := i
		for end < len(ops) {
			if ops[end].kind != opEqual {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == opEqual {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				break
			}
			end = run
		}
		to := min(end+context, len(ops))
		if n := len(out); n > 0 && from < out[n-1].to {
			from = out[n-1].to
		}
		out = append(out, hunk{from, to})
		i = end
	}
	return out
}

// diffLines computes an edit script from a to b. It is minimal (Myers) when
// the edit distance of the part between the common prefix and suffix is at
// most maxEditDistance; beyond that the middle is replaced wholesale and
// minimal is false.
func diffLines(ctx context.Context, a, b []string) (ops []editOp, minimal bool, err error) {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	for i := 0; i < pre; i++ {
		ops = append(ops, editOp{opEqual, i, i})
	}
	// Intern the middle lines so the search compares ints.
	ids := map[string]int32{}
	intern := func(lines []string) []int32 {
		out := make([]int32, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = int32(len(ids))
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	ma, mb := intern(a[pre:len(a)-suf]), intern(b[pre:len(b)-suf])
	mid, ok, err := myers(ctx, ma, mb, maxEditDistance)
	if err != nil {
		return nil, false, err
	}
	minimal = ok
	if !ok {
		mid = mid[:0]
		for i := range ma {
			mid = append(mid, editOp{opDelete, i, 0})
		}
		for j := range mb {
			mid = append(mid, editOp{opInsert, 0, j})
		}
	}
	for _, op := range mid {
		op.ai += pre
		op.bi += pre
		ops = append(ops, op)
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, editOp{opEqual, len(a) - suf + i, len(b) - suf + i})
	}
	return ops, minimal, nil
}

// myers is the greedy O((N+M)D) shortest-edit-script search (Myers 1986),
// with the trace kept for backtracking. It gives up, returning ok=false,
// when the distance exceeds maxD.
func myers(ctx context.Context, a, b []int32, maxD int) ([]editOp, bool, error) {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		var ops []editOp
		for i := 0; i < n; i++ {
			ops = append(ops, editOp{opDelete, i, 0})
		}
		for j := 0; j < m; j++ {
			ops = append(ops, editOp{opInsert, 0, j})
		}
		return ops, true, nil
	}
	total := n + m
	off := total + 1
	v := make([]int32, 2*total+3)
	var trace [][]int32
	for d := 0; d <= total; d++ {
		if d > maxD {
			return nil, false, nil
		}
		if d%64 == 0 && ctx.Err() != nil {
			return nil, false, errBudget
		}
		// trace[d] is v as it stood after step d-1, for k in [-d, d].
		snap := make([]int32, 2*d+1)
		copy(snap, v[off-d:off+d+1])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = int(v[off+k+1]) // down: an insertion
			} else {
				x = int(v[off+k-1]) + 1 // right: a deletion
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = int32(x)
			if x >= n && y >= m {
				return backtrack(trace, n, m, d), true, nil
			}
		}
	}
	return nil, false, nil
}

func backtrack(trace [][]int32, n, m, dEnd int) []editOp {
	var rev []editOp
	x, y := n, m
	for d := dEnd; d > 0; d-- {
		vp := trace[d]
		at := func(k int) int { return int(vp[k+d]) }
		k := x - y
		var pk int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(pk)
		py := px - pk
		// The snake after the move.
		for x > px && y > py {
			x--
			y--
			rev = append(rev, editOp{opEqual, x, y})
		}
		if x == px {
			y--
			rev = append(rev, editOp{opInsert, x, y})
		} else {
			x--
			rev = append(rev, editOp{opDelete, x, y})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, editOp{opEqual, x, y})
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}
