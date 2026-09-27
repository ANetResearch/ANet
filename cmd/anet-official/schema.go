package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"reflect"
	"regexp"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A JSON Schema validator for the subset of 2020-12 (and draft-07) that can
// be evaluated without fetching anything and within a fixed budget.
//
// Written here rather than taken from a library for two reasons. The
// libraries at hand follow remote $ref through a loader and evaluate
// anyOf/oneOf without a bound, so a 512 KiB schema could keep a core busy
// indefinitely; this one counts every subschema application and stops at
// maxSchemaSteps. And a library answers "valid" or an error; this one
// answers valid, invalid or unknown, because a keyword it does not
// evaluate (unevaluatedProperties, $dynamicRef) is not a keyword that
// passed.
//
// Semantics that differ from a full implementation are listed in the
// capability description: $ref is followed only within the schema
// document ("#", "#/pointer", "#anchor"); $ref siblings are applied (the
// 2020-12 rule, also for draft-07 schemas); "format" is an annotation and
// not asserted; "pattern" uses RE2 syntax, and a pattern RE2 cannot compile
// makes its keyword unknown.
//
// Each schema object is compiled once, on first use, into a cschema that
// holds its keywords already parsed; evaluation then does no map lookups
// on the schema and no number parsing of schema values.
//
// The budget counts work, not only subschema applications: an enum
// comparison, a pattern match or a property scan costs in proportion to
// the size of what it reads (charge). Counting applications alone let
// {"anyOf": [2000 × {"pattern": ...}]} over one 400 KiB string run for a
// minute on 2,000 steps, far past the deadline, which a handler that never
// reaches a budget check cannot see.

// verdict is the tri-state result of applying a schema.
type verdict uint8

const (
	vPass verdict = iota
	vFail
	vUnknown
)

func (v verdict) String() string {
	switch v {
	case vPass:
		return "valid"
	case vFail:
		return "invalid"
	}
	return "unknown"
}

// and combines results that must all hold: a failure decides, then an
// unknown.
func and(a, b verdict) verdict {
	if a == vFail || b == vFail {
		return vFail
	}
	if a == vUnknown || b == vUnknown {
		return vUnknown
	}
	return vPass
}

func not(v verdict) verdict {
	switch v {
	case vPass:
		return vFail
	case vFail:
		return vPass
	}
	return vUnknown
}

// schemaError is one definite failure of the instance.
type schemaError struct {
	Instance string `json:"instance"`
	Schema   string `json:"schema"`
	Keyword  string `json:"keyword"`
	Message  string `json:"message"`
}

const (
	maxSchemaSteps  = 2_000_000
	maxSchemaErrors = 100
	maxSchemaDepth  = 256
	// budgetCheckEvery is how many steps may pass between two looks at the
	// deadline.
	budgetCheckEvery = 4096
)

// ipath is an instance location, built as a linked list so that descending
// costs one small allocation and the string is made only for a report.
// The root is the nil *ipath.
type ipath struct {
	parent *ipath
	key    string
	idx    int
	isIdx  bool
}

func (p *ipath) child(key string) *ipath { return &ipath{parent: p, key: key} }
func (p *ipath) item(i int) *ipath       { return &ipath{parent: p, idx: i, isIdx: true} }

func (p *ipath) String() string {
	var toks []string
	for n := p; n != nil; n = n.parent {
		if n.isIdx {
			toks = append(toks, strconv.Itoa(n.idx))
		} else {
			toks = append(toks, escapePtr(n.key))
		}
	}
	var b strings.Builder
	for i := len(toks) - 1; i >= 0; i-- {
		b.WriteByte('/')
		b.WriteString(toks[i])
	}
	return b.String()
}

// cschema is one compiled schema.
type cschema struct {
	loc     string // location in the schema document (JSON Pointer)
	boolean *bool

	ref    *cschema
	refRaw string
	refLoc string

	types   []string
	enum    []string // canonical forms
	hasEnum bool
	konst   *string // canonical form

	allOf, anyOf, oneOf []*cschema
	not                 *cschema
	ifS, thenS, elseS   *cschema

	minimum, maximum, exclMin, exclMax, multipleOf *big.Rat
	minLength, maxLength                           int // -1 when absent
	pattern                                        *regexp.Regexp
	patternSrc                                     string
	patternCost                                    int // program size, for the budget

	minItems, maxItems       int
	uniqueItems              bool
	prefix                   []*cschema
	rest                     *cschema
	contains                 *cschema
	minContains, maxContains int

	minProps, maxProps int
	required           []string
	depRequired        []depReq
	depSchemas         []depSchema
	props              map[string]*cschema
	propOrder          []string // the names in props, sorted
	patProps           []patProp
	addl               *cschema
	propNames          *cschema

	// unknown lists the locations of keywords that make this schema's
	// verdict unknown whenever it would otherwise pass: unsupported
	// keywords, malformed keyword values, references not followed,
	// patterns RE2 cannot compile.
	unknown []string
}

