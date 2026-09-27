//go:build !no_mcp

package agentwire

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Text editing of one top-level YAML mapping (Hermes' mcp_servers and
// a2a_agents): anet's entries live in a marked block among the mapping's
// children, and nothing else in the file is parsed or rewritten.
//
// The top-level key may be missing (anet adds it, tagged so unwire can take
// it away again), empty, or an empty flow mapping `{}` (rewritten to block
// style, the original line kept in the tag). A non-empty flow mapping is
// the one form anet cannot add a child to without parsing, and it says so.

// yamlTag marks a key line anet added or rewrote. "added" means unwire
// removes the line; `was "<json>"` means unwire puts that line back.
const yamlTag = "# anet-wire:"

type yamlMap struct {
	key     string
	markers markers
	begin   string // the full begin marker line anet writes
}

var yamlEmptyValues = map[string]bool{"": true, "{}": true, "null": true, "~": true, "Null": true, "NULL": true}

// topKey finds the key's line: at column 0, bare or quoted, followed by a
// colon. It returns the index (-1 when absent) and the value text after the
// colon with any comment removed.
func (m yamlMap) topKey(lines []string) (int, string, string) {
	re := regexp.MustCompile(`^(?:` + regexp.QuoteMeta(m.key) + `|"` + regexp.QuoteMeta(m.key) + `"|'` +
		regexp.QuoteMeta(m.key) + `')\s*:(\s.*)?$`)
	for i, l := range lines {
		if mm := re.FindStringSubmatch(strings.TrimRight(l, "\r")); mm != nil {
			val, comment := splitYAMLComment(strings.TrimSpace(mm[1]))
			return i, val, comment
		}
	}
	return -1, "", ""
}

// splitYAMLComment separates a trailing "# …" comment from a plain value.
// Values anet looks at are empty, {}, null or ~, none of which contain a
// quote, so a # inside quotes does not need handling beyond refusing it.
func splitYAMLComment(v string) (string, string) {
	if strings.HasPrefix(v, "#") {
		return "", v
	}
	if i := strings.Index(v, " #"); i >= 0 && !strings.ContainsAny(v[:i], `"'`) {
		return strings.TrimSpace(v[:i]), strings.TrimSpace(v[i:])
	}
	return v, ""
}

// regionEnd is the first line after k that starts another top-level item:
// anything at column 0 other than a comment or a blank line.
func regionEnd(lines []string, k int) int {
	for j := k + 1; j < len(lines); j++ {
		l := lines[j]
		if strings.TrimSpace(l) == "" {
			continue
		}
		if l[0] != ' ' && l[0] != '\t' && l[0] != '#' {
			return j
		}
	}
	return len(lines)
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

// childIndent is the indentation of the mapping's first child, or 2.
func childIndent(lines []string, k, end int) int {
	for j := k + 1; j < end; j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return indentOf(lines[j])
	}
	return 2
}

// hasChildKey reports the line of a child named name outside [b, e], or 0.
func hasChildKey(lines []string, k, end, indent, b, e int, name string) int {
	re := regexp.MustCompile(`^(?:` + regexp.QuoteMeta(name) + `|"` + regexp.QuoteMeta(name) + `"|'` +
		regexp.QuoteMeta(name) + `')\s*:(\s|$)`)
	for j := k + 1; j < end; j++ {
		if inBlock(j, b, e) || indentOf(lines[j]) != indent {
			continue
		}
		if re.MatchString(strings.TrimSpace(lines[j])) {
			return j + 1
		}
	}
	return 0
}

// indentLines prefixes each non-empty line with n spaces.
func indentLines(block []string, n int) []string {
	pad := strings.Repeat(" ", n)
	out := make([]string, len(block))
	for i, l := range block {
		if l != "" {
			out[i] = pad + l
		}
	}
	return out
}

// set puts body (the children, unindented, without markers) into anet's
// block under the key. children are the child keys body defines; one that
// already exists outside the block is a conflict.
func (m yamlMap) set(src, path string, body []string, children []string) (string, error) {
	lines := splitLines(src)
	b, e, err := m.markers.locate(lines, path)
	if err != nil {
		return "", err
	}
	k, val, _ := m.topKey(lines)
	if b >= 0 {
		// Replace in place, at the indentation the block already has.
		ind := indentOf(lines[b])
		if k >= 0 {
			end := regionEnd(lines, k)
			for _, c := range children {
				if line := hasChildKey(lines, k, end, ind, b, e, c); line > 0 {
					return "", &ConflictError{Path: path, Line: line, What: fmt.Sprintf("%s 下已有不是 anet 写的 %s 条目", m.key, c)}
				}
			}
		}
		block := indentLines(append(append([]string{m.begin}, body...), m.markers.end), ind)
		return joinLines(replaceBlock(lines, b, e, block)), nil
	}
	if k < 0 {
		block := append([]string{m.key + ":  " + yamlTag + " added"},
			indentLines(append(append([]string{m.begin}, body...), m.markers.end), 2)...)
		return joinLines(appendBlock(lines, block)), nil
	}
	end := regionEnd(lines, k)
	ind := childIndent(lines, k, end)
	if !yamlEmptyValues[val] {
		return "", &ConflictError{Path: path, Line: k + 1, What: fmt.Sprintf(
			"%s 用的是行内写法(%s),anet 无法在不解析 YAML 的情况下加入条目;请改成块写法后再运行 wire", m.key, val)}
	}
	if ind == 0 {
		return "", &ConflictError{Path: path, Line: k + 1, What: m.key + " 不是映射(子项没有缩进)"}
	}
	for _, c := range children {
		if line := hasChildKey(lines, k, end, ind, -1, -1, c); line > 0 {
			return "", &ConflictError{Path: path, Line: line, What: fmt.Sprintf("%s 下已有不是 anet 写的 %s 条目", m.key, c)}
		}
	}
	keyLine := lines[k]
	if val != "" {
		// `key: {}` becomes `key:` with children; the tag keeps the
		// original line so unwire can restore it.
		orig, _ := json.Marshal(strings.TrimRight(keyLine, "\r"))
		keyLine = m.key + ":  " + yamlTag + " was " + string(orig)
	}
	block := indentLines(append(append([]string{m.begin}, body...), m.markers.end), ind)
	out := append([]string{}, lines[:k]...)
	out = append(out, keyLine)
	out = append(out, block...)
	out = append(out, lines[k+1:]...)
	return joinLines(out), nil
}

// drop removes anet's block and, when the key is left with no children
// and anet added or rewrote it, the key line too.
func (m yamlMap) drop(src, path string) (string, bool, error) {
	lines := splitLines(src)
	b, e, err := m.markers.locate(lines, path)
	if err != nil || b < 0 {
		return src, false, err
	}
	lines = append(append([]string{}, lines[:b]...), lines[e+1:]...)
	if k, _, comment := m.topKey(lines); k >= 0 && strings.HasPrefix(comment, yamlTag) {
		end := regionEnd(lines, k)
		empty := true
		for j := k + 1; j < end; j++ {
			if t := strings.TrimSpace(lines[j]); t != "" && !strings.HasPrefix(t, "#") {
				empty = false
				break
			}
		}
		tag := strings.TrimSpace(strings.TrimPrefix(comment, yamlTag))
		switch {
		case empty && tag == "added":
			lines = removeBlock(lines, k, k)
		case empty && strings.HasPrefix(tag, "was "):
			var orig string
			if json.Unmarshal([]byte(strings.TrimPrefix(tag, "was ")), &orig) == nil {
				lines[k] = orig
			}
		case !empty:
			// Other entries now live under the key; keep it, drop the tag.
			lines[k] = m.key + ":"
		}
	}
	return joinLines(lines), true, nil
}

// blockBody returns the lines inside anet's block, dedented, or nil.
func (m yamlMap) blockBody(src, path string) ([]string, error) {
	lines := splitLines(src)
	b, e, err := m.markers.locate(lines, path)
	if err != nil || b < 0 {
		return nil, err
	}
	ind := indentOf(lines[b])
	var out []string
	for _, l := range lines[b+1 : e] {
		if len(l) >= ind && strings.TrimSpace(l[:ind]) == "" {
			l = l[ind:]
		}
		out = append(out, l)
	}
	return out, nil
}

// yamlString quotes s as a YAML double-quoted scalar. JSON's string escapes
// are a subset of YAML's, so a JSON string literal is one.
func yamlString(s string) string {
	b, _ := marshalNoEscape(s)
	return string(b)
}

// yamlUnquote reads back a scalar yamlString wrote.
func yamlUnquote(s string) string {
	s = strings.TrimSpace(s)
	var out string
	if json.Unmarshal([]byte(s), &out) == nil {
		return out
	}
	return strings.Trim(s, `"'`)
}
