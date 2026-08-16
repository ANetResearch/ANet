package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"
)

func testLedger(t *testing.T) (*evidenceLedger, string, *identity.Controller) {
	t.Helper()
	ctrl, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "evidence.ael.jsonl")
	l, err := openEvidenceLedger(path, ctrl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path, ctrl
}

func TestLedgerAppendAndReload(t *testing.T) {
	l, path, ctrl := testLedger(t)
	id1, err := l.Append(EvCapabilityEffect, map[string]any{"capability": "x", "status": "OK"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := l.Append(EvReceipt, map[string]any{"interaction_id": "i-1"})
	if err != nil || id1 == id2 {
		t.Fatalf("append: %v (ids %s %s)", err, id1, id2)
	}
	_ = l.Close()

	// reopen: Import must verify the whole chain and resume at seq 2
	l2, err := openEvidenceLedger(path, ctrl)
	if err != nil {
		t.Fatalf("verified reload failed: %v", err)
	}
	defer l2.Close()
	if l2.nextSeq != 2 || l2.prevID != id2 {
		t.Fatalf("resume state wrong: seq=%d prev=%s", l2.nextSeq, l2.prevID)
	}
	if _, err := l2.Append(EvReceipt, map[string]any{"interaction_id": "i-2"}); err != nil {
		t.Fatalf("append after reload: %v", err)
	}
}

func TestLedgerRefusesTamper(t *testing.T) {
	l, path, ctrl := testLedger(t)
	if _, err := l.Append(EvCapabilityEffect, map[string]any{"capability": "x", "status": "OK"}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	b, _ := os.ReadFile(path)
	doctored := strings.Replace(string(b), `"status":"OK"`, `"status":"FAKED"`, 1)
	if doctored == string(b) {
		// payload key order may differ; flip any OK occurrence
		doctored = strings.Replace(string(b), "OK", "FAKED", 1)
	}
	if err := os.WriteFile(path, []byte(doctored), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openEvidenceLedger(path, ctrl); err == nil {
		t.Fatal("doctored history MUST refuse to load")
	}
}