type depReq struct {
	kw, name string
	needs    []string
}

type depSchema struct {
	name string
	s    *cschema
}

type patProp struct {
	re   *regexp.Regexp // nil when RE2 cannot compile it
	cost int
	s    *cschema
}

type schemaValidator struct {
	ctx       context.Context
	root      any
	anchors   map[string]any
	compiled  map[uintptr]*cschema
	steps     int
	nextCheck int
	// err is set, and stays set, when the budget or the deadline ran out;
	// every evaluation after that returns at once.
	err    error
	depth  int
	active map[activeKey]bool
	// rats caches instance numbers as rationals: the same literal is
	// compared against many subschemas.
	rats map[json.Number]*big.Rat
	// keys caches the sorted member names of instance objects.
	keys map[uintptr][]string
	// patterns caches compiled patterns by source.
	patterns map[string]*compiledPattern

	errs          []schemaError
	errsTruncated bool
	schemaIssues  issues
	unsupported   map[string]bool
	noted         map[string]bool
	compileErr    error
}

// activeKey identifies a $ref being followed at one instance location. The
// same *ipath is passed down while the evaluation stays at one location, so
// a pair seen again is a cycle that consumes nothing.
type activeKey struct {
	s *cschema
	p *ipath
}

func newSchemaValidator(ctx context.Context, root any) *schemaValidator {
	v := &schemaValidator{ctx: ctx, root: root, anchors: map[string]any{}, compiled: map[uintptr]*cschema{},
		active: map[activeKey]bool{}, unsupported: map[string]bool{}, noted: map[string]bool{},
		rats: map[json.Number]*big.Rat{}, keys: map[uintptr][]string{},
		patterns: map[string]*compiledPattern{}, nextCheck: budgetCheckEvery}
	v.collectAnchors(root, 0)
	if m, ok := root.(map[string]any); ok {
		if s, ok := m["$schema"].(string); ok {
			switch strings.TrimSuffix(s, "#") {
			case "https://json-schema.org/draft/2020-12/schema", "http://json-schema.org/draft-07/schema",
				"https://json-schema.org/draft-07/schema":
			default:
				v.schemaIssues.add(sevWarning, "/$schema", "dialect",
					"$schema %q is not 2020-12 or draft-07; validated with 2020-12 rules", s)
			}
		}
	}
	return v
}

func (v *schemaValidator) collectAnchors(s any, depth int) {
	if depth > maxSchemaDepth {
		return
	}
	switch t := s.(type) {
	case map[string]any:
		if a, ok := t["$anchor"].(string); ok && a != "" {
			v.anchors[a] = t
		}
		// draft-07 plain-name fragments: {"$id": "#foo"}.
		if id, ok := t["$id"].(string); ok && strings.HasPrefix(id, "#") && len(id) > 1 {
			v.anchors[id[1:]] = t
		}
		for _, k := range sortedKeys(t) {
			v.collectAnchors(t[k], depth+1)
		}
	case []any:
		for _, e := range t {
			v.collectAnchors(e, depth+1)
		}
	}
}

// run validates inst against the root schema.
func (v *schemaValidator) run(inst any) (verdict, error) {
	root := v.compile(v.root, "", 0)
	if v.compileErr != nil {
		return vUnknown, v.compileErr
	}
	if v.err != nil {
		return vUnknown, v.err
	}
	return v.apply(inst, root, nil, true)
}

// charge spends n steps of the budget and reports whether any is left. It
// looks at the deadline every budgetCheckEvery steps, however they were
// spent.
func (v *schemaValidator) charge(n int) bool {
	if v.err != nil {
		return false
	}
	v.steps += n
	if v.steps > maxSchemaSteps {
		v.err = errBudget
		return false
	}
	if v.steps >= v.nextCheck {
		v.nextCheck = v.steps + budgetCheckEvery
		if v.ctx.Err() != nil {
			v.err = errBudget
			return false
		}
	}
	return true
}

// canonOf is canon(x), charged by the length of what it wrote.
func (v *schemaValidator) canonOf(x any) string {
	s := canon(x)
	v.charge(len(s)/32 + 1)
	return s
}

