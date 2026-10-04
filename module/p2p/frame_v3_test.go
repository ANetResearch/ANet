//go:build !no_p2p

package p2p

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/wire"

	"github.com/ANetResearch/ANet/module"
)

// FRAME-3 (core §5.2): the JSON frame of every p2p op of frame v3, in each
// direction it travels, and the frames a v3 endpoint refuses. The golden is
// testdata/frame-v3.json; tools/anetpeer reads the same file with its own
// frame type, so both ends of the wire are held to the same bytes.

var updateFrames = flag.Bool("update-frames", false, "rewrite testdata/frame-v3.json")

const frame3Path = "testdata/frame-v3.json"

type frame3File struct {
	ID      string         `json:"id"`
	About   string         `json:"about"`
	Version int            `json:"version"`
	Seeds   string         `json:"seeds"`
	Values  frame3Values   `json:"values"`
	Frames  []frame3Line   `json:"frames"`
	Refused []frame3Refuse `json:"refused"`
}

type frame3Values struct {
	Self        string `json:"self"`
	To          string `json:"to"`
	Addr        string `json:"addr"`
	EnvelopeHex string `json:"envelope_hex"`
	DAckHex     string `json:"dack_hex"`
}

// frame3Line is one frame as it travels: Op and Dir name its row of the
// registry's p2p frame table (wire/registry.json), Line is its bytes.
type frame3Line struct {
	Op   string `json:"op"`
	Dir  string `json:"dir"`
	Case string `json:"case,omitempty"`
	Line string `json:"line"`
}

// frame3Refuse is a frame an endpoint of v3 refuses, and how.
type frame3Refuse struct {
	Case string `json:"case"`
	// At is who reads it: "daemon" (module/p2p, from its peer process),
	// "anetpeer from its daemon", "anetpeer from a peer".
	At   string `json:"at"`
	Line string `json:"line"`
	// Answer is the frame the reader writes back: always an error frame of
	// v 3, with the frame's id when it had one. Closes: the reader then
	// ends the connection. Nothing refused is handed to a daemon or acked.
	Answer string `json:"answer"`
	Closes bool   `json:"closes"`
}

// seed is SHA-256("anet-golden/v016/FRAME-3/<role>") (core CORE-G5).
func seed(role string) []byte {
	h := sha256.Sum256([]byte("anet-golden/v016/FRAME-3/" + role))
	return h[:]
}

func frame3Envelope() []byte {
	return append(seed("envelope/0"), seed("envelope/1")...)
}

func frame3DAck() []byte {
	d := append(append(seed("dack/0"), seed("dack/1")...), seed("dack/2")[:16]...)
	return d[:module.DAckLen]
}

