package daemon

// evidence_queue.go keeps the evidence chain verifiable across a key event
// (KEL §8.1 KEL-9, 2026-10-10 ruling on the ENG-1a note §5 item 4).
//
// A key event — the witness designation (KEL D-13), a rot (D-30, D-32,
// D-40) — is built, signed and given its time when it is prepared, and
// takes effect when the witness accepts it (Adopt) or ends when this node
// drops it (Discard, D-14a). Between the two no key can sign a record that
// both outcomes leave valid: the old key retires at the event's time, so a
// record it signs after that time is refused (REVOKED_KEY) once the event
// is adopted; the key the event reveals is valid only if the event is, so a
// record it signs is refused (KEY_STATE_DOWNGRADE) once the event is
// dropped. The chain is verified before use, so either refusal would keep
// the daemon from starting.
//
// So a record written meanwhile is not signed. It goes to the queue file
// beside the chain (written and synced like the chain), with the time it
// was written; once the event is over it is signed, in order, by the key
// of the tip then, and chained. Its time stays the time it was written,
// which no key the chain then holds has retired by.
//
// Building, adopting and dropping a key event happen under the ledger's
// lock (whileKeyEvent), dated after every record signed so far (eventTime),
// so no record can be signed between the moment the event is dated and the
// moment it is pending, and none the old key signs is dated at or after the
// event.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
)

// evidenceQueuePath is the queue file beside the chain file path.
func evidenceQueuePath(chain string) string { return chain + ".queue" }

// queuedRecord is one record written while a key event was pending: one
// line of the queue file, base64 of its CoreDet encoding.
//
//anet:wire EvidenceQueueRecord
type queuedRecord struct {
	EventType string `cbor:"1,keyasint"`
	// Payload is the CoreDet encoding of the payload, as the chain record
	// will carry it.
	Payload   []byte `cbor:"2,keyasint"`
	Timestamp int64  `cbor:"3,keyasint"`
}

// maxQueueLine bounds one line of the queue file, as the chain's scanner
// bounds one of its own.
const maxQueueLine = 1 << 24

// errEvidenceDeactivated is a record of a deactivated identity: its dip is
// the last record there is (KEL §2.10).
var errEvidenceDeactivated = errors.New("anet: the identity is deactivated; nothing more goes on its evidence chain")

// errEvidenceQueue is a queue file this binary does not read.
var errEvidenceQueue = errors.New("anet: the evidence queue is not valid")

// encodeQueuedRecord is one line of the queue file, without its newline.
func encodeQueuedRecord(r queuedRecord) ([]byte, error) {
	b, err := coredet.Marshal(r)
	if err != nil {
		return nil, err
	}
	out := make([]byte, base64.StdEncoding.EncodedLen(len(b)))
	base64.StdEncoding.Encode(out, b)
	return out, nil
}

// decodeQueuedRecord reads one line of the queue file strictly (core
// CORE-E3): its base64 in one spelling, the three keys and no other, a
// payload that is itself one CoreDet data item, and the canonical encoding
// of all that.
func decodeQueuedRecord(line []byte) (queuedRecord, error) {
	var r queuedRecord
	if len(line) > maxQueueLine {
		return r, fmt.Errorf("%w: a line of %d bytes", errEvidenceQueue, len(line))
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(string(line))
	if err != nil {
		return r, fmt.Errorf("%w: %v", errEvidenceQueue, err)
	}
	if err := coredet.UnmarshalStrict(raw, &r); err != nil {
		return r, fmt.Errorf("%w: %v", errEvidenceQueue, err)
	}
	if re, err := coredet.Marshal(r); err != nil || !bytes.Equal(re, raw) {
		return r, fmt.Errorf("%w: not in its canonical encoding", errEvidenceQueue)
	}
	if r.EventType == "" || r.Timestamp <= 0 {
		return r, fmt.Errorf("%w: a record without a type or a time", errEvidenceQueue)
	}
	var p any
	if err := coredet.Unmarshal(r.Payload, &p); err != nil {
		return r, fmt.Errorf("%w: payload: %v", errEvidenceQueue, err)
	}
	if re, err := coredet.Marshal(p); err != nil || !bytes.Equal(re, r.Payload) {
		return r, fmt.Errorf("%w: a payload not in its canonical encoding", errEvidenceQueue)
	}
	return r, nil
}

// readEvidenceQueue reads the queue file at path; none when there is no
// file. Like the chain (openEvidenceLedger), a last line that does not
// decode is what a write cut short leaves, and is dropped; a bad line
// anywhere else refuses the file.
func readEvidenceQueue(path string) ([]queuedRecord, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), maxQueueLine+1)
	for sc.Scan() {
		if line := sc.Bytes(); len(line) > 0 {
			lines = append(lines, append([]byte(nil), line...))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", errEvidenceQueue, err)
	}
	var out []queuedRecord
	for i, line := range lines {
		r, err := decodeQueuedRecord(line)
		if err != nil {
			if i == len(lines)-1 {
				break // a torn tail: the record was never handed back as written
			}
			return nil, fmt.Errorf("anet: evidence queue line %d of %d: %w", i+1, len(lines), err)
		}
		out = append(out, r)
	}
	return out, nil
}

