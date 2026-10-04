package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// FRAME-3 seen from this side of the wire: the golden of module/p2p
// (module/p2p/testdata/frame-v3.json), read with this process's own frame
// type, and the frames it refuses, driven over real sockets. These tests
// speak raw frames for both daemons, so they need no transport module.

const frame3Golden = "../../module/p2p/testdata/frame-v3.json"

type frame3 struct {
	Values struct {
		Self, To, Addr string
	} `json:"values"`
	Frames []struct {
		Op, Dir, Case, Line string
	} `json:"frames"`
	Refused []struct {
		Case, At, Line, Answer string
		Closes                 bool
	} `json:"refused"`
}

func loadFrame3(t *testing.T) *frame3 {
	t.Helper()
	b, err := os.ReadFile(frame3Golden)
	if err != nil {
		t.Fatal(err)
	}
	var g frame3
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Frames) == 0 || len(g.Refused) == 0 {
		t.Fatalf("%s holds no frames", frame3Golden)
	}
	return &g
}

// Every frame of the golden reads into this process's frame type and
// writes back as the same bytes: the two ends of the wire agree on every
// member and its spelling.
func TestFRAME3AtThePeer(t *testing.T) {
	g := loadFrame3(t)
	for _, gl := range g.Frames {
		var f frame
		if err := json.Unmarshal([]byte(gl.Line), &f); err != nil {
			t.Errorf("%s %s: %v", gl.Op, gl.Case, err)
			continue
		}
		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(f); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSuffix(b.String(), "\n"); got != gl.Line {
			t.Errorf("%s (%s) %s: written back as\n%s\nnot\n%s", gl.Op, gl.Dir, gl.Case, got, gl.Line)
		}
		if f.V != wireVersion {
			t.Errorf("%s %s: v %d, this peer speaks %d", gl.Op, gl.Case, f.V, wireVersion)
		}
	}
}

// rawDaemon is a daemon speaking frames on a peer process's daemon socket.
type rawDaemon struct {
	t   *testing.T
	c   net.Conn
	mu  sync.Mutex
	enc *json.Encoder
	r   *bufio.Reader
}

func dialRawDaemon(t *testing.T, sock string) *rawDaemon {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &rawDaemon{t: t, c: c, enc: json.NewEncoder(c), r: bufio.NewReader(c)}
}

func (d *rawDaemon) write(line string) {
	d.t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.c.Write([]byte(line + "\n")); err != nil {
		d.t.Fatal(err)
	}
}

func (d *rawDaemon) send(f frame) {
	d.t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.enc.Encode(f); err != nil {
		d.t.Fatal(err)
	}
}

// read returns the next frame and its members, or io.EOF once the peer
// process closed the connection.
func (d *rawDaemon) read() (frame, map[string]json.RawMessage, error) {
	_ = d.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := d.r.ReadBytes('\n')
	if err != nil {
		return frame{}, nil, err
	}
	var f frame
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &f); err != nil {
		d.t.Fatalf("the peer process wrote %q: %v", line, err)
	}
	_ = json.Unmarshal(line, &m)
	return f, m, nil
}

