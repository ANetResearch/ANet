// Package tsir implements the L4 Task Semantic Intermediate Representation: the closed
// predicate calculus (this file), and the TaskDoc waist object + CID (taskdoc.go).
//
// Normative source: design3/spec/tsir-spec.md §2.4/§3.3/§5.5. The predicate calculus is
// the shared decidability substrate behind the acceptance predicate, the negative
// action-scope hard gate, and the TSIR-compatibility relation. It is total, side-effect-
// free, bounded (non-Turing-complete), so two conformant implementations compute the same
// verdict. An unknown clause op makes the carrying TaskDoc MALFORMED (fail-closed).
package tsir

import (
	"errors"
	"strings"
)

// Structural bounds (tsir-spec §2.4, realising §3.3c boundedness).
const (
	MaxClausesPerPredicate = 64
	MaxPredicateDepth      = 16
)

// Predicate op codes (tsir-spec §2.4).
const (
	OpAND       = 1
	OpOR        = 2
	OpNOT       = 3
	OpArtifact  = 10
	OpTest      = 11
	OpThreshold = 12
	OpScope     = 13
)

// effect verbs / resource kinds / enums (tsir-spec §2.5, frozen 1..5).
const (
	VerbCreate = 1
	VerbModify = 2
	VerbDelete = 3
	VerbCall   = 4
	VerbGet    = 5
)

// threshold ops (tsir-spec §2.5).
const (
	CmpLT = 1
	CmpLE = 2
	CmpEQ = 3
	CmpGE = 4
	CmpGT = 5
)

// test status (tsir-spec §2.5).
const (
	StatusPass   = 1
	StatusFail   = 2
	StatusAbsent = 3
)

// resource-match kinds (tsir-spec §2.5).
const (
	MatchGlob   = 1
	MatchSet    = 2
	MatchPrefix = 3
)

// ErrMalformed is returned for an ill-formed predicate (unknown op, over-depth, etc.).
var ErrMalformed = errors.New("MALFORMED")

// Predicate is one node of the closed grammar (tsir-spec §2.4). Exactly one shape is set.
type Predicate struct {
	Op       int              `cbor:"1,keyasint"`
	Children []*Predicate     `cbor:"2,keyasint,omitempty"` // AND/OR/NOT
	Artifact *ArtifactClause  `cbor:"10,keyasint,omitempty"`
	Test     *TestClause      `cbor:"11,keyasint,omitempty"`
	Thresh   *ThresholdClause `cbor:"12,keyasint,omitempty"`
	Scope    *ScopeClause     `cbor:"13,keyasint,omitempty"`
}

type ArtifactClause struct {
	PathGlob     string   `cbor:"1,keyasint"`
	Exists       *bool    `cbor:"2,keyasint,omitempty"`
	MinSizeBytes *uint64  `cbor:"3,keyasint,omitempty"`
	SchemaRef    string   `cbor:"4,keyasint,omitempty"`
	Contains     []string `cbor:"5,keyasint,omitempty"`
}

type TestClause struct {
	TestID string `cbor:"1,keyasint"`
	Expect int    `cbor:"2,keyasint"` // 1 pass / 2 fail
}

type ThresholdClause struct {
	Metric string  `cbor:"1,keyasint"`
	Op     int     `cbor:"2,keyasint"`
	Value  float64 `cbor:"3,keyasint"`
}

type ScopeClause struct {
	Verb       int           `cbor:"1,keyasint"`
	Kind       int           `cbor:"2,keyasint"`
	Match      ResourceMatch `cbor:"3,keyasint"`
	MergeClass *int          `cbor:"4,keyasint,omitempty"` // tsir-spec design-fix: on scope-clause
}

type ResourceMatch struct {
	Kind int    `cbor:"1,keyasint"` // 1 glob / 2 set / 3 prefix
	Val  string `cbor:"2,keyasint"`
}

// ---- EffectRecord: the typed evaluation domain (tsir-spec §3.3a) ----