// ratOf is ratOf(n) for an instance number, cached and charged.
func (v *schemaValidator) ratOf(n json.Number) (*big.Rat, bool) {
	if r, ok := v.rats[n]; ok {
		return r, r != nil
	}
	v.charge(len(n)/16 + 1)
	r, ok := ratOf(n)
	if !ok {
		r = nil
	} else {
		v.charge((r.Num().BitLen() + r.Denom().BitLen()) / 64)
	}
	v.rats[n] = r
	return r, ok
}

// patternCost is the size of the program RE2 runs for p: matching costs
// about this much per byte of input.
func patternCost(p string) int {
	re, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		return 1
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return 1
	}
	return max(len(prog.Inst), 1)
}

// matchCost is what matching a pattern of the given cost against s is
// charged.
func matchCost(s string, cost int) int { return len(s)*cost/256 + 1 }

// sortedKeysOf is sortedKeys(obj), sorted once per object: a scan of the
// same object under many subschemas then costs one lookup per member.
func (v *schemaValidator) sortedKeysOf(obj map[string]any) []string {
	id := reflect.ValueOf(obj).Pointer()
	if ks, ok := v.keys[id]; ok && len(ks) == len(obj) {
		return ks
	}
	ks := sortedKeys(obj)
	v.charge(len(ks) * 4)
	v.keys[id] = ks
	return ks
}

// unsupportedKeywords are keywords this validator does not evaluate. A
// schema using one gets "unknown" wherever the keyword would decide.
var unsupportedKeywords = []string{"unevaluatedProperties", "unevaluatedItems", "$dynamicRef", "$recursiveRef"}

// compile compiles schema node s found at loc. A node reached twice (by a
// $ref, or a cycle) is compiled once.
func (v *schemaValidator) compile(s any, loc string, depth int) *cschema {
	if depth > maxSchemaDepth {
		v.compileErr = errBudget
		return &cschema{loc: loc}
	}
	switch t := s.(type) {
	case bool:
		b := t
		return &cschema{boolean: &b, loc: loc}
	case map[string]any:
		id := reflect.ValueOf(t).Pointer()
		if c, ok := v.compiled[id]; ok {
			return c
		}
		c := &cschema{loc: loc, minLength: -1, maxLength: -1, minItems: -1, maxItems: -1,
			minContains: -1, maxContains: -1, minProps: -1, maxProps: -1}
		v.compiled[id] = c
		if !v.charge(len(t) + 1) {
			v.compileErr = v.err
			return c
		}
		v.compileObject(c, t, loc, depth)
		return c
	default:
		v.schemaIssues.add(sevError, loc, "not_a_schema", "a schema must be an object or a boolean")
		return &cschema{loc: loc, unknown: []string{loc}, minLength: -1, maxLength: -1, minItems: -1, maxItems: -1,
			minContains: -1, maxContains: -1, minProps: -1, maxProps: -1}
	}
}