// twoPeers starts two peer processes on one directory rendezvous and
// attaches a raw daemon to each, announced as aid-alice and aid-bob.
func twoPeers(t *testing.T) (alice, bob *rawDaemon, dir string) {
	t.Helper()
	dir = t.TempDir()
	rv := filepath.Join(dir, "rv")
	for _, n := range []string{"a", "b"} {
		p, err := start(filepath.Join(dir, n+".sock"), filepath.Join(dir, n+".wire"), rv, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.stop)
	}
	alice, bob = dialRawDaemon(t, filepath.Join(dir, "a.sock")), dialRawDaemon(t, filepath.Join(dir, "b.sock"))
	alice.send(frame{Op: "hello", V: wireVersion, Self: "aid-alice"})
	bob.send(frame{Op: "hello", V: wireVersion, Self: "aid-bob"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, e1 := os.Stat(filepath.Join(rv, "aid-alice"))
		_, e2 := os.Stat(filepath.Join(rv, "aid-bob"))
		if e1 == nil && e2 == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the daemons were never announced")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return alice, bob, dir
}

// T43, M36: a peer process passes the receiving daemon's dack on as it
// came. It does not decode it, measure it, fix it or drop it: a well-formed
// one, one a byte short or long, one padded or in the standard alphabet,
// one that is not base64 at all — each reaches the sending daemon as the
// receiving daemon wrote it, on the answer to its own send. Telling a
// malformed one from a good one is the sending daemon's job (DLV-W-11b).
func TestTheDAckIsCarriedAsItCame(t *testing.T) {
	alice, bob, _ := twoPeers(t)
	good := strings.Repeat("Ab-_", 26) + "Abc" // 107 characters
	dacks := map[string]string{
		"ZW52LTE": good,
		"ZW52LTI": good[:106],
		"ZW52LTM": good + "A",
		"ZW52LTQ": good + "=",
		"ZW52LTU": strings.ReplaceAll(good, "-", "+"),
		"ZW52LTY": "not base64 at all; <&> \u00e9",
		"ZW52LTc": strings.Repeat("x", 4000),
		"ZW52LTg": "", // an ack without one
	}
	// Bob's daemon acks every delivery with the dack chosen by its envelope
	// and keeps the recv frames it was handed.
	var recvs []map[string]json.RawMessage
	var mu sync.Mutex
	go func() {
		for {
			f, m, err := bob.read()
			if err != nil {
				return
			}
			if f.Op != "recv" {
				continue
			}
			mu.Lock()
			recvs = append(recvs, m)
			mu.Unlock()
			bob.send(frame{Op: "ack", V: wireVersion, ID: f.ID, DAck: dacks[f.Envelope]})
		}
	}()
	n := 0
	for env, want := range dacks {
		n++
		id := "s" + env
		alice.send(frame{Op: "send", V: wireVersion, ID: id, To: "aid-bob", Envelope: env, Addr: "203.0.113.7:39100"})
		reply, members, err := alice.read()
		if err != nil {
			t.Fatalf("no answer for %s: %v", env, err)
		}
		if reply.Op != "send" || reply.V != wireVersion || reply.ID != id || reply.Error != "" {
			t.Fatalf("answer %+v", reply)
		}
		if reply.DAck != want {
			t.Errorf("dack %q came back as %q", want, reply.DAck)
		}
		_, has := members["dack"]
		if has != (want != "") {
			t.Errorf("dack %q: the answer has a dack member: %v", want, has)
		}
		for m := range members {
			switch m {
			case "op", "v", "id", "dack":
			default:
				t.Errorf("the answer carries %q", m)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(recvs) != n {
		t.Fatalf("bob's daemon was handed %d deliveries, want %d", len(recvs), n)
	}
	for _, m := range recvs {
		for k := range m {
			switch k {
			case "op", "v", "id", "envelope":
			default:
				t.Errorf("a recv frame carries %q", k)
			}
		}
	}
}

// The refused frames of the golden that a peer process reads: from its own
// daemon (a hello or a send of another version) and from another peer (a
// send of another version). Each is answered with an error frame of v 3
// carrying the frame's id; a hello also closes the connection (M38); none
// is delivered or handed to a daemon.
func TestFRAME3RefusedByThePeer(t *testing.T) {
	g := loadFrame3(t)
	n := 0
	for _, r := range g.Refused {
		if r.At == "daemon" {
			continue
		}
		n++
		t.Run(r.At+": "+r.Case, func(t *testing.T) {
			alice, bob, dir := twoPeers(t)
			handed := make(chan frame, 4)
			go func() {
				for {
					f, _, err := bob.read()
					if err != nil {
						return
					}
					handed <- f
				}
			}()
			var in frame
			if err := json.Unmarshal([]byte(r.Line), &in); err != nil {
				t.Fatal(err)
			}
			var reply frame
			var closed bool
			switch r.At {
			case "anetpeer from its daemon":
				// A fresh connection for a hello; the announced one for a send.
				d := alice
				if in.Op == "hello" {
					d = dialRawDaemon(t, filepath.Join(dir, "a.sock"))
				}
				d.write(r.Line)
				var err error
				if reply, _, err = d.read(); err != nil {
					t.Fatalf("no answer: %v", err)
				}
				if r.Closes {
					_, _, err := d.read()
					closed = err == io.EOF
				}
			case "anetpeer from a peer":
				c, err := net.Dial("unix", filepath.Join(dir, "b.wire"))
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := c.Write([]byte(r.Line + "\n")); err != nil {
					t.Fatal(err)
				}
				if err := json.NewDecoder(c).Decode(&reply); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("unknown reader %q", r.At)
			}
			if r.Answer != "error" || reply.Op != "error" || reply.V != wireVersion || reply.ID != in.ID ||
				reply.Error != versionRefused(in.V) || reply.DAck != "" {
				t.Fatalf("answer %+v, want an error frame of v %d naming version %d, id %q", reply, wireVersion, in.V, in.ID)
			}
			if r.Closes && !closed {
				t.Fatal("the connection stayed open after a hello of another version")
			}
			select {
			case f := <-handed:
				t.Fatalf("bob's daemon was handed %+v", f)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
	if n == 0 {
		t.Fatal("the golden refuses nothing at a peer process")
	}
}

// After a hello of another version is refused, a daemon that greets in v 3
// is carried as before: the refusal does not stick to the socket.
func TestAHelloInThisVersionIsCarriedAfterARefusedOne(t *testing.T) {
	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	p, err := start(filepath.Join(dir, "a.sock"), filepath.Join(dir, "a.wire"), rv, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.stop)
	old := dialRawDaemon(t, filepath.Join(dir, "a.sock"))
	old.write(`{"op":"hello","v":2,"self":"aid-old"}`)
	if f, _, err := old.read(); err != nil || f.Op != "error" {
		t.Fatalf("hello v 2: %+v %v", f, err)
	}
	if _, err := os.Stat(filepath.Join(rv, "aid-old")); err == nil {
		t.Fatal("a daemon refused for its frame version was announced")
	}
	cur := dialRawDaemon(t, filepath.Join(dir, "a.sock"))
	cur.send(frame{Op: "hello", V: wireVersion, Self: "aid-new"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(rv, "aid-new")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a v 3 hello after a refused one was not carried")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The receiving daemon's error frame for a delivery (one it could not read)
// answers that delivery at once, as its nack does: the sending daemon hears
// of it now, without a dack, and goes to the hub, rather than after the
// whole hand-off timeout.
func TestADaemonsErrorFrameAnswersItsDeliveryAtOnce(t *testing.T) {
	alice, bob, _ := twoPeers(t)
	go func() {
		for {
			f, _, err := bob.read()
			if err != nil {
				return
			}
			if f.Op == "recv" {
				bob.send(frame{Op: "error", V: wireVersion, ID: f.ID, Error: "the envelope is not base64url without padding"})
			}
		}
	}()
	start := time.Now()
	alice.send(frame{Op: "send", V: wireVersion, ID: "s1", To: "aid-bob", Envelope: "ZW52"})
	// read gives up after 5 seconds, half the hand-off timeout.
	reply, _, err := alice.read()
	if err != nil {
		t.Fatalf("no answer (the hand-off waits %s): %v", handOffTimeout, err)
	}
	if reply.Op != "send" || reply.V != wireVersion || reply.ID != "s1" || reply.Error == "" || reply.DAck != "" {
		t.Fatalf("answer %+v, want the send answered with an error and no dack", reply)
	}
	if d := time.Since(start); d >= handOffTimeout {
		t.Fatalf("answered after %s: the daemon's error frame did not release the hand-off", d)
	}
}

// fixedAnswerPeer is a peer wire that answers every delivery with answer,
// its id set to the delivery's.
func fixedAnswerPeer(t *testing.T, answer frame) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var f frame
				if err := json.NewDecoder(bufio.NewReader(c)).Decode(&f); err != nil {
					return
				}
				a := answer
				a.ID = f.ID
				_ = json.NewEncoder(c).Encode(a)
			}(c)
		}
	}()
	return l.Addr().String()
}

// What the receiving peer answers reaches the sending daemon as it came when
// it is an answer of this version: one that reports a failure and still
// carries a dack (a send answer may carry both) keeps its dack (DLV-W-9).
// An answer of another version is no answer: the send fails and nothing in
// it, a dack included, is passed on.
func TestTheReceivingPeersAnswerIsPassedOnInThisVersionOnly(t *testing.T) {
	dack := strings.Repeat("Ab-_", 26) + "Abc"
	for _, tc := range []struct {
		name     string
		answer   frame
		wantDAck string
	}{
		{"a failure with a dack", frame{Op: "send", V: wireVersion, DAck: dack, Error: "the answer came late"}, dack},
		{"an answer of version 2", frame{Op: "send", V: 2, DAck: dack}, ""},
		{"an answer of version 0", frame{Op: "send", DAck: dack}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			rv := filepath.Join(dir, "rv")
			p, err := start(filepath.Join(dir, "a.sock"), filepath.Join(dir, "a.wire"), rv, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(p.stop)
			if err := os.MkdirAll(rv, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(rv, "aid-far"), []byte("tcp://"+fixedAnswerPeer(t, tc.answer)), 0o644); err != nil {
				t.Fatal(err)
			}
			d := dialRawDaemon(t, filepath.Join(dir, "a.sock"))
			d.send(frame{Op: "hello", V: wireVersion, Self: "aid-alice"})
			d.send(frame{Op: "send", V: wireVersion, ID: "s1", To: "aid-far", Envelope: "ZW52"})
			reply, _, err := d.read()
			if err != nil {
				t.Fatalf("no answer: %v", err)
			}
			if reply.Op != "send" || reply.V != wireVersion || reply.ID != "s1" || reply.Error == "" || reply.DAck != tc.wantDAck {
				t.Fatalf("answer %+v, want a failed send carrying dack %q", reply, tc.wantDAck)
			}
		})
	}
}
