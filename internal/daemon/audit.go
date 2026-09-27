package daemon

// audit.go reads this node's evidence chain from disk for `anet audit`, and
// checks an exported copy for `anet verify --chain` (A2A-DESIGN §14).
//
// This is a second read path next to evidenceLedger.Evidence, and it is a
// verified one: every record is decoded, its id re-derived, its signature
// checked against the key history, and the links checked from genesis, the
// same ael.Import the daemon runs when it opens the file. It exists because
// an audit has to work with the daemon stopped and has to see the whole
// chain, where the control route serves a bounded tail.
//
// Reading while a daemon appends is safe: the file is append-only and one
// record is one line, so a reader sees a prefix, possibly ending in a line
// still being written. That last line is left out and reported, as the
// daemon does for a torn write; an undecodable line anywhere else is a
// refusal.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/identity"
)

// EvidenceChain is a whole evidence chain, verified.
type EvidenceChain struct {
	// SignerAID is whose chain this is; ChainDID is "did:anet:" + SignerAID.
	SignerAID string
	Head      EvidenceHead
	Records   []EvidenceRecord
	// Lines are the verified records exactly as stored, in chain order.
	Lines [][]byte
	// KEL is the signer's key history (identity.MarshalKEL) the records
	// were verified against.
	KEL []byte
	// TornTail is set when the file's last line did not decode and was left
	// out: a record being written, or one torn by a crash.
	TornTail bool
}

// ReadEvidenceChain reads and verifies l's evidence chain against the key
// history in l's identity file. A data directory with no chain file yields
// an empty chain.
func ReadEvidenceChain(l Layout) (*EvidenceChain, error) {
	idb, err := os.ReadFile(l.IdentityPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("anet: no identity in %s yet, so no evidence chain (it is created by `anet up`)", l.Root)
	}
	if err != nil {
		return nil, fmt.Errorf("anet: read identity: %w", err)
	}
	c, err := identity.Restore(idb)
	if err != nil {
		return nil, fmt.Errorf("anet: restore identity %s: %w", l.IdentityPath(), err)
	}
	b, err := os.ReadFile(l.EvidenceLedgerPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lines := splitLines(b)
	torn := false
	if n := len(lines); n > 0 {
		if _, derr := decodeRecord(lines[n-1]); derr != nil {
			lines, torn = lines[:n-1], true
		}
	}
	ch, err := VerifyEvidenceLines(lines, c.KEL())
	if err != nil {
		return nil, fmt.Errorf("anet: evidence chain %s: %w", l.EvidenceLedgerPath(), err)
	}
	ch.TornTail = torn
	return ch, nil
}

// splitLines splits b into its non-empty lines.
func splitLines(b []byte) [][]byte {
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if line := sc.Bytes(); len(line) > 0 {
			out = append(out, append([]byte(nil), line...))
		}
	}
	return out
}

// SplitEvidenceLines splits a stored or exported chain file into records.
func SplitEvidenceLines(b []byte) [][]byte { return splitLines(b) }

// VerifyEvidenceLines decodes stored records and verifies them as one chain
// signed under kel: each id re-derives, each signature verifies at the
// record's time, every record belongs to the KEL's AID, sequence numbers run
// from 0 without a gap, and each record links to the one before it.
func VerifyEvidenceLines(lines [][]byte, kel []identity.SignedEvent) (*EvidenceChain, error) {
	states, err := identity.Replay(kel)
	if err != nil {
		return nil, fmt.Errorf("key history does not verify: %w", err)
	}
	aid := states[len(states)-1].AID
	did := "did:anet:" + aid
	kb, err := identity.MarshalKEL(kel)
	if err != nil {
		return nil, err
	}
	ch := &EvidenceChain{SignerAID: aid, KEL: kb, Head: EvidenceHead{ChainDID: did, State: ael.ChainActive},
		Records: []EvidenceRecord{}}
	recs := make([]*ael.EventRecord, 0, len(lines))
	for i, line := range lines {
		r, err := decodeRecord(line)
		if err != nil {
			return nil, fmt.Errorf("record %d of %d does not decode: %w", i+1, len(lines), err)
		}
		if r.ChainDID != did || r.SignerAID != aid {
			return nil, fmt.Errorf("record %d (seq %d) is on chain %s signed by %s, not on %s", i+1, r.Seq,
				r.ChainDID, r.SignerAID, did)
		}
		if r.Seq != uint64(i) {
			return nil, fmt.Errorf("record %d has seq %d: the chain has a gap or is out of order", i+1, r.Seq)
		}
		recs = append(recs, r)
	}
	led := ael.NewLedger()
	res, err := led.ImportBatch(recs, kel)
	if err != nil {
		return nil, fmt.Errorf("the chain does not verify (tampered or forked): %w", err)
	}
	if res.Staged > 0 || res.Dups > 0 {
		return nil, fmt.Errorf("the chain does not verify: %d records do not link, %d repeat", res.Staged, res.Dups)
	}
	if st := led.State(did); st != "" {
		ch.Head.State = st
	}
	for _, r := range recs {
		ch.Records = append(ch.Records, evidenceRecordOf(r))
	}
	ch.Lines = lines
	ch.Head.Length = uint64(len(recs))
	if n := len(recs); n > 0 {
		ch.Head.HeadID = recs[n-1].ID
	}
	return ch, nil
}

// evidenceRecordOf is the operator's view of one record, as Evidence serves
// it.
func evidenceRecordOf(r *ael.EventRecord) EvidenceRecord {
	return EvidenceRecord{
		Seq: r.Seq, ID: r.ID, PrevID: r.PrevID, EventType: r.EventType,
		Timestamp: r.Timestamp, SignerAID: r.SignerAID,
		Payload: plainMap(r.Payload),
		Sig:     base64.StdEncoding.EncodeToString(r.Sig),
	}
}
