package anetlink

// F-C1: the C1 v1 decoders under fuzzing. Two things are checked for every input:
//
//   - every entrance (decodeInvokeReq, decodeInvokeResp, decodeCapList, decodeDescribe,
//     decodeRefusal) never panics, and what it accepts satisfies C1's rules: an invoke request with
//     a capability and a caller whose via is 0 without an aid or 1 with one, which encodes again to a
//     request the entrance takes back unchanged;
//   - ANetLink's decoder and this one agree. ANetLink is built on an older ANetCore, without
//     coredet.UnmarshalStrict, and decodes with its own mode (c1DecMode) made of the options written
//     down for it: tags forbidden, duplicate map keys refused, unknown fields refused. This target
//     builds that mode from the same options and holds coredet.UnmarshalStrict to it, object by
//     object: one accepting what the other refuses, or reading it differently, is a C1 that means
//     two things.

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/fxamacker/cbor/v2"

	"github.com/ANetResearch/ANetCore/coredet"
)

// anetlinkDecOptions are ANetLink's C1 decoding options, as written for its c1DecMode.
func anetlinkDecOptions() cbor.DecOptions {
	return cbor.DecOptions{
		TagsMd:            cbor.TagsForbidden,
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
	}
}

var anetlinkDecMode = func() cbor.DecMode {
	m, err := anetlinkDecOptions().DecMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// c1Objects are a fresh value of each C1 object.
func c1Objects() map[string]func() any {
	return map[string]func() any{
		"invokereq":  func() any { return new(invokeReq) },
		"invokeresp": func() any { return new(invokeResp) },
		"caplist":    func() any { return new(capList) },
		"describe":   func() any { return new(describe) },
		"refusal":    func() any { return new(refusal) },
		"caller":     func() any { return new(caller) },
	}
}

// sameDecoding reports whether coredet.UnmarshalStrict and ANetLink's mode agree on b for every C1
// object: both refuse it, or both take it and read the same value.
func sameDecoding(t testing.TB, b []byte) {
	t.Helper()
	for name, fresh := range c1Objects() {
		ours, theirs := fresh(), fresh()
		errOurs := coredet.UnmarshalStrict(b, ours)
		errTheirs := anetlinkDecMode.Unmarshal(b, theirs)
		if (errOurs == nil) != (errTheirs == nil) {
			t.Fatalf("%s: coredet.UnmarshalStrict says %v, ANetLink's mode says %v, for %x", name, errOurs, errTheirs, b)
		}
		if errOurs == nil && !sameValue(reflect.ValueOf(ours), reflect.ValueOf(theirs)) {
			t.Fatalf("%s: the two read %x differently:\n  %#v\n  %#v", name, b, ours, theirs)
		}
	}
}

// sameValue is reflect.DeepEqual with NaN equal to NaN (a decoded float may be one).
func sameValue(a, b reflect.Value) bool {
	if a.IsValid() != b.IsValid() {
		return false
	}
	if !a.IsValid() {
		return true
	}
	if a.Type() != b.Type() {
		return false
	}
	switch a.Kind() {
	case reflect.Float32, reflect.Float64:
		x, y := a.Float(), b.Float()
		return x == y || math.IsNaN(x) && math.IsNaN(y)
	case reflect.Pointer, reflect.Interface:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return sameValue(a.Elem(), b.Elem())
	case reflect.Slice:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !sameValue(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.IsNil() != b.IsNil() || a.Len() != b.Len() {
			return false
		}
		for _, k := range a.MapKeys() {
			bv := b.MapIndex(k)
			if !bv.IsValid() || !sameValue(a.MapIndex(k), bv) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := range a.NumField() {
			if !sameValue(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

// checkEntrances runs every entrance on b and checks what each accepts.
func checkEntrances(t testing.TB, b []byte) {
	t.Helper()
	if req, err := decodeInvokeReq(b); err == nil {
		c := req.Caller
		switch {
		case req.Capability == nil || c == nil || c.Via == nil:
			t.Fatalf("accepted a request without capability, caller or via: %x", b)
		case *c.Via == viaLocal && c.AID != nil, *c.Via == viaRelay && (c.AID == nil || *c.AID == ""),
			*c.Via != viaLocal && *c.Via != viaRelay:
			t.Fatalf("accepted a request whose via and aid break C1's rules: %x", b)
		}
		// What was accepted encodes, unless it holds a value CoreDet will not write (a NaN in args, a
		// non-shortest form read back), and what encodes is taken back unchanged.
		if again, err := coredet.Marshal(req); err == nil {
			back, err := decodeInvokeReq(again)
			if err != nil || !sameValue(reflect.ValueOf(back), reflect.ValueOf(req)) {
				t.Fatalf("an accepted request does not survive its own encoding: %x -> %x: %v", b, again, err)
			}
		}
	}
	if r, err := decodeInvokeResp(b); err == nil && r.Status == "" {
		t.Fatalf("accepted an invoke answer without a status: %x", b)
	}
	if l, err := decodeCapList(b); err == nil {
		for _, c := range l.Caps {
			if c.ID == "" || c.Remote == nil {
				t.Fatalf("accepted a catalogue entry without id or remote flag: %x", b)
			}
		}
	}
	if d, err := decodeDescribe(b); err == nil && d.CID == nil {
		t.Fatalf("accepted a description without cid: %x", b)
	}
	if r, err := decodeRefusal(b); err == nil && r.Reason == "" {
		t.Fatalf("accepted a refusal without a reason: %x", b)
	}
}

func goldenSeeds(t testing.TB) [][]byte {
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.cbor"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no C1-1 vectors to seed with: %v", err)
	}
	var out [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// The two decoders agree on every C1-1 vector, positive and negative, and each entrance keeps its
// rules on them: the fuzz target's checks, run on its seeds in every test run.
func TestTheStrictDecoderIsANetLinksDecoder(t *testing.T) {
	for _, b := range goldenSeeds(t) {
		sameDecoding(t, b)
		checkEntrances(t, b)
	}
	// The options themselves, one by one: a mode that dropped any of the three would still agree on
	// most inputs.
	o := anetlinkDecOptions()
	if o.TagsMd != cbor.TagsForbidden || o.DupMapKey != cbor.DupMapKeyEnforcedAPF || o.ExtraReturnErrors != cbor.ExtraDecErrorUnknownField {
		t.Fatalf("ANetLink's options as written here: %+v", o)
	}
	for name, b := range map[string][]byte{
		"tag":           {0xa1, 0x01, 0xc0, 0x60},
		"duplicate key": {0xa2, 0x01, 0x60, 0x01, 0x60},
		"unknown key":   {0xa2, 0x01, 0x60, 0x09, 0x00},
	} {
		if coredet.UnmarshalStrict(b, new(refusal)) == nil || anetlinkDecMode.Unmarshal(b, new(refusal)) == nil {
			t.Errorf("%s: a refusal decoder took %x", name, b)
		}
	}
}

func FuzzC1Decode(f *testing.F) {
	for _, b := range goldenSeeds(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		sameDecoding(t, b)
		checkEntrances(t, b)
	})
}