func (v *schemaValidator) compileObject(c *cschema, s map[string]any, loc string, depth int) {
	kw := func(k string) string { return ptr(loc, k) }
	bad := func(k, format string, a ...any) {
		v.schemaIssues.add(sevError, kw(k), "bad_keyword", format, a...)
		c.unknown = append(c.unknown, kw(k))
	}
	sub := func(x any, l string) *cschema { return v.compile(x, l, depth+1) }
	for _, k := range unsupportedKeywords {
		if _, ok := s[k]; ok {
			c.unknown = append(c.unknown, kw(k))
			v.unsupported[kw(k)] = true
		}
	}
	if f, ok := s["format"].(string); ok && !v.noted["format"] {
		v.noted["format"] = true
		v.schemaIssues.add(sevInfo, kw("format"), "format_not_asserted", "\"format\" (%s) is an annotation and is not asserted", f)
	}
	if ref, ok := s["$ref"].(string); ok {
		c.refRaw, c.refLoc = ref, kw("$ref")
		target, where, rerr := v.resolveRef(ref)
		if rerr != "" {
			v.schemaIssues.add(sevError, kw("$ref"), "ref_not_followed", "%s", rerr)
			c.unknown = append(c.unknown, kw("$ref"))
			v.unsupported[kw("$ref")] = true
		} else {
			c.ref = sub(target, where)
		}
	}
	if t, ok := s["type"]; ok {
		switch tt := t.(type) {
		case string:
			c.types = []string{tt}
		case []any:
			c.types = []string{}
			for _, e := range tt {
				if s, ok := e.(string); ok {
					c.types = append(c.types, s)
				}
			}
		default:
			bad("type", "type must be a string or an array of strings")
		}
	}
	if e, ok := s["enum"]; ok {
		if list, isList := e.([]any); isList {
			c.hasEnum = true
			for _, x := range list {
				c.enum = append(c.enum, v.canonOf(x))
			}
		} else {
			bad("enum", "enum must be an array")
		}
	}
	if k, ok := s["const"]; ok {
		cc := v.canonOf(k)
		c.konst = &cc
	}
	subList := func(k string) []*cschema {
		raw, ok := s[k]
		if !ok {
			return nil
		}
		list, isList := raw.([]any)
		if !isList || len(list) == 0 {
			bad(k, "%s must be a non-empty array of schemas", k)
			return nil
		}
		out := make([]*cschema, len(list))
		for i, x := range list {
			out[i] = sub(x, ptr(kw(k), i))
		}
		return out
	}
	subOne := func(k string) *cschema {
		raw, ok := s[k]
		if !ok {
			return nil
		}
		return sub(raw, kw(k))
	}
	c.allOf, c.anyOf, c.oneOf = subList("allOf"), subList("anyOf"), subList("oneOf")
	c.not = subOne("not")
	c.ifS, c.thenS, c.elseS = subOne("if"), subOne("then"), subOne("else")

	rat := func(k string, positive bool) *big.Rat {
		raw, ok := s[k]
		if !ok {
			return nil
		}
		if _, isBool := raw.(bool); isBool {
			bad(k, "%s as a boolean is draft-04 syntax; use a number", k)
			return nil
		}
		r, ok := ratOf(raw)
		if n, isNum := raw.(json.Number); !ok && isNum {
			if _, valid := decimalOf(string(n)); valid {
				// A number, just not one this validator computes with.
				v.schemaIssues.add(sevWarning, kw(k), "number_out_of_range",
					"%s %.40s is too large or too precise to evaluate", k, string(n))
				c.unknown = append(c.unknown, kw(k))
				v.unsupported[kw(k)] = true
				return nil
			}
		}
		if !ok || (positive && r.Sign() <= 0) {
			if positive {
				bad(k, "%s must be a number greater than 0", k)
			} else {
				bad(k, "%s must be a number", k)
			}
			return nil
		}
		return r
	}
	c.minimum, c.maximum = rat("minimum", false), rat("maximum", false)
	c.exclMin, c.exclMax = rat("exclusiveMinimum", false), rat("exclusiveMaximum", false)
	c.multipleOf = rat("multipleOf", true)
	count := func(k string) int {
		raw, ok := s[k]
		if !ok {
			return -1
		}
		num, isNum := raw.(json.Number)
		d, ok := decimalOf(string(num))
		if !isNum || !ok || !d.isInteger() || (d.neg && !d.isZero()) {
			bad(k, "%s must be a non-negative integer", k)
			return -1
		}
		if d.isZero() {
			return 0
		}
		// Anything past 2^31 bounds nothing a 512 KiB instance can hold.
		if !d.exp.IsInt64() || d.exp.Int64()+int64(len(d.digits)) > 10 {
			return 1 << 31
		}
		r, _ := new(big.Int).SetString(d.digits+strings.Repeat("0", int(d.exp.Int64())), 10)
		if r == nil || !r.IsInt64() || r.Int64() > 1<<31 {
			return 1 << 31
		}
		return int(r.Int64())
	}
	c.minLength, c.maxLength = count("minLength"), count("maxLength")
	c.minItems, c.maxItems = count("minItems"), count("maxItems")
	c.minContains, c.maxContains = count("minContains"), count("maxContains")
	c.minProps, c.maxProps = count("minProperties"), count("maxProperties")
	if p, ok := s["pattern"].(string); ok {
		c.patternSrc = p
		if c.pattern, c.patternCost = v.regexp(p, kw("pattern")); c.pattern == nil {
			c.unknown = append(c.unknown, kw("pattern"))
		}
	}
	c.uniqueItems, _ = s["uniqueItems"].(bool)

	// Positional items: 2020-12 prefixItems + items, or draft-07 items
	// (array) + additionalItems.
	restKw := "items"
	restRaw, hasRest := s["items"]
	if p, ok := s["prefixItems"].([]any); ok {
		for i, x := range p {
			c.prefix = append(c.prefix, sub(x, ptr(kw("prefixItems"), i)))
		}
	} else if p, ok := s["items"].([]any); ok {
		for i, x := range p {
			c.prefix = append(c.prefix, sub(x, ptr(kw("items"), i)))
		}
		restKw = "additionalItems"
		restRaw, hasRest = s["additionalItems"]
	}
	if hasRest {
		if _, isArr := restRaw.([]any); !isArr {
			c.rest = sub(restRaw, kw(restKw))
		}
	}
	c.contains = subOne("contains")

	if req, ok := s["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				c.required = append(c.required, name)
			}
		}
	}
	for _, depKw := range []string{"dependentRequired", "dependentSchemas", "dependencies"} {
		m, ok := s[depKw].(map[string]any)
		if !ok {
			continue
		}
		for _, name := range sortedKeys(m) {
			if list, isList := m[name].([]any); isList {
				d := depReq{kw: depKw, name: name}
				for _, x := range list {
					if s, ok := x.(string); ok {
						d.needs = append(d.needs, s)
					}
				}
				c.depRequired = append(c.depRequired, d)
			} else {
				c.depSchemas = append(c.depSchemas, depSchema{name: name, s: sub(m[name], ptr(kw(depKw), name))})
			}
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		c.props = make(map[string]*cschema, len(props))
		c.propOrder = sortedKeys(props)
		for _, name := range c.propOrder {
			c.props[name] = sub(props[name], ptr(kw("properties"), name))
		}
	}
	if pats, ok := s["patternProperties"].(map[string]any); ok {
		for _, p := range sortedKeys(pats) {
			l := ptr(kw("patternProperties"), p)
			re, cost := v.regexp(p, l)
			if re == nil {
				c.unknown = append(c.unknown, l)
			}
			c.patProps = append(c.patProps, patProp{re: re, cost: cost, s: sub(pats[p], l)})
		}
	}
	c.addl = subOne("additionalProperties")
	c.propNames = subOne("propertyNames")
}

