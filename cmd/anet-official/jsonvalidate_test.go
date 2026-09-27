package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validateJSON(t *testing.T, args string) *jsonValidateResult {
	t.Helper()
	res, err := handleJSONValidate(context.Background(), nil, []byte(args))
	if err != nil {
		t.Fatalf("%s: %v", args, err)
	}
	return res.(*jsonValidateResult)
}

func TestJSONSyntax(t *testing.T) {
	cases := []struct {
		text      string
		ok        bool
		line, col int
		dupes     string
	}{
		{`{"a": 1}`, true, 0, 0, ""},
		{`[1, 2, 3]`, true, 0, 0, ""},
		{`"just a string"`, true, 0, 0, ""},
		{"{\n  \"a\": 1,\n  \"b\": ,\n}", false, 3, 8, ""},
		{`{"a": 1`, false, 1, 8, ""},
		{`{"a": 1} {"b": 2}`, false, 0, 0, ""},
		{`{"a": 1, "a": 2, "b": {"c": 1, "c": 2}}`, true, 0, 0, "/a,/b/c"},
		{`{"a/b": 1, "a/b": 2}`, true, 0, 0, "/a~1b"},
		{`nul`, false, 0, 0, ""},
		{``, false, 1, 1, ""},
	}
	for _, c := range cases {
		b, _ := json.Marshal(map[string]string{"json": c.text})
		res := validateJSON(t, string(b))
		if res.Syntax == nil || res.Syntax.OK != c.ok {
			t.Errorf("%q: syntax %+v, want ok=%v", c.text, res.Syntax, c.ok)
			continue
		}
		if !c.ok {
			if res.Verdict != "invalid" || res.Valid == nil || *res.Valid {
				t.Errorf("%q: verdict %s", c.text, res.Verdict)
			}
			if c.line != 0 && (res.Syntax.Line != c.line || res.Syntax.Column != c.col) {
				t.Errorf("%q: error at %d:%d, want %d:%d (%s)", c.text, res.Syntax.Line, res.Syntax.Column, c.line, c.col, res.Syntax.Message)
			}
			continue
		}
		if res.Verdict != "valid" {
			t.Errorf("%q: verdict %s", c.text, res.Verdict)
		}
		if got := strings.Join(res.DuplicateKeys, ","); got != c.dupes {
			t.Errorf("%q: duplicates %q, want %q", c.text, got, c.dupes)
		}
	}
}

