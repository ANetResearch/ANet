package daemon

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/identity/identitytest"
)

// queuedCount is how many records the queue file of the chain at path
// holds.
func queuedCount(t *testing.T, path string) int {
	t.Helper()
	q, err := readEvidenceQueue(evidenceQueuePath(path))
	if err != nil {
		t.Fatal(err)
	}
	return len(q)
}

// KEL-9 (2026-10-10 ruling; ENG-1a note §5 item 4): records written between
// the witness designation's Prepare and its Adopt are queued unsigned and
// signed afterwards with the key of the tip; the chain reopened after the
// Adopt verifies every record, and so does one reopened after a Discard.
func TestEvidenceAcrossAWitnessDesignationReopens(t *testing.T) {
	for _, outcome := range []string{"adopt", "discard"} {
		t.Run(outcome, func(t *testing.T) {
			self, err := identity.InceptAgent(uint64(nowMillis() - 60_000))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "e.jsonl")
			led, err := openEvidenceLedger(path, self)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := led.Append(EvDelegationSent, map[string]any{"n": 1}); err != nil {
				t.Fatal(err)
			}
			var ev identity.SignedEvent
			if err := led.whileKeyEvent(nowMillis(), func(ts uint64) error {
				ev, err = self.BuildWitnessDesignation(identitytest.Witness, kelDigest(2), ts)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for i := 2; i <= 4; i++ {
				id, err := led.Append(EvDelegationSent, map[string]any{"n": i, "b": []byte{byte(i)}})
				if err != nil || id != "" {
					t.Fatalf("an append while the designation is pending: id %q, %v; want it queued", id, err)
				}
			}
			if n := queuedCount(t, path); n != 3 {
				t.Fatalf("%d records queued, want 3", n)
			}
			// Totals count what is queued (the day's spending, say).
			seen := 0
			led.scan(EvDelegationSent, 0, func(int64, map[string]any) { seen++ })
			if seen != 4 {
				t.Fatalf("scan saw %d records while the designation is pending, want 4", seen)
			}
			if err := led.whileKeyEvent(nowMillis(), func(uint64) error {
				if outcome == "adopt" {
					return self.Adopt(ev)
				}
				return self.DiscardPending()
			}); err != nil {
				t.Fatal(err)
			}
			if n := queuedCount(t, path); n != 0 {
				t.Fatalf("%d records still queued after the %s", n, outcome)
			}
			if _, err := os.Stat(evidenceQueuePath(path)); !os.IsNotExist(err) {
				t.Fatalf("the queue file outlived the %s: %v", outcome, err)
			}
			if _, err := led.Append(EvDelegationSent, map[string]any{"n": 5}); err != nil {
				t.Fatal(err)
			}
			led.Close()
			again, err := openEvidenceLedger(path, self)
			if err != nil {
				t.Fatalf("the chain does not reopen after the %s: %v", outcome, err)
			}
			defer again.Close()
			head, recs := again.Evidence(EvidenceQuery{Limit: 100})
			if head.Length != 5 || len(recs) != 5 {
				t.Fatalf("the reopened chain holds %d records, want 5", head.Length)
			}
			for i, r := range recs[1:4] {
				if r.Payload["n"] != uint64(i+2) {
					t.Fatalf("record %d is %v: the queue went out of order", i+1, r.Payload)
				}
			}
		})
	}
}

// A queue a crash left half flushed is not chained twice: the chain already
// ends with its head, and only the rest is signed.
func TestAHalfFlushedQueueIsNotChainedTwice(t *testing.T) {
	self, err := identity.InceptAgent(uint64(nowMillis() - 60_000))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "e.jsonl")
	led, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	var ev identity.SignedEvent
	if err := led.whileKeyEvent(nowMillis(), func(ts uint64) error {
		ev, err = self.BuildWitnessDesignation(identitytest.Witness, kelDigest(2), ts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := led.Append(EvDelegationSent, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := os.ReadFile(evidenceQueuePath(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := led.whileKeyEvent(nowMillis(), func(uint64) error { return self.Adopt(ev) }); err != nil {
		t.Fatal(err)
	}
	led.Close()
	// The crash: the chain holds the three records, and the queue file is
	// back as it was before the flush removed it.
	if err := os.WriteFile(evidenceQueuePath(path), queue, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if head, _ := again.Evidence(EvidenceQuery{}); head.Length != 3 {
		t.Fatalf("the chain holds %d records after the restart, want 3", head.Length)
	}
	if _, err := os.Stat(evidenceQueuePath(path)); !os.IsNotExist(err) {
		t.Fatalf("the queue file was not removed: %v", err)
	}
}

// A deactivated identity, and one whose dip is pending, record nothing: no
// key of it can sign a record the chain will verify (KEL §2.10).
func TestADeactivatedIdentityRecordsNothing(t *testing.T) {
	self, err := identity.InceptAgent(uint64(nowMillis() - 60_000))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := self.BuildWitnessDesignation(identitytest.Witness, kelDigest(2), uint64(nowMillis()-30_000))
	if err != nil {
		t.Fatal(err)
	}
	if err := self.Adopt(ev); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "e.jsonl")
	led, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	if _, err := led.Append(EvDelegationSent, map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := led.whileKeyEvent(nowMillis(), func(ts uint64) error {
		_, err := self.BuildDeactivation(ed25519.NewKeyFromSeed(kelSeed(2)), ts)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.Append(EvDelegationSent, map[string]any{"n": 2}); err == nil {
		t.Fatal("a record was taken while the dip is pending")
	}
	if n := queuedCount(t, path); n != 0 {
		t.Fatalf("%d records queued for a deactivation", n)
	}
}

// FuzzEvidenceQueueLine: whatever a line of the queue file holds,
// decodeQueuedRecord refuses it or returns a record it writes back byte
// for byte.
func FuzzEvidenceQueueLine(f *testing.F) {
	pb, _ := coredet.Marshal(map[string]any{"a": uint64(1), "b": []byte{1, 2}})
	for _, r := range []queuedRecord{
		{EventType: EvDelegationSent, Payload: pb, Timestamp: 1},
		{EventType: "anet.x", Payload: []byte{0xa0}, Timestamp: 1 << 40},
	} {
		line, err := encodeQueuedRecord(r)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(line)
	}
	f.Add([]byte("oQFh"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, line []byte) {
		r, err := decodeQueuedRecord(line)
		if err != nil {
			return
		}
		if r.EventType == "" || r.Timestamp <= 0 {
			t.Fatalf("decoded a record without a type or a time: %+v", r)
		}
		back, err := encodeQueuedRecord(r)
		if err != nil || !bytes.Equal(back, line) {
			t.Fatalf("a decoded line does not encode back: %q -> %q (%v)", line, back, err)
		}
	})
}