// The ledger's state with respect to key events.
const (
	holdNone        = iota // sign now
	holdQueue              // a key event is pending: queue
	holdDeactivated        // the identity is deactivated, or its dip pending: nothing more
)

// hold says what a record written now gets (see Append).
func (l *evidenceLedger) hold() int {
	if l.self.State() == identity.StateDeactivated {
		return holdDeactivated
	}
	p, ok := l.self.Pending()
	switch {
	case !ok:
		return holdNone
	case p.Kind == identity.PendingDip:
		return holdDeactivated
	}
	return holdQueue
}

// enqueueLocked writes a record to the queue file, synced, before it
// counts as written. The caller holds l.mu.
func (l *evidenceLedger) enqueueLocked(eventType string, payload any, ts int64) error {
	pb, err := coredet.Marshal(payload)
	if err != nil {
		return err
	}
	// The payload as the chain will carry it: decoded, as a record read
	// back from the chain is.
	var p any
	if err := coredet.Unmarshal(pb, &p); err != nil {
		return err
	}
	if pb, err = coredet.Marshal(p); err != nil {
		return err
	}
	r := queuedRecord{EventType: eventType, Payload: pb, Timestamp: ts}
	line, err := encodeQueuedRecord(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.qpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	l.queue = append(l.queue, r)
	l.lastTS = max(l.lastTS, ts)
	return nil
}

// flushIfFreeLocked signs the queue when no key event holds it back.
func (l *evidenceLedger) flushIfFreeLocked() error {
	if l.hold() != holdNone {
		return nil
	}
	return l.flushLocked()
}

// flushLocked signs the queued records in order with the key of the tip and
// chains them, then removes the queue file. A record the chain already
// ends with — a flush the last process did not finish removing — is not
// chained twice: the chain's last records are compared with the head of the
// queue (type, time and payload). The caller holds l.mu and has checked
// that no key event is pending.
func (l *evidenceLedger) flushLocked() error {
	if len(l.queue) == 0 {
		return nil
	}
	q := l.queue[l.chainedPrefix():]
	for i, r := range q {
		var p any
		if err := coredet.Unmarshal(r.Payload, &p); err != nil {
			return err
		}
		if _, err := l.appendLocked(r.EventType, p, r.Timestamp); err != nil {
			l.queue = q[i:]
			return fmt.Errorf("anet: sign the evidence a key event held back: %w", err)
		}
	}
	l.queue = nil
	if err := os.Remove(l.qpath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// chainedPrefix is how many records at the head of the queue the chain
// already ends with: a flush wrote them, and the process ended before it
// removed the queue file.
func (l *evidenceLedger) chainedPrefix() int {
	events := l.led.Events(l.did)
	for k := min(len(l.queue), len(events)); k > 0; k-- {
		tail := events[len(events)-k:]
		match := true
		for i := 0; i < k && match; i++ {
			match = sameQueued(tail[i], l.queue[i])
		}
		if match {
			return k
		}
	}
	return 0
}

// sameQueued reports whether chain record e is queued record r signed.
func sameQueued(e *ael.EventRecord, r queuedRecord) bool {
	if e.EventType != r.EventType || e.Timestamp != r.Timestamp {
		return false
	}
	b, err := coredet.Marshal(e.Payload)
	return err == nil && bytes.Equal(b, r.Payload)
}

// whileKeyEvent runs f — which builds, adopts or drops a key event — with
// the ledger's lock held, and gives it the time to date a new event at
// (eventTime): no record is signed while f runs, and every record signed
// before is dated earlier than the event. A pending event f leaves is
// what holds the queue; once none is, the queue is signed. On a nil
// ledger (a daemon built without one) f runs alone.
func (l *evidenceLedger) whileKeyEvent(now int64, f func(eventTS uint64) error) error {
	if l == nil {
		return f(uint64(max(now, 1)))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := f(l.eventTime(now)); err != nil {
		return err
	}
	return l.flushIfFreeLocked()
}

// eventTime is when a key event built now is dated: now, but after every
// record signed or queued so far and after the KEL's tip (an event's ts
// increases strictly, KEL R-2).
func (l *evidenceLedger) eventTime(now int64) uint64 {
	ts := max(now, l.lastTS+1)
	if kel := l.self.KEL(); len(kel) > 0 {
		ts = max(ts, int64(kel[len(kel)-1].Event.Timestamp)+1)
	}
	return uint64(ts)
}

// flushQueue signs what the queue holds once no key event holds it back
// (after an Adopt or a Discard made outside whileKeyEvent).
func (l *evidenceLedger) flushQueue() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flushIfFreeLocked()
}

// queuedOf calls fn for each queued record of eventType at or after
// sinceMS, oldest first: what scan adds to the chain's records, so that a
// total read while a key event is pending (the day's spending, say) counts
// what was written meanwhile. The caller holds l.mu.
func (l *evidenceLedger) queuedOf(eventType string, sinceMS int64) []queuedRecord {
	var out []queuedRecord
	for _, r := range l.queue {
		if r.EventType == eventType && r.Timestamp >= sinceMS {
			out = append(out, r)
		}
	}
	return out
}
