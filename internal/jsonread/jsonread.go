// Package jsonread says whether JSON a peer or an operator wrote means the
// same to this program as to everyone else who reads it.
//
// encoding/json matches object member names to struct fields ignoring
// case, and takes the last of several matches. Every other reader anet's
// JSON meets — a2a-go's clients in other languages, JavaScript's
// JSON.parse, Python's json, this module's own map-based readers — takes
// member names as written. A document built to be read two ways ("amount"
// and "AMOUNT", "command" and "Command") is shown to the user with one
// value and acted on by anet with another (docs/notes/0033).
package jsonread

import (
	"bytes"
	"encoding/json"
	"unicode"
	"unicode/utf8"
)

// ReadsAlike reports whether raw, which json.Unmarshal decoded into v, says
// what v says to a reader that takes member names as written:
// {"amount":"1","AMOUNT":"900"} decodes to 900 and reads as 1 everywhere
// else, and {"Amount":"900"} decodes to 900 where everyone else reads no
// amount. Each member v encodes must be in raw under its own name with the
// same value, or absent at its zero value; raw may carry other members,
// which neither reader takes.
func ReadsAlike(raw []byte, v any) bool {
	enc, err := json.Marshal(v)
	if err != nil {
		return false
	}
	read := func(b []byte) (any, bool) {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var x any
		return x, dec.Decode(&x) == nil
	}
	want, ok1 := read(enc)
	got, ok2 := read(raw)
	return ok1 && ok2 && sameAsRead(want, got)
}

// hiddenByOmitempty reports a member of got whose value Go's decoder may
// have replaced with a zero that want, the re-encoding, then left out.
//
// encoding/json fills a struct field from every member whose name matches
// the field's ignoring case, the last one winning, and an omitempty field
// that ends at its zero value is not encoded. In {"type":"sse","TYPE":""}
// Go holds the "" of the second, want has no "type" at all, and the member
// by member comparison has nothing to compare, while everyone else reads
// "sse" (the review of docs/notes/0033). The sign is two or more members
// of got whose names fold alike, as encoding/json folds them, none of them
// matching a member of want, and one of them not zero. A single such
// member is one Go does not read at all; members matching one of want are
// compared already. Two members that differ only in case and that Go
// does not read either are refused as well: from the bytes alone they
// cannot be told apart from a field that was hidden.
func hiddenByOmitempty(want, got map[string]any) bool {
	inWant := make(map[string]bool, len(want))
	for k := range want {
		inWant[foldName(k)] = true
	}
	type group struct {
		n       int
		nonZero bool
	}
	groups := map[string]*group{}
	for k, v := range got {
		f := foldName(k)
		if inWant[f] {
			continue
		}
		g := groups[f]
		if g == nil {
			g = &group{}
			groups[f] = g
		}
		g.n++
		g.nonZero = g.nonZero || !isZeroJSON(v)
	}
	for _, g := range groups {
		if g.n > 1 && g.nonZero {
			return true
		}
	}
	return false
}

// foldName folds a member name as encoding/json does when it matches one
// to a struct field (encoding/json/fold.go): ASCII letters to upper case,
// any other rune to the smallest rune of its case-folding orbit, so that
// "kind" and "\u212aind" (KELVIN SIGN) fold alike, as Go matches them.
func foldName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out = append(out, c)
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		for {
			r2 := unicode.SimpleFold(r)
			if r2 <= r {
				r = r2
				break
			}
			r = r2
		}
		out = utf8.AppendRune(out, r)
		i += n
	}
	return string(out)
}

// isZeroJSON reports a JSON zero value: null, false, 0, "", [] or {}.
func isZeroJSON(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// sameAsRead compares want, a value as this node encodes it, with got, the
// same value as written: equal, apart from members got has that want
// does not, and members want has at their zero value that got leaves out.
func sameAsRead(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok && isZeroJSON(wv) {
				continue // absent is the zero value to both readers
			}
			if !ok || !sameAsRead(wv, gv) {
				return false
			}
		}
		return !hiddenByOmitempty(w, g)
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !sameAsRead(w[i], g[i]) {
				return false
			}
		}
		return true
	case json.Number:
		g, ok := got.(json.Number)
		if !ok {
			return false
		}
		if w == g {
			return true
		}
		a, err1 := w.Float64()
		b, err2 := g.Float64()
		return err1 == nil && err2 == nil && a == b
	default:
		return want == got
	}
}
