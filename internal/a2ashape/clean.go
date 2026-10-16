package a2ashape

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxShortRunes bounds peer-written text (an amount, an asset, a scheme)
// placed in a synthesized sentence.
const maxShortRunes = 64

// clean makes a peer-written string safe to put in a sentence: control
// characters (newlines included) become spaces, invisible format
// characters (bidirectional overrides and isolates, zero-width joiners) are
// dropped so that a payee or an amount reads as it is, and it is cut to max
// runes.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError || unicode.IsControl(r):
			return ' '
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

// Clean is clean for the projection of stored rows (package stored), which
// puts the same kind of text in its sentences.
func Clean(s string, max int) string { return clean(s, max) }
