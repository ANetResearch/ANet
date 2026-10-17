//go:build !no_mcp

package agentwire

import "strings"

// notesText is the English text of notes, one per line.
func notesText(ms []Msg) string {
	var b []string
	for _, m := range ms {
		b = append(b, m.String())
	}
	return strings.Join(b, "\n")
}

// argLine is the line number a message names, or 0.
func argLine(m Msg) int {
	n, _ := m.Args["line"].(int)
	return n
}
