// The daemon's local P6 evidence ledger (C5 收口): every capability effect
// and issued receipt is appended to a per-DID, signed, fork-evident hash
// chain (ANetCore/ael). Storage is an append-only JSONL file rebuilt and
// verified through ael.Import on every open — a tampered or forked file
// refuses to load rather than silently serving doctored history.
package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/identity"
)

// EvCapabilityEffect records one executed capability call and its effect.
const EvCapabilityEffect = "anet.capability.effect"

// EvReceipt records an issued interaction receipt (provider side).
const EvReceipt = "anet.interaction.receipt"

// EvDelegationSent records a task this node delegated to someone else.
const EvDelegationSent = "anet.delegation.sent"

// EvResultAccepted records a result this node received and accepted —
// the requester-side counterpart of EvReceipt, so a completed interaction
// leaves evidence on BOTH chains.
const EvResultAccepted = "anet.result.accepted"

type evidenceLedger struct {
	mu      sync.Mutex
	led     *ael.Ledger
	f       *os.File
	self    *identity.Controller
	kel     []identity.SignedEvent
	did     string
	nextSeq uint64
	prevID  string
}

// openEvidenceLedger loads (verify-before-use) or creates the chain file.
func openEvidenceLedger(path string, self *identity.Controller) (*evidenceLedger, error) {
	l := &evidenceLedger{
		led:    ael.NewLedger(),
		self:   self,
		kel:    self.KEL(),
		did:    self.DID(),
		prevID: ael.GenesisPrev(),
	}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		var recs []*ael.EventRecord
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var r ael.EventRecord
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("anet: evidence ledger corrupt line: %w", err)
			}
			recs = append(recs, &r)
		}
		if err := l.led.Import(recs, l.kel); err != nil {
			return nil, fmt.Errorf("anet: evidence ledger refuses to load (tamper/fork?): %w", err)
		}
		for _, r := range recs {
			if r.Seq >= l.nextSeq {
				l.nextSeq = r.Seq + 1
				l.prevID = r.ID
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l.f = f
	return l, nil
}

// Append signs and chains one event, verifies it into the in-memory ledger,
// then persists the line. Returns the event id.
func (l *evidenceLedger) Append(eventType string, payload any) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec := &ael.EventRecord{
		ChainDID: l.did, Seq: l.nextSeq, PrevID: l.prevID,
		EventType: eventType, VersionMajor: ael.VersionMajor2,
		Payload: payload, Timestamp: nowMillis(),
	}
	if err := rec.Sign(l.self); err != nil {
		return "", err
	}
	if err := l.led.Append(rec, l.kel); err != nil {
		return "", err
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return "", err
	}
	l.nextSeq, l.prevID = rec.Seq+1, rec.ID
	return rec.ID, nil
}

func (l *evidenceLedger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}
