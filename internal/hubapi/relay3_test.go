package hubapi_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// TestASessionAnswerIsReadStrictly (core §2.12.2, CORE-E5): an answer to
// POST /relay/session gives its id, its end and the sealed key in their one
// spelling and size; any member missing, padded, of the standard alphabet
// or of another size, and an end of 0, is no session.
func TestASessionAnswerIsReadStrictly(t *testing.T) {
	sid := bytes.Repeat([]byte{0xfb}, relayauth.SessionIDLen)
	enc := bytes.Repeat([]byte{0xfe}, hubapi.SessionEncLen)
	ct := bytes.Repeat([]byte{0xff}, hubapi.SessionCTLen)
	good := hubapi.SessionResponse{SID: hubapi.B64(sid), Exp: 1_700_000_000_000, Enc: hubapi.B64(enc), CT: hubapi.B64(ct)}
	gotSID, exp, gotEnc, gotCT, err := good.Decode()
	if err != nil || !bytes.Equal(gotSID, sid) || exp != good.Exp || !bytes.Equal(gotEnc, enc) || !bytes.Equal(gotCT, ct) {
		t.Fatalf("a well-formed answer: %v", err)
	}
	for name, bad := range map[string]func(*hubapi.SessionResponse){
		"no exp":           func(r *hubapi.SessionResponse) { r.Exp = 0 },
		"no sid":           func(r *hubapi.SessionResponse) { r.SID = "" },
		"short sid":        func(r *hubapi.SessionResponse) { r.SID = hubapi.B64(sid[1:]) },
		"padded sid":       func(r *hubapi.SessionResponse) { r.SID += "==" },
		"standard enc":     func(r *hubapi.SessionResponse) { r.Enc = "+" + r.Enc[1:] },
		"enc with a break": func(r *hubapi.SessionResponse) { r.Enc = r.Enc[:8] + "\n" + r.Enc[8:] },
		"long ct":          func(r *hubapi.SessionResponse) { r.CT = hubapi.B64(append(ct, 0)) },
		"no ct":            func(r *hubapi.SessionResponse) { r.CT = "" },
	} {
		r := good
		bad(&r)
		if _, _, _, _, err := r.Decode(); !errors.Is(err, hubapi.ErrBadSession) {
			t.Errorf("%s: %v, want ErrBadSession", name, err)
		}
	}
}

// TestARefusalIsReadByItsCode (core G1, sealed-sender §2.11): the reason is
// taken only in the shape of a code; a body that is no refusal gives none.
func TestARefusalIsReadByItsCode(t *testing.T) {
	for body, want := range map[string][2]string{
		`{"reason":"session_invalid","error":"Open a new session."}`:  {"session_invalid", "Open a new session."},
		`{"reason":"Session Invalid","error":"x"}`:                    {"", "x"},
		`{"reason":"a_code_far_longer_than_thirty_chars","error":""}`: {"", ""},
		`{"error":"an older hub's sentence"}`:                         {"", "an older hub's sentence"},
		`<html>bad gateway</html>`:                                    {"", ""},
	} {
		reason, sentence := hubapi.ParseErrorBody([]byte(body))
		if reason != want[0] || sentence != want[1] {
			t.Errorf("%s: (%q, %q), want (%q, %q)", body, reason, sentence, want[0], want[1])
		}
	}
}
