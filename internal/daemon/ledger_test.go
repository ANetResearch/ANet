package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
)

// A chain must be readable by the daemon that wrote it.
//
// It was not. The chain was stored as JSON while an event id is derived
// from the record's CBOR preimage, and CBOR distinguishes a byte string
// from text where JSON does not — so a payload carrying a receipt came back
// as base64 text, the preimage changed, and the id failed to re-derive. The
// ledger verifies before use, so the daemon refused to start against its
// own chain with "tamper/fork?", on the first restart after accepting a
// result.
//
// Latent until then, which is why nothing caught it: every other test
// builds a fresh chain, and a chain is only read back on a restart with
// history in it.
func TestChainWithBinaryPayloadReloads(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evidence.ael.jsonl")

	led, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the shape that broke it: a result carrying a signed receipt.
	if _, err := led.Append(EvResultAccepted, map[string]any{
		"interaction_id": "ix-1",
		"result_cid":     "bafy...",
		"receipt_bytes":  []byte{0xa1, 0x02, 0x03, 0xff},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.Append(EvDelegationSent, map[string]any{
		"interaction_id": "ix-2", "provider_aid": "aid", "request_cid": "bafy...",
	}); err != nil {
		t.Fatal(err)
	}
	if err := led.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatalf("the daemon must be able to reload its own chain: %v", err)
	}
	defer reopened.Close()

	// And it must continue the chain rather than restart it: a fork in a
	// node's own evidence is the thing the ledger exists to make impossible.
	if reopened.nextSeq != 2 {
		t.Fatalf("nextSeq = %d after two records, want 2", reopened.nextSeq)
	}
	if _, err := reopened.Append(EvCapabilityEffect, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("appending after a reload must work: %v", err)
	}
}

// A chain written by an older daemon still loads, as long as its records
// re-derive. Refusing to start on an upgrade would be a worse failure than
// the one this fixes.
func TestLegacyJSONChainStillLoads(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.ael.jsonl")

	// Write one record in the current format, then re-encode it as the JSON
	// an older daemon would have written.
	led, err := openEvidenceLedger(path, self)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := led.Append(EvDelegationSent, map[string]any{
		"interaction_id": "ix-1", "provider_aid": "aid", "request_cid": "bafy...",
	}); err != nil {
		t.Fatal(err)
	}
	led.Close()

	line, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := decodeRecord([]byte(trimNewline(string(line))))
	if err != nil {
		t.Fatal(err)
	}
	// What an older daemon wrote: json.Marshal of the same record.
	rec.Payload = plainMap(rec.Payload)
	legacy, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(legacy, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := openEvidenceLedger(path, self); err != nil {
		t.Fatalf("a chain from an older daemon must still load: %v", err)
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// The chain has to be readable by the node that keeps it.
//
// It was not, for its whole existence. Every capability effect, receipt
// and accepted result went on, and nothing read it back — an audit
// substrate whose only reader was the verifier that refuses to start.
// The operator accumulating the evidence was the one person who could not
// look at it.
func TestTheChainCanBeRead(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	led, err := openEvidenceLedger(filepath.Join(t.TempDir(), "e.jsonl"), self)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()

	for i := 0; i < 3; i++ {
		if _, err := led.Append(EvCapabilityEffect, map[string]any{
			"capability": "cas.put", "n": i,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := led.Append(EvResultAccepted, map[string]any{
		"interaction_id": "ix-1",
		// Bytes are why this chain is CBOR; the read surface has to render
		// them as something JSON can carry.
		"receipt_bytes": []byte{0xde, 0xad},
	}); err != nil {
		t.Fatal(err)
	}

	head, recs := led.Evidence(EvidenceQuery{})
	if head.ChainDID != self.DID() {
		t.Errorf("chain did = %q", head.ChainDID)
	}
	if head.Length != 4 {
		t.Errorf("length = %d, want 4", head.Length)
	}
	if head.State != "ACTIVE" {
		t.Errorf("state = %q, want ACTIVE", head.State)
	}
	if len(recs) != 4 {
		t.Fatalf("got %d records, want 4", len(recs))
	}

	// The links must come back, or a reader can only believe the node.
	for i, r := range recs {
		if r.ID == "" || r.Sig == "" {
			t.Errorf("record %d has no id or signature — nothing to check", i)
		}
		if i > 0 && r.PrevID != recs[i-1].ID {
			t.Errorf("record %d does not link to %d", i, i-1)
		}
	}
	if head.HeadID != recs[len(recs)-1].ID {
		t.Errorf("head id %s is not the last record %s", head.HeadID, recs[len(recs)-1].ID)
	}
	if got := recs[3].Payload["receipt_bytes"]; got != "3q0=" {
		t.Errorf("bytes rendered as %v, want base64 3q0=", got)
	}

	// Filters.
	_, effects := led.Evidence(EvidenceQuery{EventType: EvCapabilityEffect})
	if len(effects) != 3 {
		t.Errorf("type filter returned %d, want 3", len(effects))
	}
	_, since := led.Evidence(EvidenceQuery{Since: 2})
	if len(since) != 2 {
		t.Errorf("since filter returned %d, want 2", len(since))
	}
	// A limit keeps the tail: "what just happened" is the question.
	_, tail := led.Evidence(EvidenceQuery{Limit: 1})
	if len(tail) != 1 || tail[0].Seq != 3 {
		t.Errorf("limit returned %+v, want the newest record", tail)
	}
}

// An empty chain is not a broken one, and a node with no ledger at all
// must answer rather than crash.
func TestReadingAnEmptyChain(t *testing.T) {
	self, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	led, err := openEvidenceLedger(filepath.Join(t.TempDir(), "e.jsonl"), self)
	if err != nil {
		t.Fatal(err)
	}
	defer led.Close()
	head, recs := led.Evidence(EvidenceQuery{})
	if head.State != "ACTIVE" {
		t.Errorf("an empty chain must read as ACTIVE, got %q", head.State)
	}
	if head.Length != 0 || len(recs) != 0 {
		t.Errorf("empty chain returned %d records, length %d", len(recs), head.Length)
	}
	if head.HeadID != "" {
		t.Errorf("an empty chain has no head, got %q", head.HeadID)
	}
}
