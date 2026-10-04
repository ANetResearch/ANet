package main

import (
	"bufio"
	"encoding/json"
	"net"
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
		if out.DAck != "" {
			t.Fatalf("%s: a confirmation %q with no daemon to make one", line, out.DAck)
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
	f.Fuzz(func(t *testing.T, dack string) {
		// What a daemon can say is a JSON string: the value after a round
		// trip through the wire is what has to arrive.
		b, _ := json.Marshal(dack)
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
			_ = enc.Encode(frame{Op: "hello", V: wireVersion, Self: ""})
			for {
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var f frame
				if json.Unmarshal(line, &f) == nil && f.Op == "recv" {
					_ = enc.Encode(frame{Op: "ack", V: wireVersion, ID: f.ID, DAck: dack})
				}
			}
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			p.mu.Lock()
			attached := p.enc != nil
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
		if out.Error != "" || out.DAck != want {
			t.Fatalf("dack %q answered as %+v", want, out)
		}
	})
}