type EffectRecord struct {
	Artifacts []Artifact         `cbor:"1,keyasint,omitempty"`
	Tests     []TestResult       `cbor:"2,keyasint,omitempty"`
	Metrics   map[string]float64 `cbor:"3,keyasint,omitempty"`
	Resources []ResourceRef      `cbor:"4,keyasint,omitempty"`
	Effects   []EffectRef        `cbor:"5,keyasint,omitempty"`
}

type Artifact struct {
	Path       string `cbor:"1,keyasint"`
	SizeBytes  uint64 `cbor:"2,keyasint"`
	ContentCID string `cbor:"3,keyasint,omitempty"`
	MediaType  string `cbor:"4,keyasint,omitempty"`
}

type TestResult struct {
	ID     string `cbor:"1,keyasint"`
	Status int    `cbor:"2,keyasint"`
}

type ResourceRef struct {
	Kind       int    `cbor:"1,keyasint"`
	ID         string `cbor:"2,keyasint"`
	MergeClass int    `cbor:"3,keyasint"` // absent ⇒ 0 (none)
}

type EffectRef struct {
	Verb     int         `cbor:"1,keyasint"`
	Resource ResourceRef `cbor:"2,keyasint"`
}

// Validate checks structural well-formedness (totality/boundedness, §3.3c). Unknown op,
// over-depth, or a wrong-arity connective ⇒ ErrMalformed.
func (p *Predicate) Validate() error { return p.validate(0) }

func (p *Predicate) validate(depth int) error {
	if depth > MaxPredicateDepth {
		return ErrMalformed
	}
	switch p.Op {
	case OpAND, OpOR:
		if len(p.Children) < 2 || len(p.Children) > MaxClausesPerPredicate {
			return ErrMalformed
		}
		for _, c := range p.Children {
			if err := c.validate(depth + 1); err != nil {
				return err
			}
		}
	case OpNOT:
		if len(p.Children) != 1 {
			return ErrMalformed
		}
		return p.Children[0].validate(depth + 1)
	case OpArtifact:
		if p.Artifact == nil {
			return ErrMalformed
		}
		// schema_ref / contains[] need resolved CAS content that the EffectRecord does
		// not carry; evaluating them would be a silent always-false. Reject at validate
		// time (fail-loud) rather than evaluate to a misleading false (tsir-spec §3.4).
		if p.Artifact.SchemaRef != "" || len(p.Artifact.Contains) > 0 {
			return ErrMalformed
		}
	case OpTest:
		if p.Test == nil || (p.Test.Expect != StatusPass && p.Test.Expect != StatusFail) {
			return ErrMalformed
		}
	case OpThreshold:
		if p.Thresh == nil || p.Thresh.Op < CmpLT || p.Thresh.Op > CmpGT {
			return ErrMalformed
		}
	case OpScope:
		if p.Scope == nil || p.Scope.Verb < VerbCreate || p.Scope.Verb > VerbGet {
			return ErrMalformed
		}
		if p.Scope.Match.Kind < MatchGlob || p.Scope.Match.Kind > MatchPrefix {
			return ErrMalformed
		}
	default:
		return ErrMalformed // unknown op ⇒ fail-closed (§3.3b)
	}
	return nil
}

