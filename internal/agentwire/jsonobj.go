//go:build !no_mcp

package agentwire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// jsonObject is a JSON object that keeps its members in file order.
//
// Decoding a tool's settings into a Go map and encoding it back sorts every
// key: ~/.claude.json is some seventy keys long, and the operator's diff of
// "anet added one MCP server" would show the whole file reordered. Members
// anet does not touch are carried as raw bytes and come back unchanged
// apart from indentation.
type jsonObject struct {
	members []jsonMember
}

type jsonMember struct {
	key string
	val json.RawMessage
}

// parseObject reads a JSON object. Empty input is an empty object, so a
// file that exists but was truncated to nothing is treated like a new one.
func parseObject(data []byte) (*jsonObject, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &jsonObject{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	obj := &jsonObject{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("object key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		obj.members = append(obj.members, jsonMember{key, raw})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected content after the JSON object")
	}
	return obj, nil
}

func (o *jsonObject) get(key string) (json.RawMessage, bool) {
	for _, m := range o.members {
		if m.key == key {
			return m.val, true
		}
	}
	return nil, false
}

// set replaces the member in place, or appends it.
func (o *jsonObject) set(key string, val json.RawMessage) {
	for i := range o.members {
		if o.members[i].key == key {
			o.members[i].val = val
			return
		}
	}
	o.members = append(o.members, jsonMember{key, val})
}

func (o *jsonObject) del(key string) bool {
	for i := range o.members {
		if o.members[i].key == key {
			o.members = append(o.members[:i], o.members[i+1:]...)
			return true
		}
	}
	return false
}

func (o *jsonObject) empty() bool { return len(o.members) == 0 }

// raw encodes the object compactly.
func (o *jsonObject) raw() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o.members {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := marshalNoEscape(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// render indents the object like the file it came from.
func (o *jsonObject) render(like []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, o.raw(), "", detectIndent(like)); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// detectIndent returns the indentation unit of a JSON file: the leading
// whitespace of its first indented line. Two spaces when there is none,
// which is what Claude Code, Cursor and opencode write.
func detectIndent(src []byte) string {
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		return line[:len(line)-len(trimmed)]
	}
	return "  "
}

// subObject returns the object stored under key, creating an empty one
// when absent. A member that exists but is not an object is an error: it is
// the operator's, and anet does not know what it means.
func (o *jsonObject) subObject(key string) (*jsonObject, bool, error) {
	raw, ok := o.get(key)
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return &jsonObject{}, false, nil
	}
	sub, err := parseObject(raw)
	if err != nil {
		return nil, true, fmt.Errorf("%q is not a JSON object", key)
	}
	return sub, true, nil
}

// marshalNoEscape encodes v without HTML escaping: a path containing & or <
// stays readable in the file, and the result is still valid JSON.
func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// decodeLoose decodes raw into v, reporting only whether it worked; an
// entry that does not decode is simply not anet's.
func decodeLoose(raw json.RawMessage, v any) bool {
	return json.Unmarshal(raw, v) == nil
}

// sameStrings compares two string slices.
func sameStrings(a, b []string) bool {
	return reflect.DeepEqual(append([]string{}, a...), append([]string{}, b...))
}

// sameEnv compares two environments, treating nil and empty as equal.
func sameEnv(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