// resolveRef finds the schema a local reference names, and its location.
func (v *schemaValidator) resolveRef(ref string) (any, string, string) {
	if !strings.HasPrefix(ref, "#") {
		return nil, "", fmt.Sprintf("$ref %q is not a local reference; remote schemas are never fetched", ref)
	}
	frag, err := url.PathUnescape(ref[1:])
	if err != nil {
		return nil, "", fmt.Sprintf("$ref %q: bad percent-encoding", ref)
	}
	if frag == "" {
		return v.root, "", ""
	}
	if !strings.HasPrefix(frag, "/") {
		if t, ok := v.anchors[frag]; ok {
			return t, "#" + frag, ""
		}
		return nil, "", fmt.Sprintf("$ref %q names no $anchor in this schema", ref)
	}
	cur := v.root
	for _, tok := range strings.Split(frag[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			next, ok := c[tok]
			if !ok {
				return nil, "", fmt.Sprintf("$ref %q: no member %q", ref, tok)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil, "", fmt.Sprintf("$ref %q: no element %q", ref, tok)
			}
			cur = c[i]
		default:
			return nil, "", fmt.Sprintf("$ref %q: path runs through a non-container", ref)
		}
	}
	return cur, frag, ""
}

// compiledPattern is one pattern source, compiled once per validation.
type compiledPattern struct {
	re   *regexp.Regexp // nil when RE2 cannot compile it
	cost int
	err  error
}

// regexp compiles pattern p found at loc, charging the budget by the size
// of its program: a schema can name thousands of patterns, and compiling
// one of "[a-z]{1000}" is a thousand instructions.
func (v *schemaValidator) regexp(p, loc string) (*regexp.Regexp, int) {
	cp, ok := v.patterns[p]
	if !ok {
		cp = &compiledPattern{cost: patternCost(p)}
		if v.charge(cp.cost*4 + len(p)/16) {
			cp.re, cp.err = regexp.Compile(p)
		}
		v.patterns[p] = cp
	}
	if cp.err != nil {
		v.schemaIssues.add(sevWarning, loc, "pattern_unsupported",
			"pattern %q does not compile as RE2 (%v); ECMA-262 features such as lookaround are not supported", p, cp.err)
		v.unsupported[loc] = true
	}
	return cp.re, cp.cost
}

func (v *schemaValidator) fail(record bool, p *ipath, loc, kw, format string, a ...any) verdict {
	if record {
		if len(v.errs) >= maxSchemaErrors {
			v.errsTruncated = true
		} else {
			v.errs = append(v.errs, schemaError{Instance: clip(p.String(), maxReportedPath), Schema: clip(loc, maxReportedPath),
				Keyword: kw, Message: clip(fmt.Sprintf(format, a...), maxReportedMessage)})
		}
	}
	return vFail
}

