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
// send answer of this version, exactly DAckLen bytes, in its one spelling;
// a confirmation member that is not one is reported bad; an answer of
// another version yields nothing but an error.
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
		`{"op":"send","v":3,"id":"1","dack":7}`,
		`{"op":"send","v":"3"}`,
		`[]`,
	} {
		f.Add([]byte(l))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		var reply frame
		if json.Unmarshal(line, &reply) != nil {
			return
		}
		got, bad, err := readAnswer(reply)
		if got != nil {
			if len(got) != module.DAckLen || reply.V != WireVersion || reply.Op != opSend || encodeB64(got) != reply.DAck {
				t.Fatalf("%s: returned dack %x", line, got)
			}
			if bad {
				t.Fatalf("%s: a dack returned and reported bad", line)
			}
		}
		if reply.V != WireVersion && (got != nil || bad || err == nil) {
			t.Fatalf("%s: an answer of version %d read as dack=%x bad=%v err=%v", line, reply.V, got, bad, err)
		}
		if reply.V == WireVersion && reply.Op == opSend && reply.DAck != "" && got == nil && !bad {
			t.Fatalf("%s: a dack member neither returned nor reported bad", line)
		}
		if (reply.Op != opSend || reply.Error != "") && reply.V == WireVersion && err == nil {
			t.Fatalf("%s: a failed answer read as a success", line)
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