// Evaluate computes evaluate(predicate, EffectRecord) → bool by structural recursion
// (tsir-spec §3.3d). The predicate MUST already be Validate()-clean.
func (p *Predicate) Evaluate(e *EffectRecord) bool {
	switch p.Op {
	case OpAND:
		for _, c := range p.Children {
			if !c.Evaluate(e) {
				return false
			}
		}
		return true
	case OpOR:
		for _, c := range p.Children {
			if c.Evaluate(e) {
				return true
			}
		}
		return false
	case OpNOT:
		return !p.Children[0].Evaluate(e)
	case OpArtifact:
		return evalArtifact(p.Artifact, e)
	case OpTest:
		for _, tr := range e.Tests {
			if tr.ID == p.Test.TestID {
				return tr.Status == p.Test.Expect
			}
		}
		return false
	case OpThreshold:
		v, ok := e.Metrics[p.Thresh.Metric]
		if !ok {
			return false
		}
		return cmp(v, p.Thresh.Op, p.Thresh.Value)
	case OpScope:
		for _, ef := range e.Effects {
			if ef.Verb == p.Scope.Verb && ef.Resource.Kind == p.Scope.Kind &&
				matchResource(p.Scope.Match, ef.Resource.ID) {
				return true
			}
		}
		return false
	}
	return false
}

func evalArtifact(a *ArtifactClause, e *EffectRecord) bool {
	for _, art := range e.Artifacts {
		if !globMatch(a.PathGlob, art.Path) {
			continue
		}
		if a.Exists != nil && !*a.Exists {
			continue
		}
		if a.MinSizeBytes != nil && art.SizeBytes < *a.MinSizeBytes {
			continue
		}
		// schema_ref/contains require resolved CAS content (not in the EffectRecord) and
		// are rejected at validate time (see (*Predicate).validate, OpArtifact: ErrMalformed,
		// tsir-spec §3.4) — a Validate()-clean clause never reaches here carrying them, so
		// evaluation is over size/exists/path only (the §3.3d red/green core).
		return true
	}
	return false
}

func cmp(a float64, op int, b float64) bool {
	switch op {
	case CmpLT:
		return a < b
	case CmpLE:
		return a <= b
	case CmpEQ:
		return a == b // compares canonical-float values (tsir-spec design-fix)
	case CmpGE:
		return a >= b
	case CmpGT:
		return a > b
	}
	return false
}

// matchResource applies a resource-match against an id (tsir-spec §2.4/§3.2 C-D4).
func matchResource(m ResourceMatch, id string) bool {
	switch m.Kind {
	case MatchGlob:
		return globMatch(m.Val, id)
	case MatchPrefix:
		return strings.HasPrefix(id, strings.TrimSuffix(m.Val, "/")) // C-D4 prefix: trailing / stripped
	case MatchSet:
		for _, tok := range strings.Split(m.Val, ",") {
			if tok == id {
				return true
			}
		}
	}
	return false
}

// globMatch implements the C-D4 glob dialect: '*' matches within a path segment (not '/'),
// '**' crosses '/'. (tsir-spec §3.2 design-fix: POSIX fnmatch with explicit ** crossing.)
func globMatch(pat, s string) bool {
	return globHelper(pat, s)
}

func globHelper(pat, s string) bool {
	for len(pat) > 0 {
		if strings.HasPrefix(pat, "**") {
			rest := strings.TrimLeft(pat[2:], "/")
			if rest == "" {
				return true // ** matches the remainder, crossing '/'
			}
			for i := 0; i <= len(s); i++ {
				if globHelper(rest, s[i:]) {
					return true
				}
			}
			return false
		}
		if pat[0] == '*' {
			rest := pat[1:]
			// '*' matches any run not containing '/'
			for i := 0; i <= len(s); i++ {
				if i > 0 && s[i-1] == '/' {
					break
				}
				if globHelper(rest, s[i:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 || pat[0] != s[0] {
			return false
		}
		pat, s = pat[1:], s[1:]
	}
	return len(s) == 0
}

// EvaluateScope is the negative-scope hard gate (tsir-spec §5.5): the committed action's
// EffectRecord violates negative_scope iff the forbidden pattern is present.
type ScopeVerdict int

const (
	ScopeOK ScopeVerdict = iota
	ScopeViolation
)

func EvaluateScope(negativeScope *Predicate, committed *EffectRecord) ScopeVerdict {
	if negativeScope == nil {
		return ScopeOK
	}
	if negativeScope.Evaluate(committed) {
		return ScopeViolation
	}
	return ScopeOK
}
