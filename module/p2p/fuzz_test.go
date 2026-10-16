//go:build !no_p2p

package p2p

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// decodeB64 has one spelling per value: whatever it accepts, encoding the
// result gives the input back, and it accepts nothing outside the base64url
// alphabet.
func FuzzDecodeB64(f *testing.F) {
	for _, s := range []string{"", "AA", "AAA", "AAAA", "ZW52ZWxvcGU", "ZW52ZWxvcGV", "ZW52ZWxvcGU=",
		"+/+/", "-_-_", "ZW52\nZW52", "ZW52\r", " ZW52", "A", "AAAAA", strings.Repeat("Ab-_", 27)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		b, err := decodeB64(s)
		if err != nil {
			return
		}
		if back := encodeB64(b); back != s {
			t.Fatalf("%q decodes to bytes that encode as %q", s, back)
		}
		if strings.ContainsAny(s, "=+/ \t\r\n") {
			t.Fatalf("%q accepted", s)
		}
	})
}

// A send answer, whatever its bytes: a confirmation is returned only from a
// good send answer of this version, exactly DAckLen bytes, in its one
// spelling of 107 characters; a dack member that is not one, "" included, is
// reported bad; a bad frame (another version, a member frame v3 does not
// have) yields an error, no dack, and is reported bad (core CORE-K13a).
func FuzzReadAnswer(f *testing.F) {
	dack := encodeB64(bytes.Repeat([]byte{0xfb}, module.DAckLen))
	for _, l := range []string{
		`{"op":"send","v":3,"id":"1","dack":"` + dack + `"}`,
		`{"op":"send","v":3,"id":"1"}`,
		`{"op":"send","v":3,"id":"1","error":"no peer"}`,
		`{"op":"send","v":2,"id":"1","dack":"` + dack + `"}`,
		`{"op":"error","v":3,"id":"1","dack":"` + dack + `"}`,
		`{"op":"send","v":3,"id":"1","dack":"` + dack + `="}`,
		`{"op":"send","v":3,"id":"1","dack":"` + dack[:106] + `"}`,
		`{"op":"send","v":3,"id":"1","dack":""}`,
		`{"op":"send","v":3,"id":"1","dack":"` + dack + `","outcome":1}`,
		`{"op":"send","v":3,"id":"1","dack":7}`,
		`{"op":"send","v":"3"}`,
		`{"op":"send","v":3}`,
		`[]`,
	} {
		f.Add([]byte(l))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		reply, has, badFrame, err := readFrame(line)
		if err != nil {
			return
		}
		got, bad, aerr := readAnswer(answer{f: reply, has: has, bad: badFrame})
		if got != nil {
			if len(got) != module.DAckLen || badFrame != "" || reply.Op != opSend || encodeB64(got) != reply.DAck || len(reply.DAck) != 107 {
				t.Fatalf("%s: returned dack %x", line, got)
			}
			if bad {
				t.Fatalf("%s: a dack returned and reported bad", line)
			}
		}
		if badFrame != "" && (got != nil || !bad || aerr == nil) {
			t.Fatalf("%s: a bad frame (%s) read as dack=%x bad=%v err=%v", line, badFrame, got, bad, aerr)
		}
		if reply.V != WireVersion && badFrame == "" {
			t.Fatalf("%s: a frame of version %d is not bad", line, reply.V)
		}
		if badFrame == "" && reply.Op == opSend && has["dack"] && got == nil && !bad {
			t.Fatalf("%s: a dack member neither returned nor reported bad", line)
		}
		if badFrame == "" && (reply.Op != opSend || reply.Error != "") && aerr == nil {
			t.Fatalf("%s: a failed answer read as a success", line)
		}
		var members map[string]any
		_ = json.Unmarshal(line, &members)
		for m := range members {
			if badFrame == "" && !map[string]bool{"op": true, "v": true, "id": true, "dack": true, "error": true, "self": true, "envelope": true}[m] {
				t.Fatalf("%s: member %q let through", line, m)
			}
		}
	})
}

// A recv frame, whatever its bytes: the daemon is handed an envelope only
// from a frame of this version with an id, and exactly the bytes its
// envelope member spells in base64url without padding; every other frame
// gets a refusal.
func FuzzCheckRecv(f *testing.F) {
	for _, l := range []string{
		`{"op":"recv","v":3,"id":"d1","envelope":"ZW52ZWxvcGU"}`,
		`{"op":"recv","v":2,"id":"d1","envelope":"ZW52ZWxvcGU"}`,
		`{"op":"recv","id":"d1","envelope":"ZW52ZWxvcGU"}`,
		`{"op":"recv","v":3,"envelope":"ZW52ZWxvcGU"}`,
		`{"op":"recv","v":3,"id":"d1","envelope":"ZW52ZWxvcGU="}`,
		`{"op":"recv","v":3,"id":"d1","envelope":""}`,
		`{"op":"recv","v":3,"id":"d1","envelope":"+/+/"}`,
	} {
		f.Add([]byte(l))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		var in frame
		if json.Unmarshal(line, &in) != nil {
			return
		}
		env, refusal := checkRecv(in)
		if env == nil {
			if refusal == "" {
				t.Fatalf("%s: neither handed on nor refused", line)
			}
			return
		}
		if refusal != "" || in.V != WireVersion || in.ID == "" || len(env) == 0 {
			t.Fatalf("%s: handed on (refusal %q)", line, refusal)
		}
		if want, err := base64.RawURLEncoding.Strict().DecodeString(in.Envelope); err != nil || !bytes.Equal(env, want) ||
			encodeB64(env) != in.Envelope {
			t.Fatalf("%s: handed on bytes %x", line, env)
		}
	})
}
