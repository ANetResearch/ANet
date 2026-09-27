//go:build !no_mcp

package agentwire

import (
	"fmt"
	"strings"
)

// A managed block is a run of lines between two marker lines that anet owns
// outright: wire replaces everything between the markers, unwire removes
// the markers and everything between them, and nothing outside them is
// touched. It is how anet edits formats it does not parse (TOML, YAML) and
// files that are mostly the operator's (AGENTS.md, SOUL.md, CLAUDE.md).
type markers struct {
	// begin matches by prefix, so the explanatory text after it can change
	// between versions without orphaning blocks an older anet wrote.
	begin string
	end   string
}

// splitLines splits text into lines without their terminators. Empty text
// has no lines.
func splitLines(src string) []string {
	if src == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(src, "\n"), "\n")
}

// joinLines is the inverse of splitLines; non-empty output always ends in a
// newline.
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// locate finds the block. b is -1 when there is none; e is the index of the
// end marker. A begin with no end, or a second block, is an error: guessing
// where anet's content stops would mean deleting the operator's.
func (m markers) locate(lines []string, path string) (b, e int, err error) {
	b, e = -1, -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, m.begin):
			if b >= 0 {
				return -1, -1, fmt.Errorf("%s:%d: 这里开始了第二个 anet 托管块;请手动删掉其中一个", path, i+1)
			}
			b = i
		case t == m.end:
			if b < 0 || e >= 0 {
				return -1, -1, fmt.Errorf("%s:%d: anet 托管块的结束标记没有对应的开始标记;请手动修正", path, i+1)
			}
			e = i
		}
	}
	if b >= 0 && e < 0 {
		return -1, -1, fmt.Errorf("%s:%d: anet 托管块没有结束标记;请手动修正", path, b+1)
	}
	return b, e, nil
}

// inBlock reports whether line i is inside [b, e].
func inBlock(i, b, e int) bool { return b >= 0 && i >= b && i <= e }

// replaceBlock swaps lines[b..e] for block.
func replaceBlock(lines []string, b, e int, block []string) []string {
	out := append([]string{}, lines[:b]...)
	out = append(out, block...)
	return append(out, lines[e+1:]...)
}

// appendBlock adds block at the end, separated from existing text by one
// blank line.
func appendBlock(lines []string, block []string) []string {
	out := append([]string{}, lines...)
	if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
		out = append(out, "")
	}
	return append(out, block...)
}

// removeBlock deletes lines[b..e] and the blank separator line appendBlock
// put before it, so wire followed by unwire gives back the original text.
func removeBlock(lines []string, b, e int) []string {
	out := append([]string{}, lines[:b]...)
	rest := lines[e+1:]
	if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" &&
		(len(rest) == 0 || strings.TrimSpace(rest[0]) == "") {
		out = out[:len(out)-1]
	}
	return append(out, rest...)
}

// onlyBlank reports whether nothing but whitespace is left, in which case
// the file anet created is removed rather than left empty.
func onlyBlank(lines []string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

// Markdown blocks: the persona text in AGENTS.md, SOUL.md and the legacy
// ~/.claude/CLAUDE.md block. The begin prefix also matches the marker the
// old `anet install` wrote, so wire upgrades those blocks in place.
var mdMarkers = markers{begin: "<!-- anet:begin", end: "<!-- anet:end -->"}

const mdBegin = "<!-- anet:begin (managed by `anet agents wire`; edits inside this block are overwritten) -->"

// setTextBlock returns src with the markdown block set to body (appended
// when absent) and whether anything changed.
func setTextBlock(src, path, body string, m markers, begin string) (string, bool, error) {
	lines := splitLines(src)
	b, e, err := m.locate(lines, path)
	if err != nil {
		return "", false, err
	}
	block := append([]string{begin}, splitLines(body)...)
	block = append(block, m.end)
	var out []string
	if b >= 0 {
		out = replaceBlock(lines, b, e, block)
	} else {
		out = appendBlock(lines, block)
	}
	res := joinLines(out)
	return res, res != src, nil
}

// dropTextBlock returns src without the block and whether it had one.
func dropTextBlock(src, path string, m markers) (string, bool, error) {
	lines := splitLines(src)
	b, e, err := m.locate(lines, path)
	if err != nil || b < 0 {
		return src, false, err
	}
	return joinLines(removeBlock(lines, b, e)), true, nil
}

// planTextBlock plans writing the persona block into path.
func planTextBlock(o *Options, path, body, what string) (*change, error) {
	cur, err := readMaybe(realPath(path))
	if err != nil {
		return nil, err
	}
	out, changed, err := setTextBlock(string(cur), path, body, mdMarkers, mdBegin)
	if err != nil || !changed {
		return nil, err
	}
	return &change{path: path, before: cur, after: []byte(out), mode: 0o644,
		note: "写入 " + what + ":" + short(o, path)}, nil
}

// planDropTextBlock plans removing the persona block from path, deleting
// the file when nothing else is left in it.
func planDropTextBlock(o *Options, path, what string) (*change, error) {
	cur, err := readMaybe(realPath(path))
	if err != nil || cur == nil {
		return nil, err
	}
	out, had, err := dropTextBlock(string(cur), path, mdMarkers)
	if err != nil || !had {
		return nil, err
	}
	c := &change{path: path, before: cur, after: []byte(out), note: "删除 " + what + ":" + short(o, path)}
	if onlyBlank(splitLines(out)) {
		c.del = true
	}
	return c, nil
}
