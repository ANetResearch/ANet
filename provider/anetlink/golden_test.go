package anetlink

// C1-1 (the C1 v1 golden vectors): testdata/c1v1 holds the same bytes as ANetLink's
// northbound/c1serv/testdata/c1v1, copied from the ANetLink release named in ANETLINK_REF and
// checked against its SHA256SUMS. The two programs are built from different ANetCore versions, so
// each repository proves its own side:
//
//  1. its encoder (here ANetCore v0.16's coredet.Marshal) turns the fixed Go value of each positive
//     vector into exactly the file's bytes;
//  2. its strict decoder (here coredet.UnmarshalStrict, followed by C1's own rules in the same
//     entrance function) turns each positive file back into that value, and refuses every negative
//     one.
//
// If an encoder changes, (1) goes red. The answer is never to rewrite the files: they are the
// contract between the two programs, and a deliberate change of it is a wire change made in both
// repositories at once. CI's c1-golden-xrepo job, and TestC1GoldenIsANetLinks here when an ANetLink
// checkout is at hand, compare the two directories file by file.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/effect"
)

const goldenDir = "testdata/c1v1"

// Public, label-derived values: nothing here is a real identity. They are ANetLink's.
var (
	goldenAID = anetcid.MustSum([]byte("anet-golden/v016/C1-1/caller"))
	goldenCID = anetcid.MustSum([]byte("anet-golden/v016/C1-1/catalog"))
)

func sp(s string) *string { return &s }
func up(n uint64) *uint64 { return &n }

type c1Vector struct {
	file  string
	kind  string // invokereq, invokeresp, caplist, describe, refusal
	value any    // a positive vector's Go value; nil for a negative one
}

func goldenPositives() []c1Vector {
	return []c1Vector{
		{"caplist.cbor", "caplist", capList{Caps: []capEntry{
			{ID: lampCap, Remote: up(1)},
			{ID: "sensor.temperature@modbus/plc-1", Remote: up(0)},
		}}},
		{"caplist-empty.cbor", "caplist", capList{Caps: []capEntry{}}},
		{"describe-empty.cbor", "describe", describe{CID: sp("")}},
		{"describe-cid.cbor", "describe", describe{CID: sp(goldenCID)}},
		{"invokereq-local.cbor", "invokereq", invokeReq{
			Capability: sp(lampCap), Args: map[string]any{"on": true}, CallID: sp("call-1"),
			Caller: &caller{Via: up(0)},
		}},
		{"invokereq-relay.cbor", "invokereq", invokeReq{
			Capability: sp(lampCap),
			Args: map[string]any{
				"on": true, "level": uint64(40), "offset": int64(-3), "ratio": 0.5,
				"label": "前门", "rgb": []any{uint64(255), uint64(128), uint64(0)},
				"opts": map[any]any{"fade_ms": uint64(300)},
			},
			CallID: sp("call-2"),
			Caller: &caller{Via: up(1), AID: sp(goldenAID)},
		}},
		{"invokereq-minimal.cbor", "invokereq", invokeReq{Capability: sp(lampCap), Caller: &caller{Via: up(0)}}},
		{"invokeresp-full.cbor", "invokeresp", invokeResp{
			Status:  "OK",
			Metrics: map[string]float64{"power_state": 1, "temperature": 22.13},
			Message: "done",
			Evidence: &effect.Evidence{
				Requested: "on=true", Protocol: "zigbee", NativeAck: true, ObservedState: "on",
				LatencyMS: 12, VerifyTrust: 4, AuthTrust: 2, Quirk: "aqara.temp.scale100",
			},
		}},
		{"invokeresp-unavailable.cbor", "invokeresp", invokeResp{Status: "UNAVAILABLE", Message: "device offline"}},
		{"refusal-not_permitted.cbor", "refusal", refusal{Reason: "not_permitted"}},
		{"refusal-bad_request.cbor", "refusal", refusal{Reason: "bad_request"}},
		{"refusal-too_large.cbor", "refusal", refusal{Reason: "too_large"}},
		{"refusal-unsupported_media.cbor", "refusal", refusal{Reason: "unsupported_media"}},
		{"refusal-unknown_capability.cbor", "refusal", refusal{Reason: "unknown_capability"}},
		{"refusal-c1_version.cbor", "refusal", refusal{Reason: "c1_version"}},
		{"refusal-not_found.cbor", "refusal", refusal{Reason: "not_found"}},
		{"refusal-internal.cbor", "refusal", refusal{Reason: "internal"}},
	}
}