// apply applies compiled schema c to inst at instance location p. With
// record, definite failures are recorded; inside anyOf, oneOf, not, if and
// contains, branches run without recording.
func (v *schemaValidator) apply(inst any, c *cschema, p *ipath, record bool) (verdict, error) {
	if !v.charge(1) {
		return vUnknown, v.err
	}
	v.depth++
	defer func() { v.depth-- }()
	if v.depth > maxSchemaDepth {
		return vUnknown, errBudget
	}
	if c.boolean != nil {
		if *c.boolean {
			return vPass, nil
		}
		return v.fail(record, p, c.loc, "false", "the schema is false: no value is valid here"), nil
	}

	res := vPass
	if len(c.unknown) > 0 {
		res = vUnknown
	}
	var err error
	sub := func(inst any, s *cschema, p *ipath, rec bool) verdict {
		if err != nil {
			return vUnknown
		}
		var r verdict
		r, err = v.apply(inst, s, p, rec)
		return r
	}

	if c.ref != nil {
		key := activeKey{c.ref, p}
		if v.active[key] {
			v.schemaIssues.add(sevError, c.refLoc, "ref_cycle", "$ref %q loops without consuming the instance", c.refRaw)
			v.unsupported[c.refLoc] = true
			res = and(res, vUnknown)
		} else {
			v.active[key] = true
			res = and(res, sub(inst, c.ref, p, record))
			delete(v.active, key)
		}
	}
	if c.types != nil {
		if n, ok := inst.(json.Number); ok {
			v.charge(len(n)/64 + 1) // typeOf reads the literal
		}
		got := typeOf(inst)
		ok := false
		for _, w := range c.types {
			if w == got || (w == "number" && got == "integer") {
				ok = true
				break
			}
		}
		if !ok {
			res = and(res, v.fail(record, p, ptr(c.loc, "type"), "type", "type is %s, want %s", got, strings.Join(c.types, " or ")))
		}
	}
	if c.hasEnum {
		ci := v.canonOf(inst)
		v.charge(len(c.enum))
		found := false
		for _, e := range c.enum {
			if e == ci {
				found = true
				break
			}
		}
		if !found {
			res = and(res, v.fail(record, p, ptr(c.loc, "enum"), "enum", "value is not one of the %d allowed values", len(c.enum)))
		}
	}
	if c.konst != nil && v.canonOf(inst) != *c.konst {
		res = and(res, v.fail(record, p, ptr(c.loc, "const"), "const", "value is not the required constant"))
	}

	for _, s := range c.allOf {
		res = and(res, sub(inst, s, p, record))
	}
	if c.anyOf != nil {
		some := vFail
		for _, s := range c.anyOf {
			r := sub(inst, s, p, false)
			if r == vPass {
				some = vPass
				break
			}
			if r == vUnknown {
				some = vUnknown
			}
		}
		if some == vFail {
			v.fail(record, p, ptr(c.loc, "anyOf"), "anyOf", "value matches none of the %d anyOf schemas", len(c.anyOf))
		}
		res = and(res, some)
	}
	if c.oneOf != nil {
		passed, unknown := 0, 0
		for _, s := range c.oneOf {
			switch sub(inst, s, p, false) {
			case vPass:
				passed++
			case vUnknown:
				unknown++
			}
		}
		switch {
		case passed > 1:
			res = and(res, v.fail(record, p, ptr(c.loc, "oneOf"), "oneOf", "value matches %d oneOf schemas, not exactly one", passed))
		case unknown > 0:
			res = and(res, vUnknown)
		case passed == 0:
			res = and(res, v.fail(record, p, ptr(c.loc, "oneOf"), "oneOf", "value matches none of the %d oneOf schemas", len(c.oneOf)))
		}
	}
	if c.not != nil {
		r := not(sub(inst, c.not, p, false))
		if r == vFail {
			v.fail(record, p, ptr(c.loc, "not"), "not", "value matches the schema under \"not\"")
		}
		res = and(res, r)
	}
	if c.ifS != nil {
		switch sub(inst, c.ifS, p, false) {
		case vPass:
			if c.thenS != nil {
				res = and(res, sub(inst, c.thenS, p, record))
			}
		case vFail:
			if c.elseS != nil {
				res = and(res, sub(inst, c.elseS, p, record))
			}
		default:
			res = and(res, vUnknown)
		}
	}

	switch t := inst.(type) {
	case json.Number:
		res = and(res, v.checkNumber(t, c, p, record))
	case string:
		res = and(res, v.checkString(t, c, p, record))
	case []any:
		res = and(res, v.checkArray(t, c, p, record, sub))
	case map[string]any:
		res = and(res, v.checkObject(t, c, p, record, sub))
	}
	if err == nil {
		err = v.err
	}
	if err != nil {
		return vUnknown, err
	}
	return res, nil
}

