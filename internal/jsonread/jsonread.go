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
		return true
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
