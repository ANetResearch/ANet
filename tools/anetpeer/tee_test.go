package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --tee-dir is what the joint run replays (A2A-DESIGN SI-10): the exact
// bytes this peer delivered for its daemon. A copy of something that was
// never delivered would make the replay section test a first delivery, so
// only accepted deliveries are kept. Driven at the frame level, as the two
// daemons would, so it needs no transport module.
func TestTeeKeepsExactlyWhatWasDelivered(t *testing.T) {
	old := handOffTimeout
	handOffTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handOffTimeout = old })

	dir := t.TempDir()
	rv := filepath.Join(dir, "rv")
	tee := filepath.Join(dir, "tee")
	alice, err := startWith(filepath.Join(dir, "a.sock"), filepath.Join(dir, "a.wire"), rv, "", tee)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(alice.stop)
	bob, err := start(filepath.Join(dir, "b.sock"), filepath.Join(dir, "b.wire"), rv, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bob.stop)

	refused := base64.StdEncoding.EncodeToString([]byte("refused"))
	delivered := base64.StdEncoding.EncodeToString([]byte("delivered"))

	// Bob's daemon acknowledges everything but the refused envelope.
	bc := dialUnix(t, filepath.Join(dir, "b.sock"))
	benc := json.NewEncoder(bc)
	if err := benc.Encode(frame{Op: "hello", V: wireVersion, Self: "aid-bob"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		dec := json.NewDecoder(bufio.NewReader(bc))
		for {
			var f frame
			if dec.Decode(&f) != nil {
				return
			}
			if f.Op == "recv" && f.Envelope != refused {
				_ = benc.Encode(frame{Op: "ack", V: wireVersion, ID: f.ID})
			}
		}
	}()

	ac := dialUnix(t, filepath.Join(dir, "a.sock"))
	aenc, adec := json.NewEncoder(ac), json.NewDecoder(bufio.NewReader(ac))
	if err := aenc.Encode(frame{Op: "hello", V: wireVersion, Self: "aid-alice"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(rv, "aid-bob")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bob never announced himself")
		}
		time.Sleep(10 * time.Millisecond)
	}
	send := func(id, env string) frame {
		t.Helper()
		if err := aenc.Encode(frame{Op: "send", V: wireVersion, ID: id, To: "aid-bob", Envelope: env}); err != nil {
			t.Fatal(err)
		}
		_ = ac.SetReadDeadline(time.Now().Add(5 * time.Second))
		var r frame
		if err := adec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := send("1", refused); r.Error == "" {
		t.Fatal("a delivery the receiving daemon did not acknowledge was reported delivered")
	}
	if r := send("2", delivered); r.Error != "" {
		t.Fatalf("delivery failed: %s", r.Error)
	}

	entries, err := os.ReadDir(tee)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("tee holds %d files, want only the delivered envelope", len(entries))
	}
	if name := entries[0].Name(); name != "000001-aid-bob.env" {
		t.Errorf("tee file %q, want <seq>-<to>.env", name)
	}
	b, err := os.ReadFile(filepath.Join(tee, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != delivered+"\n" {
		t.Fatalf("tee kept %q, want the delivered envelope's base64", b)
	}
}

func dialUnix(t *testing.T, path string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