func typeOf(inst any) string {
	switch t := inst.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		if isIntegerLiteral(string(t)) || numberIsInteger(t) {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

// isIntegerLiteral is the fast path of typeOf: a literal with no fraction
// and no exponent is an integer without parsing it.
func isIntegerLiteral(s string) bool {
	if s == "" || s == "-" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && !(i == 0 && s[i] == '-') {
			return false
		}
	}
	return true
}

func (v *schemaValidator) checkNumber(n json.Number, c *cschema, p *ipath, record bool) verdict {
	if c.minimum == nil && c.maximum == nil && c.exclMin == nil && c.exclMax == nil && c.multipleOf == nil {
		return vPass
	}
	x, ok := v.ratOf(n)
	if !ok {
		return vUnknown
	}
	res := vPass
	check := func(b *big.Rat, kw, rel string, ok func(int) bool) {
		if b != nil && !ok(x.Cmp(b)) {
			res = and(res, v.fail(record, p, ptr(c.loc, kw), kw, "%s is not %s %s", n, rel, b.RatString()))
		}
	}
	check(c.minimum, "minimum", ">=", func(r int) bool { return r >= 0 })
	check(c.maximum, "maximum", "<=", func(r int) bool { return r <= 0 })
	check(c.exclMin, "exclusiveMinimum", ">", func(r int) bool { return r > 0 })
	check(c.exclMax, "exclusiveMaximum", "<", func(r int) bool { return r < 0 })
	if c.multipleOf != nil {
		v.charge((x.Num().BitLen() + x.Denom().BitLen() + c.multipleOf.Num().BitLen() + c.multipleOf.Denom().BitLen()) / 64)
		if q := new(big.Rat).Quo(x, c.multipleOf); !q.IsInt() {
			res = and(res, v.fail(record, p, ptr(c.loc, "multipleOf"), "multipleOf", "%s is not a multiple of %s", n, c.multipleOf.RatString()))
		}
	}
	return res
}

func (v *schemaValidator) checkString(str string, c *cschema, p *ipath, record bool) verdict {
	res := vPass
	if c.minLength >= 0 || c.maxLength >= 0 {
		v.charge(len(str)/64 + 1)
		length := utf8.RuneCountInString(str)
		if c.minLength >= 0 && length < c.minLength {
			res = and(res, v.fail(record, p, ptr(c.loc, "minLength"), "minLength", "length %d is less than %d", length, c.minLength))
		}
		if c.maxLength >= 0 && length > c.maxLength {
			res = and(res, v.fail(record, p, ptr(c.loc, "maxLength"), "maxLength", "length %d is more than %d", length, c.maxLength))
		}
	}
	if c.pattern != nil && v.charge(matchCost(str, c.patternCost)) && !c.pattern.MatchString(str) {
		res = and(res, v.fail(record, p, ptr(c.loc, "pattern"), "pattern", "does not match pattern %q", c.patternSrc))
	}
	return res
}

type applyFunc func(inst any, s *cschema, p *ipath, rec bool) verdict

func (v *schemaValidator) checkArray(arr []any, c *cschema, p *ipath, record bool, sub applyFunc) verdict {
	res := vPass
	if c.minItems >= 0 && len(arr) < c.minItems {
		res = and(res, v.fail(record, p, ptr(c.loc, "minItems"), "minItems", "%d items, fewer than %d", len(arr), c.minItems))
	}
	if c.maxItems >= 0 && len(arr) > c.maxItems {
		res = and(res, v.fail(record, p, ptr(c.loc, "maxItems"), "maxItems", "%d items, more than %d", len(arr), c.maxItems))
	}
	if c.uniqueItems {
		seen := make(map[string]int, len(arr))
		for i, e := range arr {
			k := v.canonOf(e)
			if v.err != nil {
				return vUnknown
			}
			if j, dup := seen[k]; dup {
				res = and(res, v.fail(record, p.item(i), ptr(c.loc, "uniqueItems"), "uniqueItems", "item %d equals item %d", i, j))
				break
			}
			seen[k] = i
		}
	}
	for i := 0; i < len(c.prefix) && i < len(arr); i++ {
		res = and(res, sub(arr[i], c.prefix[i], p.item(i), record))
	}
	if c.rest != nil {
		for i := len(c.prefix); i < len(arr); i++ {
			res = and(res, sub(arr[i], c.rest, p.item(i), record))
		}
	}
	if c.contains != nil {
		minC := c.minContains
		if minC < 0 {
			minC = 1
		}
		passed, unknown := 0, 0
		for i, e := range arr {
			switch sub(e, c.contains, p.item(i), false) {
			case vPass:
				passed++
			case vUnknown:
				unknown++
			}
		}
		hasMax := c.maxContains >= 0
		switch {
		case hasMax && passed > c.maxContains:
			res = and(res, v.fail(record, p, ptr(c.loc, "maxContains"), "maxContains", "%d items match \"contains\", more than %d", passed, c.maxContains))
		case passed >= minC && (!hasMax || passed+unknown <= c.maxContains):
		case passed+unknown < minC:
			res = and(res, v.fail(record, p, ptr(c.loc, "contains"), "contains", "%d items match \"contains\", fewer than %d", passed, minC))
		default:
			res = and(res, vUnknown)
		}
	}
	return res
}

func (v *schemaValidator) checkObject(obj map[string]any, c *cschema, p *ipath, record bool, sub applyFunc) verdict {
	res := vPass
	if c.minProps >= 0 && len(obj) < c.minProps {
		res = and(res, v.fail(record, p, ptr(c.loc, "minProperties"), "minProperties", "%d properties, fewer than %d", len(obj), c.minProps))
	}
	if c.maxProps >= 0 && len(obj) > c.maxProps {
		res = and(res, v.fail(record, p, ptr(c.loc, "maxProperties"), "maxProperties", "%d properties, more than %d", len(obj), c.maxProps))
	}
	for _, name := range c.required {
		if _, present := obj[name]; !present {
			res = and(res, v.fail(record, p, ptr(c.loc, "required"), "required", "required property %q is missing", name))
		}
	}
	for _, d := range c.depRequired {
		if _, present := obj[d.name]; !present {
			continue
		}
		for _, need := range d.needs {
			if _, present := obj[need]; !present {
				res = and(res, v.fail(record, p, ptr(ptr(c.loc, d.kw), d.name), d.kw, "property %q requires %q", d.name, need))
			}
		}
	}
	for _, d := range c.depSchemas {
		if _, present := obj[d.name]; present {
			res = and(res, sub(obj, d.s, p, record))
		}
	}
	if c.props == nil && c.patProps == nil && c.addl == nil && c.propNames == nil {
		return res
	}
	if c.patProps == nil && c.addl == nil && c.propNames == nil {
		// Only "properties": look up the schema's names in the object
		// rather than scanning every member. Same order as the scan below.
		if !v.charge(len(c.propOrder)) {
			return vUnknown
		}
		for _, name := range c.propOrder {
			if val, ok := obj[name]; ok {
				res = and(res, sub(val, c.props[name], p.child(name), record))
			}
		}
		return res
	}
	if !v.charge(2 * len(obj)) {
		return vUnknown
	}
	for _, name := range v.sortedKeysOf(obj) {
		val := obj[name]
		cp := p.child(name)
		matched := false
		if s, ok := c.props[name]; ok {
			matched = true
			res = and(res, sub(val, s, cp, record))
		}
		for _, pp := range c.patProps {
			if pp.re != nil && v.charge(matchCost(name, pp.cost)) && pp.re.MatchString(name) {
				matched = true
				res = and(res, sub(val, pp.s, cp, record))
			}
		}
		if !matched && c.addl != nil {
			if c.addl.boolean != nil && !*c.addl.boolean {
				res = and(res, v.fail(record, cp, c.addl.loc, "additionalProperties", "property %q is not allowed", name))
			} else {
				res = and(res, sub(val, c.addl, cp, record))
			}
		}
		if c.propNames != nil {
			res = and(res, sub(name, c.propNames, cp, record))
		}
	}
	return res
}

// canon renders a JSON value so that two values are equal exactly when
// their renderings are: numbers by value (1 and 1.0 are equal), objects
// with sorted member names, strings length-prefixed rather than quoted
// (unambiguous, and a copy rather than a rune-by-rune escape).
func canon(v any) string {
	var b strings.Builder
	writeCanon(&b, v)
	return b.String()
}

func writeCanon(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		if d, ok := decimalOf(string(t)); ok {
			b.WriteString("n" + d.canonical())
		} else {
			b.WriteString("n?" + string(t))
		}
	case string:
		writeCanonString(b, t)
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanon(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonString(b, k)
			b.WriteByte(':')
			writeCanon(b, t[k])
		}
		b.WriteByte('}')
	default:
		fmt.Fprintf(b, "?%v", t)
	}
}

func writeCanonString(b *strings.Builder, s string) {
	b.WriteByte('s')
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteByte(':')
	b.WriteString(s)
}