// goldenNegatives are C1-1-neg, by file, with why each is refused. Eight of them are well-formed
// CBOR that only C1's own rules refuse (via outside {0, 1}, via and aid that do not go together, a
// required member missing): "refused" means refused by the whole entrance, not by the decoder alone.
func goldenNegatives() map[string]string {
	return map[string]string{
		"neg-invokereq-via2.cbor":                  "via 2 (the retired voucher door)",
		"neg-invokereq-via3.cbor":                  "via outside {0, 1}",
		"neg-invokereq-local-with-aid.cbor":        "via 0 carrying an aid",
		"neg-invokereq-relay-no-aid.cbor":          "via 1 without an aid",
		"neg-invokereq-relay-empty-aid.cbor":       "via 1 with an empty aid",
		"neg-invokereq-key4.cbor":                  "retired key 4 (C1 v0's caller_aid)",
		"neg-invokereq-caller-key3.cbor":           "reserved Caller key 3 (witnessed)",
		"neg-invokereq-unknown-key.cbor":           "unregistered key 6",
		"neg-invokereq-dup-key.cbor":               "key 1 twice",
		"neg-invokereq-tag.cbor":                   "a CBOR tag around the capability",
		"neg-invokereq-utf8.cbor":                  "text that is not valid UTF-8",
		"neg-invokereq-json.cbor":                  "a JSON body",
		"neg-invokereq-no-caller.cbor":             "required key 5 (caller) missing",
		"neg-invokereq-no-capability.cbor":         "required key 1 (capability) missing",
		"neg-invokereq-caller-no-via.cbor":         "required Caller key 1 (via) missing",
		"neg-invokereq-text-keys.cbor":             "text keys instead of integer keys",
		"neg-invokereq-args-not-map.cbor":          "args that is not a map",
		"neg-invokereq-trailing.cbor":              "bytes after the object",
		"neg-invokeresp-utf8.cbor":                 "a message that is not valid UTF-8",
		"neg-invokeresp-unknown-key.cbor":          "unregistered key 5",
		"neg-invokeresp-evidence-unknown-key.cbor": "unregistered Evidence key 9",
		"neg-caplist-unknown-key.cbor":             "unregistered CapEntry key 3",
	}
}

// kindOf is the object a golden file holds, read from its name (neg-<kind>-…, <kind>-…).
func kindOf(file string) string {
	k, _, _ := strings.Cut(strings.TrimPrefix(file, "neg-"), "-")
	return strings.TrimSuffix(k, ".cbor")
}

// decodeAs is this client's whole entrance for one kind of object.
func decodeAs(kind string, b []byte) (any, error) {
	switch kind {
	case "invokereq":
		return decodeInvokeReq(b)
	case "invokeresp":
		return decodeInvokeResp(b)
	case "caplist":
		return decodeCapList(b)
	case "describe":
		return decodeDescribe(b)
	case "refusal":
		r, err := decodeRefusal(b)
		if err == nil && c1Status[r.Reason] == 0 {
			return nil, fmt.Errorf("reason %q is not a C1 v1 reason", r.Reason)
		}
		return r, err
	}
	panic("unknown kind " + kind)
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("C1-1 %s: %v", name, err)
	}
	return b
}

// LEC §2.1.3, items 1 and 2, for every positive vector.
func TestC1GoldenPositive(t *testing.T) {
	for _, v := range goldenPositives() {
		want := readGolden(t, v.file)
		got, err := coredet.Marshal(v.value)
		if err != nil {
			t.Errorf("%s: encode: %v", v.file, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: ANetCore v0.16 coredet.Marshal gives\n  %x\nthe golden file holds\n  %x\n"+
				"(an encoder change: fix the encoder, never the file)", v.file, got, want)
			continue
		}
		dec, err := decodeAs(v.kind, want)
		if err != nil {
			t.Errorf("%s: strict decode: %v", v.file, err)
			continue
		}
		if !reflect.DeepEqual(dec, v.value) {
			t.Errorf("%s: decodes to\n  %#v\nwant\n  %#v", v.file, dec, v.value)
		}
	}
}

// C1-1-neg: every negative vector is refused by the whole entrance for its object.
func TestC1GoldenNegative(t *testing.T) {
	for file, why := range goldenNegatives() {
		if _, err := decodeAs(kindOf(file), readGolden(t, file)); err == nil {
			t.Errorf("%s: accepted, but must be refused (%s)", file, why)
		} else {
			t.Logf("%s (%s): %v", file, why, err)
		}
	}
}

