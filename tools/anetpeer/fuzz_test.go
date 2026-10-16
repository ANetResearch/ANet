package main

import (
	"bufio"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// A frame from another peer, whatever its bytes, with no daemon attached:
// the answer is of this version and echoes the frame's id; a frame of
// another version gets the version error; and with no daemon nothing comes
// back that could pass for a confirmation.
func FuzzPeerReceive(f *testing.F) {
	for _, l := range []string{
		`{"op":"send","v":3,"id":"7","to":"aid-self","envelope":"ZW52"}`,
		`{"op":"send","v":2,"id":"7","to":"aid-self","envelope":"ZW52","not_delivered":true}`,
		`{"op":"send","id":"7","to":"aid-self","payload":"cGxhaW4"}`,
		`{"op":"send","v":3,"id":"7","to":"aid-other","envelope":"ZW52"}`,
		`{"op":"send","v":3,"id":"7","to":"aid-self","envelope":"ZW52","dack":"AAAA"}`,
		`{"op":"hello","v":3,"self":"x"}`,
	} {
		f.Add([]byte(l))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		var in frame
		if json.Unmarshal(line, &in) != nil {
			return
		}
		p := &peer{acks: map[string]chan handOffAnswer{}, limits: newSourceLimits(), self: "aid-self"}
		out := p.receive("fuzz", in)
		if out.V != wireVersion || out.ID != in.ID {
			t.Fatalf("%s: answered %+v", line, out)
		}
		if out.DAck.set {
			t.Fatalf("%s: a confirmation %q with no daemon to make one", line, out.DAck.s)
		}
		if in.V != wireVersion && (out.Op != "error" || out.Error != versionRefused(in.V)) {
			t.Fatalf("%s: a frame of version %d answered %+v", line, in.V, out)
		}
		if out.Error == "" {
			t.Fatalf("%s: answered as taken with no daemon attached", line)
		}
	})
}

// Whatever the receiving daemon puts in the dack of its ack, the answer
// this process gives the sending peer carries the same string: not
// decoded, measured, trimmed or dropped (DLV-W-9).
func FuzzTheDAckIsCarried(f *testing.F) {
	for _, d := range []string{"", strings.Repeat("Ab-_", 26) + "Abc", "AAAA=", "+/", "\u00e9<&>", strings.Repeat("x", 300), " \n"} {
		f.Add(d)
	}
	f.Fuzz(func(t *testing.T, d string) {
		// What a daemon can say is a JSON string: the value after a round
		// trip through the wire is what has to arrive.
		b, _ := json.Marshal(d)
		var want string
		if json.Unmarshal(b, &want) != nil {
			return
		}
		ours, theirs := net.Pipe()
		defer ours.Close()
		defer theirs.Close()
		p := &peer{acks: map[string]chan handOffAnswer{}, limits: newSourceLimits(), self: "aid-self"}
		go p.serveDaemon(ours)
		go func() {
			r := bufio.NewReader(theirs)
			enc := json.NewEncoder(theirs)
			_ = enc.Encode(frame{Op: "hello", V: wireVersion, Self: "aid-self"})
			for {
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var f frame
				if json.Unmarshal(line, &f) == nil && f.Op == "recv" {
					// Present, "" included: an ack that has a dack member.
					_ = enc.Encode(frame{Op: "ack", V: wireVersion, ID: f.ID, DAck: dackText{s: d, set: true}})
				}
			}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.mu.Lock()
			attached := p.conn != nil
			p.mu.Unlock()
			if attached {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the daemon never attached")
			}
			time.Sleep(time.Millisecond)
		}
		out := p.receive("fuzz", frame{Op: "send", V: wireVersion, ID: "7", To: "aid-self", Envelope: "ZW52"})
		if out.Error != "" || !out.DAck.set || out.DAck.s != want {
			t.Fatalf("dack %q answered as %+v", want, out)
		}
	})
}

// A line read on any leg, whatever its bytes: a frame readFrame takes is of
// this version, of an op the leg carries, with every member its row must
// have and none it does not, each of its type; written back and read again
// it is the same frame. readFrame never panics.
func FuzzReadFrame(f *testing.F) {
	for _, l := range []string{
		`{"op":"hello","v":3,"self":"a"}`,
		`{"op":"send","v":3,"id":"1","to":"b","envelope":"ZW52","addr":"h:1"}`,
		`{"op":"send","v":3,"id":"1","to":"b","envelope":"ZW52"}`,
		`{"op":"ack","v":3,"id":"d1","dack":""}`,
		`{"op":"ack","v":3,"id":"d1","dack":null}`,
		`{"op":"nack","v":3,"id":"d1","error":"x"}`,
		`{"op":"error","v":3}`,
		`{"op":"send","v":3,"id":"1","dack":"x","error":"e"}`,
		`{"op":"send","v":2,"id":"1"}`,
		`{"op":"send","v":3.0,"id":"1","to":"b","envelope":"ZW52"}`,
		`{"op":"send","v":3,"id":"1","to":"b","envelope":"ZW52","from":"a"}`,
		`[]`, `null`, `{"op":1}`,
	} {
		for leg := range 3 {
			f.Add([]byte(l), uint8(leg))
		}
	}
	legs := []map[string]frameRow{fromDaemon, fromPeer, answerFromPeer}
	f.Fuzz(func(t *testing.T, line []byte, which uint8) {
		leg := legs[int(which)%len(legs)]
		fr, bad, err := readFrame(line, leg)
		if err != nil || bad != "" {
			return
		}
		if fr.V != wireVersion {
			t.Fatalf("%s: a frame of version %d taken", line, fr.V)
		}
		row, ok := leg[fr.Op]
		if !ok {
			t.Fatalf("%s: op %q taken on a leg without it", line, fr.Op)
		}
		var members map[string]json.RawMessage
		if json.Unmarshal(line, &members) != nil {
			t.Fatalf("%s: taken but not an object", line)
		}
		for m := range members {
			if m != "op" && !slices.Contains(row.must, m) && !slices.Contains(row.may, m) {
				t.Fatalf("%s: member %q taken", line, m)
			}
		}
		for _, m := range row.must {
			if _, ok := members[m]; !ok {
				t.Fatalf("%s: taken without %q", line, m)
			}
		}
		back, err := encodeLine(fr)
		if err != nil {
			t.Fatal(err)
		}
		again, bad, err := readFrame(back[:len(back)-1], leg)
		if err != nil || again != fr {
			t.Fatalf("%s: written back as %s, read as %+v (%q, %v)", line, back, again, bad, err)
		}
	})
}
