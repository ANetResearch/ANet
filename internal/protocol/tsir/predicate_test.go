package tsir

import "testing"

func u64(v uint64) *uint64 { return &v }

// §3.3d red/green: an acceptance ArtifactClause is true iff an artifact matches path-glob
// and meets min_size_bytes; one byte below the floor is false.
func TestArtifactRedGreen(t *testing.T) {
	clause := &Predicate{Op: OpArtifact, Artifact: &ArtifactClause{
		PathGlob: "build/*.bin", MinSizeBytes: u64(1024),
	}}
	if err := clause.Validate(); err != nil {
		t.Fatal(err)
	}
	green := &EffectRecord{Artifacts: []Artifact{{Path: "build/out.bin", SizeBytes: 1024}}}
	if !clause.Evaluate(green) {
		t.Fatal("green: artifact at floor should satisfy")
	}
	red := &EffectRecord{Artifacts: []Artifact{{Path: "build/out.bin", SizeBytes: 1023}}}
	if clause.Evaluate(red) {
		t.Fatal("red: artifact one byte below floor should NOT satisfy")
	}
	// path-glob '*' does not cross '/'.
	nested := &EffectRecord{Artifacts: []Artifact{{Path: "build/sub/out.bin", SizeBytes: 4096}}}
	if clause.Evaluate(nested) {
		t.Fatal("'*' must not cross '/'")
	}
}

// Negative action-scope hard gate: a delete over secrets/** is a SCOPE_VIOLATION.
func TestEvaluateScope(t *testing.T) {
	neg := &Predicate{Op: OpScope, Scope: &ScopeClause{
		Verb: VerbDelete, Kind: 1, Match: ResourceMatch{Kind: MatchGlob, Val: "secrets/**"},
	}}
	if err := neg.Validate(); err != nil {
		t.Fatal(err)
	}
	violating := &EffectRecord{Effects: []EffectRef{
		{Verb: VerbDelete, Resource: ResourceRef{Kind: 1, ID: "secrets/api/key.pem"}},
	}}
	if EvaluateScope(neg, violating) != ScopeViolation {
		t.Fatal("delete under secrets/** must be SCOPE_VIOLATION")
	}
	clean := &EffectRecord{Effects: []EffectRef{
		{Verb: VerbDelete, Resource: ResourceRef{Kind: 1, ID: "tmp/cache"}},
	}}
	if EvaluateScope(neg, clean) != ScopeOK {
		t.Fatal("delete outside secrets/** must be OK")
	}
	// a read (get) under secrets/** is not a delete → OK (verb-specific).
	readOnly := &EffectRecord{Effects: []EffectRef{
		{Verb: VerbGet, Resource: ResourceRef{Kind: 1, ID: "secrets/api/key.pem"}},
	}}
	if EvaluateScope(neg, readOnly) != ScopeOK {
		t.Fatal("get under secrets/** is not the forbidden delete → OK")
	}
}

// Threshold + test + boolean composition.
func TestComposite(t *testing.T) {
	p := &Predicate{Op: OpAND, Children: []*Predicate{
		{Op: OpTest, Test: &TestClause{TestID: "unit", Expect: StatusPass}},
		{Op: OpThreshold, Thresh: &ThresholdClause{Metric: "coverage", Op: CmpGE, Value: 0.8}},
		{Op: OpNOT, Children: []*Predicate{
			{Op: OpThreshold, Thresh: &ThresholdClause{Metric: "errors", Op: CmpGT, Value: 0}},
		}},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	good := &EffectRecord{
		Tests:   []TestResult{{ID: "unit", Status: StatusPass}},
		Metrics: map[string]float64{"coverage": 0.85, "errors": 0},
	}
	if !p.Evaluate(good) {
		t.Fatal("good run should satisfy")
	}
	bad := &EffectRecord{
		Tests:   []TestResult{{ID: "unit", Status: StatusPass}},
		Metrics: map[string]float64{"coverage": 0.5, "errors": 0},
	}
	if p.Evaluate(bad) {
		t.Fatal("low coverage should fail")
	}
}

// Malformed predicates fail closed (unknown op, wrong arity, bad enum).
func TestMalformed(t *testing.T) {
	cases := []*Predicate{
		{Op: 99},                   // unknown op
		{Op: OpNOT, Children: nil}, // NOT needs one child
		{Op: OpAND, Children: []*Predicate{{Op: OpTest, Test: &TestClause{TestID: "x", Expect: StatusPass}}}}, // AND needs ≥2
		{Op: OpThreshold, Thresh: &ThresholdClause{Metric: "m", Op: 9, Value: 1}},                             // bad cmp op
		{Op: OpScope, Scope: &ScopeClause{Verb: 7, Kind: 1, Match: ResourceMatch{Kind: 1}}},                   // bad verb
		// schema_ref / contains[] need resolved CAS content the EffectRecord lacks → fail-loud (§3.4).
		{Op: OpArtifact, Artifact: &ArtifactClause{PathGlob: "doc/*.json", SchemaRef: "cid:abc"}},
		{Op: OpArtifact, Artifact: &ArtifactClause{PathGlob: "doc/*.txt", Contains: []string{"TODO"}}},
	}
	for i, p := range cases {
		if err := p.Validate(); err != ErrMalformed {
			t.Errorf("case %d: want ErrMalformed, got %v", i, err)
		}
	}
	// over-depth fails closed.
	deep := &Predicate{Op: OpNOT, Children: []*Predicate{{Op: OpTest, Test: &TestClause{TestID: "x", Expect: StatusPass}}}}
	for i := 0; i < MaxPredicateDepth+2; i++ {
		deep = &Predicate{Op: OpNOT, Children: []*Predicate{deep}}
	}
	if err := deep.Validate(); err != ErrMalformed {
		t.Errorf("over-depth: want ErrMalformed, got %v", err)
	}
}