// Each case is a schema, an instance, the verdict, and the keywords that
// must be among the reported failures.
func TestJSONSchema(t *testing.T) {
	cases := []struct {
		name     string
		schema   string
		instance string
		verdict  string
		keywords string // comma-separated, in report order
	}{
		{"true schema", `true`, `{"x":1}`, "valid", ""},
		{"false schema", `false`, `1`, "invalid", "false"},
		{"type", `{"type":"string"}`, `1`, "invalid", "type"},
		{"integer accepts 1.0", `{"type":"integer"}`, `1.0`, "valid", ""},
		{"integer refuses 1.5", `{"type":"integer"}`, `1.5`, "invalid", "type"},
		{"type list", `{"type":["null","string"]}`, `null`, "valid", ""},
		{"required", `{"type":"object","required":["id","name"]}`, `{"name":"x"}`, "invalid", "required"},
		{"enum by value", `{"enum":[1,"a",{"k":[1,2]}]}`, `{"k":[1.0,2]}`, "valid", ""},
		{"enum miss", `{"enum":["a","b"]}`, `"c"`, "invalid", "enum"},
		{"const", `{"const":{"a":1}}`, `{"a":2}`, "invalid", "const"},
		{"multipleOf exact decimal", `{"multipleOf":0.1}`, `0.3`, "valid", ""},
		{"multipleOf miss", `{"multipleOf":3}`, `10`, "invalid", "multipleOf"},
		{"bounds", `{"minimum":1,"exclusiveMaximum":10}`, `10`, "invalid", "exclusiveMaximum"},
		{"big integers", `{"maximum":9007199254740993}`, `9007199254740994`, "invalid", "maximum"},
		{"string length counts characters", `{"maxLength":2}`, `"你好"`, "valid", ""},
		{"minLength", `{"minLength":3}`, `"ab"`, "invalid", "minLength"},
		{"pattern", `{"pattern":"^[a-z]+$"}`, `"abc1"`, "invalid", "pattern"},
		{"items", `{"items":{"type":"integer"}}`, `[1,"x",3]`, "invalid", "type"},
		{"prefixItems and items false", `{"prefixItems":[{"type":"string"}],"items":false}`, `["a",1]`, "invalid", "false"},
		{"draft-07 items array", `{"items":[{"type":"string"}],"additionalItems":{"type":"integer"}}`, `["a",1,"b"]`, "invalid", "type"},
		{"uniqueItems by value", `{"uniqueItems":true}`, `[1,1.0]`, "invalid", "uniqueItems"},
		{"contains", `{"contains":{"type":"string"},"minContains":2}`, `["a",1]`, "invalid", "contains"},
		{"maxContains", `{"contains":{"type":"string"},"maxContains":1}`, `["a","b"]`, "invalid", "maxContains"},
		{"additionalProperties", `{"properties":{"a":{}},"patternProperties":{"^x-":{}},"additionalProperties":false}`, `{"a":1,"x-y":2,"b":3}`, "invalid", "additionalProperties"},
		{"propertyNames", `{"propertyNames":{"maxLength":3}}`, `{"abcd":1}`, "invalid", "maxLength"},
		{"dependentRequired", `{"dependentRequired":{"card":["cvv"]}}`, `{"card":"x"}`, "invalid", "dependentRequired"},
		{"draft-07 dependencies", `{"dependencies":{"a":{"required":["b"]}}}`, `{"a":1}`, "invalid", "required"},
		{"anyOf", `{"anyOf":[{"type":"string"},{"type":"integer"}]}`, `true`, "invalid", "anyOf"},
		{"oneOf two match", `{"oneOf":[{"type":"integer"},{"minimum":0}]}`, `5`, "invalid", "oneOf"},
		{"oneOf one match", `{"oneOf":[{"type":"integer"},{"type":"string"}]}`, `5`, "valid", ""},
		{"not", `{"not":{"type":"null"}}`, `null`, "invalid", "not"},
		{"if then", `{"if":{"properties":{"k":{"const":"a"}}},"then":{"required":["x"]},"else":{"required":["y"]}}`, `{"k":"b","y":1}`, "valid", ""},
		{"if else", `{"if":{"properties":{"k":{"const":"a"}}},"then":{"required":["x"]},"else":{"required":["y"]}}`, `{"k":"b"}`, "invalid", "required"},
		{"$ref to $defs", `{"$defs":{"pos":{"type":"integer","minimum":1}},"properties":{"n":{"$ref":"#/$defs/pos"}}}`, `{"n":0}`, "invalid", "minimum"},
		{"$ref to anchor", `{"$defs":{"s":{"$anchor":"str","type":"string"}},"items":{"$ref":"#str"}}`, `["a",1]`, "invalid", "type"},
		{"recursive $ref", `{"type":"object","properties":{"child":{"$ref":"#"}},"required":["v"]}`, `{"v":1,"child":{"child":{}}}`, "invalid", "required"},
		{"ref siblings apply", `{"$defs":{"i":{"type":"integer"}},"$ref":"#/$defs/i","minimum":5}`, `3`, "invalid", "minimum"},
		// Not evaluated is not passed.
		{"remote $ref is unknown", `{"$ref":"https://example.com/schema.json"}`, `1`, "unknown", ""},
		{"unsupported keyword is unknown", `{"unevaluatedProperties":false}`, `{"a":1}`, "unknown", ""},
		{"not of unknown stays unknown", `{"not":{"unevaluatedProperties":false}}`, `{"a":1}`, "unknown", ""},
		{"a definite failure decides over unknown", `{"type":"string","unevaluatedProperties":false}`, `1`, "invalid", "type"},
		{"anyOf with a pass beats unknown", `{"anyOf":[{"$dynamicRef":"#x"},{"type":"integer"}]}`, `1`, "valid", ""},
		{"RE2-incompatible pattern", `{"pattern":"^(?!x)"}`, `"a"`, "unknown", ""},
		{"ref cycle without progress", `{"$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"$ref":"#/$defs/a"}`, `1`, "unknown", ""},
		{"format is not asserted", `{"format":"email"}`, `"not an email"`, "valid", ""},
	}
	for _, c := range cases {
		args := `{"schema":` + c.schema + `,"instance":` + c.instance + `}`
		res := validateJSON(t, args)
		if res.Verdict != c.verdict {
			t.Errorf("%s: verdict %s, want %s; errors %+v; issues %+v", c.name, res.Verdict, c.verdict, res.Errors, res.SchemaIssues)
			continue
		}
		if (res.Valid == nil) != (c.verdict == "unknown") {
			t.Errorf("%s: valid must be present exactly when the verdict is definite", c.name)
		}
		var kws []string
		for _, e := range res.Errors {
			kws = append(kws, e.Keyword)
		}
		if c.keywords != "" && !strings.Contains(strings.Join(kws, ","), c.keywords) {
			t.Errorf("%s: failures %v, want %s", c.name, kws, c.keywords)
		}
		if c.verdict == "valid" && len(res.Errors) != 0 {
			t.Errorf("%s: valid with errors %+v", c.name, res.Errors)
		}
	}
}

