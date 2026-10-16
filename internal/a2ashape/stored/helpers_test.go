package stored_test

import (
	"encoding/json"
	"reflect"
	"testing"
)

// viaSDK decodes b into the a2a-go type sdk points to and encodes it again.
func viaSDK(t *testing.T, b []byte, sdk any) []byte {
	t.Helper()
	if err := json.Unmarshal(b, sdk); err != nil {
		t.Fatalf("a2a-go cannot read %s: %v", b, err)
	}
	out, err := json.Marshal(sdk)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// semanticallySame: decoding and re-encoding through a2a-go leaves the
// projection's JSON meaning the same (keys, values, order-insensitive).
func semanticallySame(t *testing.T, ours []byte, sdk any) {
	t.Helper()
	again := viaSDK(t, ours, sdk)
	var a, b any
	if err := json.Unmarshal(ours, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(again, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("a2a-go changed the meaning:\nours: %s\nsdk:  %s", ours, again)
	}
}
