package hubapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// FuzzDecodeB64 checks the strict decoder against an independent rule: a
// value is accepted exactly when Go's lenient base64url decoder reads it and
// encoding the result gives the value back — one spelling, nothing skipped
// or ignored — and it has the size asked for (core CORE-E5).
func FuzzDecodeB64(f *testing.F) {
	for _, s := range []string{"", "AA", "AAA", "AAAA", "A", "AB", "-_v7", "-_v7=", "+/", "AA\n", "\rAA", "AA AA",
		hubapi.B64(bytes.Repeat([]byte{0xfb}, 16)), hubapi.B64(bytes.Repeat([]byte{1}, 80))} {
		for _, n := range []int{-1, 0, 1, 16, 80} {
			f.Add(s, n)
		}
	}
	f.Fuzz(func(t *testing.T, s string, n int) {
		b, err := hubapi.DecodeB64(s, n)
		ref, rerr := base64.RawURLEncoding.DecodeString(s)
		want := rerr == nil && base64.RawURLEncoding.EncodeToString(ref) == s && (n < 0 || len(ref) == n)
		if (err == nil) != want {
			t.Fatalf("DecodeB64(%q, %d) = %x, %v; want accepted %v", s, n, b, err, want)
		}
		if err == nil && (!bytes.Equal(b, ref) || hubapi.B64(b) != s) {
			t.Fatalf("DecodeB64(%q) = %x, which does not encode back", s, b)
		}
	})
}

// FuzzHubDeliveryAnswers decodes arbitrary send and status answers as the
// daemon does (unknown members ignored, core CORE-E7): nothing panics, and
// a rid or dack that decodes is of its size and spelled the one way.
func FuzzHubDeliveryAnswers(f *testing.F) {
	d := hubapi.B64(bytes.Repeat([]byte{3}, 80))
	for _, s := range []string{
		`{"status":"queued","rid":"` + hubapi.B64(bytes.Repeat([]byte{5}, 16)) + `","recipient_quiet":false}`,
		`{"status":"queued","rid":"AAAA"}`,
		`{"items":[{"state":"queued"},{"state":"done","dack":"` + d + `"},{"state":"unknown"}]}`,
		`{"items":[{"state":"done","dack":"` + d + `="}]}`,
		`{"items":[{"state":"done"}],"extra":1}`,
		`{"items":null}`, `[]`, `{}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var send hubapi.RelaySendResponse
		if json.Unmarshal(body, &send) == nil {
			if rid, err := send.RIDBytes(); err == nil && hubapi.B64(rid[:]) != send.RID {
				t.Fatalf("rid %q decodes to %x", send.RID, rid)
			}
		}
		var st hubapi.RelayStatusResponse
		if json.Unmarshal(body, &st) == nil {
			for _, it := range st.Items {
				if dk, err := it.DAckBytes(); err == nil && hubapi.B64(dk[:]) != it.DAck {
					t.Fatalf("dack %q decodes to %x", it.DAck, dk)
				}
			}
		}
	})
}
