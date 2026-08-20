// Package inv2 guards INV-2: nothing this node publishes publicly may
// carry org-scoped data.
//
// It is the publish-side dual of INV-1. INV-1 keeps org data off the p2p
// fabric; INV-2 keeps it out of the things a node says about itself to
// everyone — its hub registration, its public profile. A node that belongs
// to an org must not disclose that it does, or which one, by leaking an
// org id into a field nobody was reading carefully.
//
// The daemon does not know what an org is, and must not learn. A module
// that holds something confidential says so by implementing
// module.Confidential, and the daemon screens its public publications
// against whatever tokens come back. That keeps the knowledge where it
// belongs: the org module knows its org id is a secret, the daemon knows
// only that some string must never leave.
//
// Best-effort by construction, and the limit is worth stating. Screen
// matches raw byte substrings, so it catches an id in the textual CID form
// that dominates real leaks — a CBOR or JSON text field keeps the token
// contiguous — and it does not catch a re-encoded form such as the raw
// multihash digest. The structural guard is the stronger one: a
// publishable type has no field for org data, and Fields proves it.
package inv2

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// ErrLeak is returned when a publication carries a forbidden token.
var ErrLeak = errors.New("inv2: org-scoped data in a public publication")

// Screen rejects serialized bytes containing any non-empty forbidden token.
func Screen(serialized []byte, forbidden []string) error {
	for _, tok := range forbidden {
		if tok == "" {
			continue
		}
		if bytes.Contains(serialized, []byte(tok)) {
			return fmt.Errorf("%w: a confidential token appears in what this node is about to publish", ErrLeak)
		}
	}
	return nil
}

// orgMarkers are field-name fragments that would mean org data has a
// declared home on a public type.
var orgMarkers = []string{"orgid", "credential", "membership"}

// Fields returns the dotted path of every field on a publishable type whose
// name suggests org-scoped data, at any nesting depth.
//
// The structural half of the guard, and the half that actually holds.
// Screening bytes catches a value that leaked into an opaque field;
// this catches someone adding the field in the first place, which is how
// it would really happen — a schema grows an OrgID because somewhere it
// was convenient, and every node starts publishing one.
func Fields(v any) []string {
	return walk(reflect.TypeOf(v), "", map[reflect.Type]bool{})
}

func walk(t reflect.Type, path string, seen map[reflect.Type]bool) []string {
	if t == nil || seen[t] {
		return nil
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return walk(t.Elem(), path, seen)
	case reflect.Map:
		return append(walk(t.Key(), path, seen), walk(t.Elem(), path, seen)...)
	case reflect.Struct:
		seen[t] = true
		defer delete(seen, t)
		var out []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			p := f.Name
			if path != "" {
				p = path + "." + f.Name
			}
			if isOrgField(f.Name) {
				out = append(out, p)
			}
			out = append(out, walk(f.Type, p, seen)...)
		}
		return out
	}
	return nil
}

func isOrgField(name string) bool {
	n := strings.ToLower(strings.ReplaceAll(name, "_", ""))
	for _, m := range orgMarkers {
		if strings.Contains(n, m) {
			return true
		}
	}
	return false
}
