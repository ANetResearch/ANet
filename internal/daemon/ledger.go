// The daemon's local P6 evidence ledger (C5 收口): every capability effect
// and issued receipt is appended to a per-DID, signed, fork-evident hash
// chain (ANetCore/ael). Storage is an append-only file of base64
// CoreDet-CBOR records, one per line, rebuilt and verified through
// ael.Import on every open — a tampered or forked file refuses to load
// rather than silently serving doctored history. See encodeRecord for why
// the encoding is CBOR and not the JSON the .jsonl path still suggests.
package daemon

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/coredet"
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
			r, err := decodeRecord(line)
			if err != nil {
				return nil, fmt.Errorf("anet: evidence ledger corrupt line: %w", err)
			}
			recs = append(recs, r)
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
	line, err := encodeRecord(rec)
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

// The chain is stored as base64 CoreDet-CBOR, one record per line.
//
// It used to be JSON, and JSON cannot hold this record. An event id is
// derived from the CBOR preimage of the record, and CBOR distinguishes a
// byte string from a text string where JSON does not: a payload carrying
// bytes — a receipt, a signature, any binary at all — comes back from JSON
// as base64 text, the preimage no longer matches, and the id fails to
// re-derive.
//
// The failure is not subtle when it lands. The ledger verifies before use,
// so the daemon refuses to start with "tamper/fork?" against a chain it
// wrote itself, on the first restart after accepting a result. It is latent
// until then, which is why nothing caught it: every test builds a fresh
// chain, and a chain is only read back on a restart with history.
//
// Storing the canonical encoding removes the class of bug rather than the
// instance. Base64 keeps the file line-oriented, so it stays appendable and
// a corrupt line stays one line.
func encodeRecord(rec *ael.EventRecord) ([]byte, error) {
	b, err := coredet.Marshal(rec)
	if err != nil {
		return nil, err
	}
	out := make([]byte, base64.StdEncoding.EncodedLen(len(b)))
	base64.StdEncoding.Encode(out, b)
	return out, nil
}

// decodeRecord reads one line, accepting the JSON that older daemons wrote.
//
// A node upgrading has a chain on disk in the old format, and refusing to
// start would be a worse failure than the one this fixes. Old records that
// carry no bytes re-derive correctly and are read as before; one that does
// carry bytes fails verification here exactly as it did — the format change
// cannot retroactively repair a line whose type information is already
// gone, and pretending otherwise would mean accepting a record whose id
// does not match its content.
func decodeRecord(line []byte) (*ael.EventRecord, error) {
	if len(line) > 0 && line[0] == '{' {
		var r ael.EventRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, err
		}
		return &r, nil
	}
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(line)))
	n, err := base64.StdEncoding.Decode(raw, line)
	if err != nil {
		return nil, err
	}
	var r ael.EventRecord
	if err := coredet.Unmarshal(raw[:n], &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// EvidenceRecord is one event as an operator sees it.
//
// Every field an outsider needs to re-derive the id and re-check the
// chain travels: the preimage fields, the signature, and the link to the
// record before it. A read surface that returned only "what happened"
// would make the chain a log, and the whole point of a hash chain is that
// a reader can check it rather than believe it.
type EvidenceRecord struct {
	Seq       uint64         `json:"seq"`
	ID        string         `json:"id"`
	PrevID    string         `json:"prev_id"`
	EventType string         `json:"event_type"`
	Timestamp int64          `json:"timestamp"`
	SignerAID string         `json:"signer_aid"`
	Payload   map[string]any `json:"payload,omitempty"`
	Sig       string         `json:"sig,omitempty"` // base64, detached over the preimage
}

// EvidenceQuery filters a read of this node's own chain.
type EvidenceQuery struct {
	// EventType, when set, keeps only events of that kind.
	EventType string `json:"event_type,omitempty"`
	// Since keeps events with Seq >= Since.
	Since uint64 `json:"since,omitempty"`
	// Limit caps the number returned, newest last. Zero means the default.
	Limit int `json:"limit,omitempty"`
}

// EvidenceHead summarises the chain itself.
type EvidenceHead struct {
	ChainDID string `json:"chain_did"`
	HeadID   string `json:"head_id"`
	Length   uint64 `json:"length"`
	// State is ACTIVE, or QUARANTINED if a fork was detected. A node
	// serving evidence from a quarantined chain has to say so — that is
	// the one fact a reader most needs and would never think to ask for.
	State string `json:"state"`
}

const (
	defaultEvidenceLimit = 50
	maxEvidenceLimit     = 1000
)

// Evidence reads this node's own chain.
//
// It serves from the verified in-memory ledger, never by re-reading the
// file. The chain is verified once at open and refuses to load if it does
// not re-derive; a second read path that parsed the file directly would
// be an unverified view of the same bytes, and the difference between the
// two would surface as an argument about whose copy is right.
func (l *evidenceLedger) Evidence(q EvidenceQuery) (EvidenceHead, []EvidenceRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()

	head := EvidenceHead{ChainDID: l.did, Length: l.nextSeq, State: l.led.State(l.did)}
	if head.State == "" {
		// No chain yet: nothing has been recorded. ACTIVE is the honest
		// answer — an empty chain is not a broken one.
		head.State = ael.ChainActive
	}
	if l.nextSeq > 0 {
		head.HeadID = l.prevID
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultEvidenceLimit
	}
	if limit > maxEvidenceLimit {
		limit = maxEvidenceLimit
	}

	all := l.led.Events(l.did)
	out := make([]EvidenceRecord, 0, limit)
	for _, r := range all {
		if r.Seq < q.Since {
			continue
		}
		if q.EventType != "" && r.EventType != q.EventType {
			continue
		}
		out = append(out, EvidenceRecord{
			Seq: r.Seq, ID: r.ID, PrevID: r.PrevID, EventType: r.EventType,
			Timestamp: r.Timestamp, SignerAID: r.SignerAID,
			Payload: plainMap(r.Payload),
			Sig:     base64.StdEncoding.EncodeToString(r.Sig),
		})
	}
	// Newest last, and the tail is what an operator wants: "what just
	// happened" is the question being asked almost every time.
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return head, out
}

// plainMap re-keys a CBOR-decoded payload for JSON. CBOR decodes a map
// into map[any]any because its keys need not be strings; ours always are.
func plainMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[k] = plainValue(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = plainValue(val)
		}
		return out
	}
	return nil
}

func plainValue(v any) any {
	switch t := v.(type) {
	case map[any]any, map[string]any:
		return plainMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plainValue(e)
		}
		return out
	case []byte:
		// Bytes are why this chain is CBOR and not JSON. Rendering them as
		// base64 here is a display choice and says so, rather than letting
		// the encoder guess.
		return base64.StdEncoding.EncodeToString(t)
	}
	return v
}
