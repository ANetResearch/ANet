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
