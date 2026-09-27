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
			"aid", "avg_rating", "caps", "home_hub", "listed",
			"name", "pricing", "readme", "registered_at", "review_count", "summary",
		}},
		// No goal, no deliverable: the hub holds no task content (A2A-DESIGN
		// §9). Mirrors ANetHub internal/aghub/wirecontract_test.go.
		{"ReviewView", hubapi.ReviewView{}, []string{
			"comment", "completed_at", "content_binding", "created_at",
			"interaction_id", "rating", "receipt_cid", "request_cid",
			"result_cid", "reviewer_aid", "subject_aid",
		}},
		{"UploadReviewRequest", hubapi.UploadReviewRequest{}, []string{"receipt", "review"}},
		// Relay v2 (A2A-DESIGN §3.7). No sender, kind or interaction id
		// appears in any of these: the hub routes by to_aid alone and the
		// rest is inside the sealed envelope.
		{"RelaySendRequest", hubapi.RelaySendRequest{}, []string{"envelope", "to_aid"}},
		{"RelaySendResponse", hubapi.RelaySendResponse{}, []string{"id", "recipient_quiet", "status", "via_hub", "warning"}},
		{"RelayPollRequest", hubapi.RelayPollRequest{}, []string{"after_id", "limit"}},
		{"RelayPollResponse", hubapi.RelayPollResponse{}, []string{"messages"}},
		{"RelayMessage", hubapi.RelayMessage{}, []string{"envelope", "id"}},
		{"RelayAckRequest", hubapi.RelayAckRequest{}, []string{"ids"}},
		{"KeysResponse", hubapi.KeysResponse{}, []string{"aid", "kel", "keyset"}},
		{"KeysPublishRequest", hubapi.KeysPublishRequest{}, []string{"keyset"}},
		{"KeysPublishResponse", hubapi.KeysPublishResponse{}, []string{"aid", "keys_status"}},
		{"RegisterRequest", hubapi.RegisterRequest{}, []string{
			"aid", "caps", "card", "enc_keys", "invite", "kel", "name",
		}},
		{"RegisterResponse", hubapi.RegisterResponse{}, []string{
			"aid", "card_error", "card_status", "keys_error", "keys_status", "status",
		}},
		{"HubIdentity", hubapi.HubIdentity{}, []string{"aid", "kel"}},
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
			if f.Type() == reflect.TypeOf(json.RawMessage(nil)) {
				// A raw JSON field needs a valid JSON value, not a zero byte.
				f.Set(reflect.ValueOf(json.RawMessage(`{}`)))
				continue
			}
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		}
	}
	return v
}

// keys_status is reported by the hub in another repository and read by
// the daemon to decide whether to publish its key set again; a value
// spelled differently on one side reads as a refusal.
func TestTheKeysStatusValuesArePinned(t *testing.T) {
	for want, got := range map[string]string{
		"ok": hubapi.KeysStatusOK, "unchanged": hubapi.KeysStatusUnchanged, "absent": hubapi.KeysStatusAbsent,
		"invalid": hubapi.KeysStatusInvalid, "conflict": hubapi.KeysStatusConflict,
	} {
		if got != want {
			t.Errorf("keys_status %q, want %q — the hub reports these exact strings", got, want)
		}
	}
}

// The wire version and the relay v2 authentication header names are
// strings the hub in another repository matches exactly. A daemon that
// states version 1, or signs under a header the hub does not read, is
// refused on every signed call.
func TestTheWireVersionAndAuthHeadersArePinned(t *testing.T) {
	if hubapi.WireVersion != 2 {
		t.Errorf("WireVersion = %d, want 2 (sealed envelopes, relayauth v2)", hubapi.WireVersion)
	}
	for want, got := range map[string]string{
		"X-ANet-Wire": hubapi.WireVersionHeader,
		"X-ANet-AID":  hubapi.HeaderAID,
		"X-ANet-TS":   hubapi.HeaderTS,
		"X-ANet-Seq":  hubapi.HeaderSeq,
		"X-ANet-Sig":  hubapi.HeaderSig,
	} {
		if got != want {
			t.Errorf("header = %q, want %q", got, want)
		}
	}
}