func frame3Build(t *testing.T) []byte {
	t.Helper()
	const (
		self = "bafyreibkccppym6ec5j7a4gq2gvk5x6w2fdjmifisqeshpmik4q2wj6aei"
		to   = "bafyreiaal7k2ike6wvs46k4ga2s7klovc2hdfrn7xqsnp6quc4jzcr2pse"
		addr = "203.0.113.7:39100"
	)
	env, dack := encodeB64(frame3Envelope()), encodeB64(frame3DAck())
	line := func(f frame) string {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	v := WireVersion
	out := frame3File{
		ID: "FRAME-3",
		About: "p2p frame v3: one JSON object per line. Each frame of the registry's p2p table " +
			"(op, dir) as module/p2p and tools/anetpeer write it, byte for byte; both read every line " +
			"and write it back unchanged. Binary values are base64url without padding (core CORE-E5): " +
			"the envelope here, and the dack (80 bytes, 107 characters), which only the recipient's " +
			"daemon makes and everything between carries as it came. No frame says whether a message was " +
			"processed or refused. The refused frames are answered with an error frame of v 3 (with the " +
			"frame's id when it had one) and are never handed to a daemon or acked; a hello is also " +
			"answered by closing the connection. Written by the Go test (go test ./module/p2p -run FRAME3 " +
			"-update-frames).",
		Version: WireVersion,
		Seeds: "every value below is SHA-256(\"anet-golden/v016/FRAME-3/<role>\"): envelope = " +
			"H(envelope/0) || H(envelope/1), 64 bytes; dack = H(dack/0) || H(dack/1) || H(dack/2)[:16], " +
			"80 bytes. The AIDs are those of KEL-2B; the address is in 203.0.113.0/24 (RFC 5737).",
		Values: frame3Values{Self: self, To: to, Addr: addr,
			EnvelopeHex: hex.EncodeToString(frame3Envelope()), DAckHex: hex.EncodeToString(frame3DAck())},
		Frames: []frame3Line{
			{Op: "hello", Dir: "both", Line: line(frame{Op: opHello, V: v, Self: self})},
			{Op: "send", Dir: "daemon -> local anetpeer", Line: line(frame{Op: opSend, V: v, ID: "7", To: to, Envelope: env, Addr: addr})},
			{Op: "send", Dir: "anetpeer -> remote anetpeer", Line: line(frame{Op: opSend, V: v, ID: "7", To: to, Envelope: env})},
			{Op: "send answer", Dir: "remote anetpeer -> anetpeer; anetpeer -> daemon", Case: "with the recipient's dack",
				Line: line(frame{Op: opSend, V: v, ID: "7", DAck: dack})},
			{Op: "send answer", Dir: "remote anetpeer -> anetpeer; anetpeer -> daemon", Case: "acked without a dack",
				Line: line(frame{Op: opSend, V: v, ID: "7"})},
			{Op: "send answer", Dir: "remote anetpeer -> anetpeer; anetpeer -> daemon", Case: "not taken",
				Line: line(frame{Op: opSend, V: v, ID: "7", Error: "receiving daemon did not take the delivery: temporarily refused"})},
			{Op: "recv", Dir: "anetpeer -> local daemon", Line: line(frame{Op: opRecv, V: v, ID: "d1", Envelope: env})},
			{Op: "ack", Dir: "daemon -> anetpeer", Case: "with a dack", Line: line(frame{Op: opAck, V: v, ID: "d1", DAck: dack})},
			{Op: "ack", Dir: "daemon -> anetpeer", Case: "without a dack", Line: line(frame{Op: opAck, V: v, ID: "d1"})},
			{Op: "nack", Dir: "daemon -> anetpeer", Line: line(frame{Op: opNack, V: v, ID: "d1", Error: "temporarily refused"})},
			{Op: "error", Dir: "either", Case: "about a frame with an id",
				Line: line(frame{Op: opError, V: v, ID: "7", Error: versionRefused(2)})},
			{Op: "error", Dir: "either", Case: "about a hello", Line: line(frame{Op: opError, V: v, Error: versionRefused(2)})},
		},
		Refused: []frame3Refuse{
			{Case: "hello of v 2", At: "daemon", Line: line(frame{Op: opHello, V: 2, Self: to}), Answer: "error", Closes: true},
			{Case: "hello of v 2", At: "anetpeer from its daemon", Line: line(frame{Op: opHello, V: 2, Self: self}), Answer: "error", Closes: true},
			{Case: "hello without v", At: "anetpeer from its daemon", Line: `{"op":"hello","self":"` + self + `"}`, Answer: "error", Closes: true},
			{Case: "recv of v 2", At: "daemon", Line: line(frame{Op: opRecv, V: 2, ID: "d1", Envelope: env}), Answer: "error"},
			{Case: "recv without v", At: "daemon", Line: `{"op":"recv","id":"d1","envelope":"` + env + `"}`, Answer: "error"},
			{Case: "recv with a padded envelope", At: "daemon",
				Line: line(frame{Op: opRecv, V: v, ID: "d1", Envelope: "ZW52ZWxvcGU="}), Answer: "error"},
			{Case: "recv with an envelope in the standard alphabet", At: "daemon",
				Line: line(frame{Op: opRecv, V: v, ID: "d1", Envelope: "+/+/ZW52"}), Answer: "error"},
			{Case: "send of v 2", At: "anetpeer from its daemon", Line: line(frame{Op: opSend, V: 2, ID: "7", To: to, Envelope: env}), Answer: "error"},
			{Case: "send of v 2", At: "anetpeer from a peer", Line: line(frame{Op: opSend, V: 2, ID: "7", To: to, Envelope: env}), Answer: "error"},
			{Case: "send of v 2 with not_delivered", At: "anetpeer from a peer",
				Line: `{"op":"send","v":2,"id":"7","to":"` + to + `","envelope":"` + env + `","not_delivered":true}`, Answer: "error"},
		},
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // "->" in the registry's directions, as written there
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func loadFrame3(t *testing.T) *frame3File {
	t.Helper()
	b, err := os.ReadFile(frame3Path)
	if err != nil {
		t.Fatal(err)
	}
	var f frame3File
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("%s: %v", frame3Path, err)
	}
	return &f
}

// The golden is what the Go encoder writes from its seeds, byte for byte.
// Changing a byte of it is a change of the wire (core CORE-G8).
func TestFRAME3IsWhatTheEncoderWrites(t *testing.T) {
	want := frame3Build(t)
	if *updateFrames {
		if err := os.MkdirAll(filepath.Dir(frame3Path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(frame3Path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(frame3Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from what the encoder writes; a change of the wire needs core, the registry and this file together", frame3Path)
	}
}

// Every line decodes and encodes back to itself; it has v 3; its members
// are those of its row of the registry's frame table (never outcome, reason
// or anything else); every row has a line; the dack is 107 characters and
// decodes to the seeded 80 bytes; the envelope to the seeded envelope.
func TestFRAME3(t *testing.T) {
	g := loadFrame3(t)
	if g.Version != WireVersion {
		t.Fatalf("golden version %d, WireVersion %d", g.Version, WireVersion)
	}
	reg := wire.MustLoad().P2P
	if reg.Version != WireVersion {
		t.Fatalf("the registry says frame version %d, this module speaks %d", reg.Version, WireVersion)
	}
	type rowKey struct{ op, dir string }
	rows := map[rowKey]*wire.Frame{}
	for _, f := range reg.Frames {
		rows[rowKey{f.Op, f.Dir}] = f
	}
	covered := map[rowKey]bool{}
	wantEnv, _ := hex.DecodeString(g.Values.EnvelopeHex)
	wantDAck, _ := hex.DecodeString(g.Values.DAckHex)
	if !bytes.Equal(wantEnv, frame3Envelope()) || !bytes.Equal(wantDAck, frame3DAck()) {
		t.Fatal("the golden's values are not the seeded ones")
	}
	for _, gl := range g.Frames {
		name := gl.Op + " (" + gl.Dir + ") " + gl.Case
		row := rows[rowKey{gl.Op, gl.Dir}]
		if row == nil {
			t.Errorf("%s: no such row in the registry's p2p frame table", name)
			continue
		}
		covered[rowKey{gl.Op, gl.Dir}] = true
		var f frame
		if err := json.Unmarshal([]byte(gl.Line), &f); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if back, _ := json.Marshal(f); string(back) != gl.Line {
			t.Errorf("%s: written back as\n%s\nnot\n%s", name, back, gl.Line)
		}
		if f.V != WireVersion {
			t.Errorf("%s: v = %d", name, f.V)
		}
		wantOp := gl.Op
		if gl.Op == "send answer" {
			wantOp = opSend
		}
		if f.Op != wantOp {
			t.Errorf("%s: op %q", name, f.Op)
		}
		var members map[string]json.RawMessage
		_ = json.Unmarshal([]byte(gl.Line), &members)
		allowed := map[string]bool{"op": true}
		for _, m := range append(append([]string(nil), row.Fields...), row.May...) {
			allowed[m] = true
		}
		for m := range members {
			if !allowed[m] {
				t.Errorf("%s: member %q is not in its registry row %v + %v", name, m, row.Fields, row.May)
			}
		}
		for _, m := range row.Fields {
			if _, ok := members[m]; !ok {
				t.Errorf("%s: lacks member %q of its registry row", name, m)
			}
		}
		if f.DAck != "" {
			d, err := decodeB64(f.DAck)
			if len(f.DAck) != 107 || err != nil || !bytes.Equal(d, wantDAck) {
				t.Errorf("%s: dack of %d characters (%v), want the 107 of the seeded 80 bytes", name, len(f.DAck), err)
			}
		}
		if f.Envelope != "" {
			if e, err := decodeB64(f.Envelope); err != nil || !bytes.Equal(e, wantEnv) {
				t.Errorf("%s: envelope does not decode to the seeded one (%v)", name, err)
			}
		}
	}
	var missing []string
	for k := range rows {
		if !covered[k] {
			missing = append(missing, k.op+" ("+k.dir+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("registry rows without a golden line: %s", strings.Join(missing, "; "))
	}
	for _, r := range reg.Removed {
		for _, gl := range g.Frames {
			for _, word := range strings.Fields(r) {
				if word == "frame" {
					continue
				}
				if strings.Contains(gl.Line, `"`+word+`"`) {
					t.Errorf("%s: carries %q, which v3 removed (%s)", gl.Op, word, r)
				}
			}
		}
	}
}

// The refused frames, read by this module from its peer process: each is
// answered with an error frame of v 3 and nothing reaches the daemon or is
// acked; a hello also ends the session.
func TestFRAME3RefusedByTheDaemon(t *testing.T) {
	g := loadFrame3(t)
	n := 0
	for _, r := range g.Refused {
		if r.At != "daemon" {
			continue
		}
		n++
		t.Run(r.Case, func(t *testing.T) {
			peer := newFakePeer(t)
			rec := &recorder{}
			dialTransport(t, peer, rec)
			var in frame
			if err := json.Unmarshal([]byte(r.Line), &in); err != nil {
				t.Fatal(err)
			}
			peer.pushLine(r.Line)
			waitFor(t, func() bool { _, ok := peer.errorFor(in.ID); return ok }, "an error frame")
			e, _ := peer.errorFor(in.ID)
			if r.Answer != "error" || e.Op != opError || e.V != WireVersion {
				t.Fatalf("answer %+v, want an error frame of v %d", e, WireVersion)
			}
			if r.Closes {
				waitFor(t, func() bool { peer.mu.Lock(); defer peer.mu.Unlock(); return peer.ended > 0 }, "the session to end")
			}
			if rec.count() != 0 || peer.totalAcks() != 0 {
				t.Fatalf("handed to the daemon %d times, acked %d times", rec.count(), peer.totalAcks())
			}
		})
	}
	if n == 0 {
		t.Fatal("the golden refuses nothing at the daemon")
	}
}

// The frames of the golden, played between this module and a peer process:
// the recv line reaches the daemon as the seeded envelope and is acked with
// the ack line of the daemon's dack; the send this module writes, with the
// answer line, yields the seeded dack.
func TestFRAME3RoundTrip(t *testing.T) {
	g := loadFrame3(t)
	find := func(op, cas string) string {
		for _, gl := range g.Frames {
			if gl.Op == op && gl.Case == cas {
				return gl.Line
			}
		}
		t.Fatalf("no %s line %q", op, cas)
		return ""
	}
	peer := newFakePeer(t)
	rec := &recorder{dack: frame3DAck()}
	tr := dialTransport(t, peer, rec)

	peer.pushLine(find("recv", ""))
	waitFor(t, func() bool { return peer.acksFor("d1") == 1 }, "the ack")
	rec.mu.Lock()
	got := rec.got[0]
	rec.mu.Unlock()
	if got != string(frame3Envelope()) {
		t.Fatal("the recv line did not hand the daemon the seeded envelope")
	}
	ackLine, _ := json.Marshal(peer.ackOf("d1"))
	if string(ackLine) != find("ack", "with a dack") {
		t.Fatalf("ack written as\n%s\nnot\n%s", ackLine, find("ack", "with a dack"))
	}

	answer := find("send answer", "with the recipient's dack")
	peer.mu.Lock()
	peer.answer = func(f frame) *frame {
		var a frame
		_ = json.Unmarshal([]byte(answer), &a)
		a.ID = f.ID
		return &a
	}
	peer.mu.Unlock()
	dack, handed, err := tr.Send(t.Context(), g.Values.To, frame3Envelope())
	if err != nil || !handed || !bytes.Equal(dack, frame3DAck()) {
		t.Fatalf("send: dack=%x handed=%v err=%v", dack, handed, err)
	}
	peer.mu.Lock()
	sent := peer.sent[0]
	peer.mu.Unlock()
	sent.ID, sent.Addr = "7", g.Values.Addr // the id this transport minted; the address SS-11 fills in
	if b, _ := json.Marshal(sent); string(b) != find("send", "") || fmt.Sprint(sent.V) != "3" {
		t.Fatalf("send written as\n%s\nnot\n%s", b, find("send", ""))
	}
}
