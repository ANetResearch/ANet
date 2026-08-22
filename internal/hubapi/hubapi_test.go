package hubapi_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// These names are the contract with a hub that lives in another
// repository, built from another checkout, deployed on another machine.
//
// Nothing in either build fails when they drift. The request succeeds,
// the JSON parses, and a field silently arrives as its zero value — which
// is how this daemon shipped reading a balance of "balance" from a hub
// that sends "credits", reporting zero for every funded account and
// passing a full green suite while doing it. A type with no test is not
// the problem; a wire with no test is.
//
// So the field names are pinned the way ANetCore pins its CBOR vectors: a
// rename has to be a deliberate act that updates this list, and a hub
// operator upgrading one side can read here what the other side expects.
func TestTheWireFieldNamesArePinned(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"AgentView", hubapi.AgentView{}, []string{
			"aid", "avg_rating", "caps", "guest_quota", "home_hub", "listed",
			"name", "pricing", "readme", "registered_at", "review_count", "summary",
		}},
		{"ReviewView", hubapi.ReviewView{}, []string{
			"comment", "completed_at", "created_at", "deliverable", "goal",
			"interaction_id", "rating", "receipt_cid", "request_cid",
			"result_cid", "reviewer_aid", "subject_aid",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jsonNames(t, tc.value)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("wire fields changed\n got  %v\n want %v\n"+
					"if this is deliberate, the hub must change with it — "+
					"a rename on one side is a field that silently arrives empty on the other",
					got, tc.want)
			}
		})
	}
}

// jsonNames is what the type actually puts on the wire, sorted.
func jsonNames(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	// omitempty hides the empty ones, so marshal a populated copy too and
	// take the union: a field that is only ever present when set is still
	// part of the contract.
	filled := fillStrings(reflect.New(reflect.TypeOf(v)).Elem())
	b2, err := json.Marshal(filled.Interface())
	if err != nil {
		t.Fatal(err)
	}
	var obj2 map[string]any
	if err := json.Unmarshal(b2, &obj2); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for k := range obj {
		seen[k] = true
	}
	for k := range obj2 {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fillStrings puts a non-zero value in every field so omitempty cannot
// hide one from the contract.
func fillStrings(v reflect.Value) reflect.Value {
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Int, reflect.Int64:
			f.SetInt(1)
		case reflect.Uint, reflect.Uint64:
			f.SetUint(1)
		case reflect.Float64:
			f.SetFloat(1)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		}
	}
	return v
}

// The relay wire kinds are strings both sides switch on. A typo on one
// side routes a delegation to a handler that does not exist, and the
// message is acked and gone.
func TestTheRelayKindsArePinned(t *testing.T) {
	for name, got := range map[string]string{
		"delegate": hubapi.RelayKindDelegate,
		"message":  hubapi.RelayKindMessage,
		"result":   hubapi.RelayKindResult,
	} {
		if got != name {
			t.Errorf("relay kind = %q, want %q — the hub switches on this string", got, name)
		}
	}
}