// The request entrance is the one this client sends through: each positive request, encoded from
// its value as the client would (encodeInvokeReq), is the golden file.
func TestTheClientsRequestsAreTheGoldenBytes(t *testing.T) {
	for _, v := range goldenPositives() {
		if v.kind != "invokereq" {
			continue
		}
		b, err := encodeInvokeReq(v.value.(invokeReq))
		if err != nil || !bytes.Equal(b, readGolden(t, v.file)) {
			t.Errorf("%s: encodeInvokeReq gives %x, %v", v.file, b, err)
		}
	}
	// And what it refuses to send, it refuses before sending.
	via2 := invokeReq{Capability: sp(lampCap), Caller: &caller{Via: up(2)}}
	if _, err := encodeInvokeReq(via2); err == nil {
		t.Fatal("encodeInvokeReq let via 2 out")
	}
}

func sums(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var out bytes.Buffer
	for _, n := range names {
		h := sha256.Sum256(files[n])
		fmt.Fprintf(&out, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	return out.Bytes()
}

// The directory holds exactly the vector table, SHA256SUMS describes it, and ANETLINK_REF names the
// ANetLink release the files came from.
func TestC1GoldenSums(t *testing.T) {
	onDisk, err := filepath.Glob(filepath.Join(goldenDir, "*.cbor"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, p := range onDisk {
		files[filepath.Base(p)] = readGolden(t, filepath.Base(p))
	}
	table := map[string]bool{}
	for _, v := range goldenPositives() {
		table[v.file] = true
	}
	for f := range goldenNegatives() {
		table[f] = true
	}
	for name := range table {
		if _, ok := files[name]; !ok {
			t.Errorf("vector %s has no file", name)
		}
	}
	for name := range files {
		if !table[name] {
			t.Errorf("%s is in %s but not in the vector table", name, goldenDir)
		}
	}
	if got, want := readGolden(t, "SHA256SUMS"), sums(files); !bytes.Equal(got, want) {
		t.Errorf("SHA256SUMS does not describe the files:\n%s\nwant\n%s", got, want)
	}
	ref := strings.TrimSpace(string(readGolden(t, "ANETLINK_REF")))
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`).MatchString(ref) || strings.Contains(ref, "..") ||
		string(readGolden(t, "ANETLINK_REF")) != ref+"\n" {
		t.Errorf("ANETLINK_REF %q is not one git ref on one line", ref)
	}
}

// The ANetLink checkout this test compares with: ANETLINK_DIR, or one next to this repository (as
// the integration workspace has it), or none.
func anetlinkDir(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("ANETLINK_DIR"); d != "" {
		return d
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(root, "ANetLink"), filepath.Join(root, "Refs", "ANetLink")} {
		if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil && strings.Contains(string(b), "module github.com/ANetResearch/ANetLink") {
			return d
		}
	}
	return ""
}

// C1-1, across the two repositories (CI c1-golden-xrepo): every file of ANetLink's copy is here, byte
// for byte, and nothing else. CI checks ANetLink out at ANETLINK_REF and sets ANETLINK_DIR; a run
// without an ANetLink checkout skips, saying so, and CI treats that skip as a failure.
func TestC1GoldenIsANetLinks(t *testing.T) {
	dir := anetlinkDir(t)
	if dir == "" {
		t.Skip("no ANetLink checkout: set ANETLINK_DIR, or put ANetLink next to this repository")
	}
	theirs, err := filepath.Glob(filepath.Join(dir, "northbound", "c1serv", goldenDir, "*"))
	if err != nil || len(theirs) == 0 {
		t.Fatalf("no golden files in %s: %v", dir, err)
	}
	ours, _ := filepath.Glob(filepath.Join(goldenDir, "*"))
	mine := map[string]bool{}
	for _, p := range ours {
		if filepath.Base(p) != "ANETLINK_REF" {
			mine[filepath.Base(p)] = true
		}
	}
	for _, p := range theirs {
		name := filepath.Base(p)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, readGolden(t, name)) {
			t.Errorf("%s differs from ANetLink's (sha256 %x here, %x there)", name, sha256.Sum256(readGolden(t, name)), sha256.Sum256(b))
		}
		delete(mine, name)
	}
	for name := range mine {
		t.Errorf("%s is here but not in ANetLink's copy", name)
	}
}