// Errors point into the instance and the schema with JSON Pointers.
func TestJSONSchemaPointers(t *testing.T) {
	res := validateJSON(t, `{"schema":{"properties":{"list":{"items":{"properties":{"a/b":{"type":"string"}}}}}},"instance":{"list":[{"a/b":"x"},{"a/b":2}]}}`)
	if len(res.Errors) != 1 {
		t.Fatalf("errors: %+v", res.Errors)
	}
	e := res.Errors[0]
	if e.Instance != "/list/1/a~1b" || e.Schema != "/properties/list/items/properties/a~1b/type" {
		t.Errorf("pointers: instance %q schema %q", e.Instance, e.Schema)
	}
}

// A schema built to explode (nested anyOf through $ref) stops at the step
// budget instead of holding a core.
func TestJSONSchemaBudget(t *testing.T) {
	schema := `{"properties":{"a":{"anyOf":[{"$ref":"#"},{"$ref":"#"},{"$ref":"#"}]}},"required":["missing"]}`
	inst := strings.Repeat(`{"a":`, 40) + "1" + strings.Repeat("}", 40)
	_, err := handleJSONValidate(context.Background(), nil, []byte(`{"schema":`+schema+`,"instance":`+inst+`}`))
	if !errors.Is(err, errBudget) {
		t.Errorf("exponential schema: err = %v, want the budget error", err)
	}
}

func TestJSONValidateArguments(t *testing.T) {
	for _, bad := range []string{
		`{}`, `{"json":"1","instance":1}`, `{"instance":1,"schema":"string"}`, `{"instance":1,"schema":[1]}`,
		`{"json":"1","extra":true}`,
	} {
		if _, err := handleJSONValidate(context.Background(), nil, []byte(bad)); err == nil {
			t.Errorf("%s: must be refused", bad)
		}
	}
	// An explicit null instance is an instance.
	res := validateJSON(t, `{"instance":null,"schema":{"type":"null"}}`)
	if res.Verdict != "valid" {
		t.Errorf("null instance: %s", res.Verdict)
	}
	// Without a schema, a value instance is valid JSON by construction.
	if res := validateJSON(t, `{"instance":[1,2]}`); res.Verdict != "valid" || res.SchemaChecked {
		t.Errorf("no schema: %+v", res)
	}
}
