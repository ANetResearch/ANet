package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type jsonValidateArgs struct {
	// JSON is the document as text: its syntax is checked, with the
	// position of the first error and any duplicate member names.
	JSON *string `json:"json"`
	// Instance is the document as a value (already parsed by whoever sent
	// it, so only the schema can find anything wrong with it).
	Instance json.RawMessage `json:"instance"`
	Schema   json.RawMessage `json:"schema"`
}

type syntaxReport struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

type jsonValidateResult struct {
	// Verdict is valid, invalid or unknown. Unknown means nothing
	// evaluated failed but something that could decide was not evaluated
	// (an unsupported keyword, a remote $ref); it is not a pass.
	Verdict string `json:"verdict"`
	// Valid is present only when the verdict is definite.
	Valid         *bool         `json:"valid,omitempty"`
	Syntax        *syntaxReport `json:"syntax,omitempty"`
	DuplicateKeys []string      `json:"duplicate_keys,omitempty"`
	SchemaChecked bool          `json:"schema_checked"`
	Errors        []schemaError `json:"errors"`
	SchemaIssues  []issue       `json:"schema_issues"`
	Unsupported   []string      `json:"unsupported,omitempty"`
	Truncated     bool          `json:"truncated,omitempty"`
}

func handleJSONValidate(ctx context.Context, _ *env, body []byte) (any, error) {
	var a jsonValidateArgs
	if err := decodeArgs(body, &a); err != nil {
		return nil, err
	}
	hasInstance := len(a.Instance) > 0
	if (a.JSON == nil) == !hasInstance {
		// Both or neither. An explicit "instance": null is an instance.
		if a.JSON != nil {
			return nil, badArgs("give \"json\" (text) or \"instance\" (value), not both")
		}
		return nil, badArgs("\"json\" (text) or \"instance\" (value) is required")
	}
	res := &jsonValidateResult{Errors: []schemaError{}, SchemaIssues: []issue{}}

	var inst any
	if a.JSON != nil {
		v, dupes, serr := parseStrictJSON(*a.JSON)
		res.Syntax = &syntaxReport{OK: serr == nil}
		if serr != nil {
			res.Syntax.Message, res.Syntax.Offset = serr.msg, serr.offset
			res.Syntax.Line, res.Syntax.Column = lineCol(*a.JSON, serr.offset)
			f := false
			res.Verdict, res.Valid = vFail.String(), &f
			return res, nil
		}
		res.DuplicateKeys = dupes
		inst = v
	} else {
		v, _, serr := parseStrictJSON(string(a.Instance))
		if serr != nil {
			return nil, badArgs("\"instance\": %s", serr.msg)
		}
		inst = v
	}

	verdict := vPass
	if len(a.Schema) > 0 {
		schema, _, serr := parseStrictJSON(string(a.Schema))
		if serr != nil {
			return nil, badArgs("\"schema\": %s", serr.msg)
		}
		switch schema.(type) {
		case bool, map[string]any:
		default:
			return nil, badArgs("\"schema\" must be an object or a boolean")
		}
		sv := newSchemaValidator(ctx, schema)
		var err error
		verdict, err = sv.run(inst)
		if err != nil {
			return nil, err
		}
		res.SchemaChecked = true
		res.Errors = sv.errs
		if res.Errors == nil {
			res.Errors = []schemaError{}
		}
		res.SchemaIssues = sv.schemaIssues.out()
		res.Truncated = sv.errsTruncated || sv.schemaIssues.truncated
		res.Unsupported = sortedKeys(sv.unsupported)
	}
	res.Verdict = verdict.String()
	if verdict != vUnknown {
		ok := verdict == vPass
		res.Valid = &ok
	}
	return res, nil
}

type jsonSyntaxError struct {
	msg    string
	offset int64
}

// maxJSONDepth bounds nesting in documents and schemas.
const maxJSONDepth = 512

// parseStrictJSON parses one JSON document, keeping numbers exact
// (json.Number), and reports duplicate member names by JSON Pointer. The
// value of a duplicate is the last one, as encoding/json and most
// parsers keep it.
func parseStrictJSON(text string) (any, []string, *jsonSyntaxError) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var dupes []string
	var parse func(path string, depth int) (any, error)
	parse = func(path string, depth int) (any, error) {
		if depth > maxJSONDepth {
			return nil, fmt.Errorf("nesting deeper than %d", maxJSONDepth)
		}
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				obj := map[string]any{}
				for dec.More() {
					kt, err := dec.Token()
					if err != nil {
						return nil, err
					}
					key, ok := kt.(string)
					if !ok {
						return nil, errors.New("object member name is not a string")
					}
					p := ptr(path, key)
					if _, dup := obj[key]; dup && len(dupes) < maxIssues {
						dupes = append(dupes, p)
					}
					v, err := parse(p, depth+1)
					if err != nil {
						return nil, err
					}
					obj[key] = v
				}
				if _, err := dec.Token(); err != nil { // '}'
					return nil, err
				}
				return obj, nil
			case '[':
				arr := []any{}
				for dec.More() {
					v, err := parse(ptr(path, len(arr)), depth+1)
					if err != nil {
						return nil, err
					}
					arr = append(arr, v)
				}
				if _, err := dec.Token(); err != nil { // ']'
					return nil, err
				}
				return arr, nil
			}
			return nil, fmt.Errorf("unexpected %q", t)
		default:
			return t, nil
		}
	}
	v, err := parse("", 0)
	if err == nil {
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("data after the end of the JSON value")
			if terr != nil && !errors.Is(terr, io.EOF) {
				err = terr
			}
		}
	}
	if err != nil {
		off := dec.InputOffset()
		var se *json.SyntaxError
		if errors.As(err, &se) {
			off = se.Offset
		}
		msg := err.Error()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			msg = "unexpected end of JSON input"
			off = int64(len(text))
		}
		return nil, nil, &jsonSyntaxError{msg: msg, offset: off}
	}
	return v, dupes, nil
}

// lineCol converts a byte offset into a 1-based line and column (the
// column counts characters).
func lineCol(text string, off int64) (line, col int) {
	if off > int64(len(text)) {
		off = int64(len(text))
	}
	before := text[:off]
	line = bytes.Count([]byte(before), []byte("\n")) + 1
	start := strings.LastIndexByte(before, '\n') + 1
	col = len([]rune(before[start:])) + 1
	return line, col
}
