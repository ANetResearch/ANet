package main

import (
	"fmt"
	"sort"
	"strconv"
	"unicode/utf8"
)

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// issue is one finding of a checker (json.validate, a2a.card.validate,
// a2a.x402.check). Path is a JSON Pointer (RFC 6901) into the checked
// value; "" is the whole value.
type issue struct {
	Severity string `json:"severity"` // error, warning or info
	Path     string `json:"path"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

const (
	sevError   = "error"
	sevWarning = "warning"
	sevInfo    = "info"
)

// maxIssues bounds a checker's report. A value with thousands of problems
// is described well enough by its first few hundred.
const maxIssues = 200

// issues collects findings up to maxIssues.
type issues struct {
	list      []issue
	truncated bool
}

func (is *issues) add(sev, path, code, format string, a ...any) {
	if len(is.list) >= maxIssues {
		is.truncated = true
		return
	}
	msg := format
	if len(a) > 0 {
		msg = fmt.Sprintf(format, a...)
	}
	is.list = append(is.list, issue{Severity: sev, Path: path, Code: code, Message: msg})
}

func (is *issues) errors() int {
	n := 0
	for _, i := range is.list {
		if i.Severity == sevError {
			n++
		}
	}
	return n
}

// out returns the list, never nil, so the JSON is [] rather than null.
func (is *issues) out() []issue {
	if is.list == nil {
		return []issue{}
	}
	return is.list
}

// ptr appends one reference token to a JSON Pointer, escaping "~" and "/"
// as RFC 6901 requires.
func ptr(base string, tok any) string {
	var s string
	switch t := tok.(type) {
	case string:
		s = escapePtr(t)
	case int:
		s = strconv.Itoa(t)
	default:
		s = fmt.Sprintf("%v", t)
	}
	return base + "/" + s
}

func escapePtr(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '~':
			out = append(out, '~', '0')
		case '/':
			out = append(out, '~', '1')
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
