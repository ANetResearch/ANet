package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/ael"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/daemon"
)

type testEvent struct {
	typ     string
	payload map[string]any
}

// writeChain gives a data directory an identity and an evidence chain of
// events, signed and stored the way the daemon stores them.
func writeChain(t *testing.T, events []testEvent) daemon.Layout {
	t.Helper()
	layout := daemon.NewLayout(t.TempDir())
	c, err := identity.Incept()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.IdentityPath(), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	prev := ael.GenesisPrev()
	at := time.Now().Add(-time.Hour).UnixMilli()
	for i, ev := range events {
		rec := &ael.EventRecord{ChainDID: c.DID(), Seq: uint64(i), PrevID: prev, EventType: ev.typ,
			VersionMajor: ael.VersionMajor2, Payload: ev.payload, Timestamp: at + int64(i)}
		if err := rec.Sign(c); err != nil {
			t.Fatal(err)
		}
		b, err := coredet.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		file.WriteString(base64.StdEncoding.EncodeToString(b) + "\n")
		prev = rec.ID
	}
	if err := os.WriteFile(layout.EvidenceLedgerPath(), file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return layout
}

var auditEvents = []testEvent{
	{"anet.delegation.sent", map[string]any{"interaction_id": "ix_a", "provider_aid": "bafyreiprovider0001", "request_cid": "c1"}},
	{"anet.result.accepted", map[string]any{"interaction_id": "ix_a", "result_cid": "r1", "receipt_verified": false, "state": "completed"}},
	{"anet.result.accepted", map[string]any{"interaction_id": "ix_b", "result_cid": "r2", "receipt_verified": true, "state": "completed"}},
	{"anet.capability.effect", map[string]any{"interaction_id": "ix_c", "capability": "text.stats", "caller_aid": "bafyreicaller0001", "status": "UNVERIFIED"}},
	{"anet.capability.effect", map[string]any{"interaction_id": "ix_d", "capability": "text.stats", "caller_aid": "bafyreicaller0001", "status": "OK"}},
	{"anet.payment.settled", map[string]any{"interaction_id": "ix_a", "amount": 3, "verified": false}},
	{"anet.future.thing", map[string]any{"interaction_id": "ix_e", "opaque": "kept as is"}},
	{"anet.policy.changed", map[string]any{"field": "peers.allow", "to": "bafyreiprovider0001"}},
}

// The display rules of A2A-DESIGN §14: receipt_verified=false is "could not
// verify", UNVERIFIED is not a success, an unknown event is listed with its
// payload, and the source is named.
func TestAuditAppliesTheDisplayRules(t *testing.T) {
	layout := writeChain(t, auditEvents)
	ch, err := daemon.ReadEvidenceChain(layout)
	if err != nil {
		t.Fatal(err)
	}
	rep := buildAudit(ch, layout.EvidenceLedgerPath(), auditFilter{})
	s := rep.Summary
	if s.Events != len(auditEvents) || s.CapabilitySucceeded != 1 || s.CapabilityByStatus["UNVERIFIED"] != 1 ||
		s.ResultsReceiptVerified != 1 || s.ResultsReceiptUnverified != 1 || s.PaymentsUnverified != 1 || s.Unknown != 1 {
		t.Fatalf("summary %+v", s)
	}
	for _, ev := range rep.Events {
		switch {
		case ev.Type == "anet.capability.effect" && ev.Status == "UNVERIFIED" && (ev.Success == nil || *ev.Success):
			t.Errorf("UNVERIFIED counted as success: %+v", ev)
		case ev.Type == "anet.future.thing" && (ev.Known || ev.Payload["opaque"] != "kept as is"):
			t.Errorf("unknown event not listed as recorded: %+v", ev)
		case ev.Source == "":
			t.Errorf("event without a source: %+v", ev)
		}
	}
	var out bytes.Buffer
	renderAudit(&out, rep)
	text := out.String()
	for _, want := range []string{"could not verify (未能核验)", "status UNVERIFIED (not counted as success)",
		"unknown event, as recorded", "kept as is", "source: " + layout.EvidenceLedgerPath(),
		"Events — source:", "Summary — source:", "succeeded (OK) 1 of 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("audit output lacks %q:\n%s", want, text)
		}
	}

	byIX := buildAudit(ch, "", auditFilter{interaction: "ix_a"})
	byPeer := buildAudit(ch, "", auditFilter{peer: "bafyreiprovider0001"})
	if byIX.Summary.Events != 3 || byPeer.Summary.Events != 2 {
		t.Fatalf("filters: interaction %d events, peer %d", byIX.Summary.Events, byPeer.Summary.Events)
	}
	since := buildAudit(ch, "", auditFilter{since: time.Now().UnixMilli()})
	if since.Summary.Events != 0 {
		t.Fatalf("--since now kept %d events", since.Summary.Events)
	}
}

// An export verifies with nothing but its directory; altering, truncating or
// losing the manifest's head is caught; --head pins a head learned
// elsewhere; the export refuses to overwrite.
func TestAuditExportVerifiesWithVerifyChain(t *testing.T) {
	layout := writeChain(t, auditEvents)
	ch, err := daemon.ReadEvidenceChain(layout)
	if err != nil {
		t.Fatal(err)
	}
	export := func() string {
		dir := filepath.Join(t.TempDir(), "export")
		if err := exportChain(dir, ch, buildAudit(ch, "", auditFilter{})); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	dir := export()
	if err := verifyChain(dir, "", "", ""); err != nil {
		t.Fatalf("a fresh export: %v", err)
	}
	if err := verifyChain(dir, "", "", ch.Head.HeadID); err != nil {
		t.Fatalf("with its own head: %v", err)
	}
	if err := verifyChain(dir, "", "", "bafyreinotarecord"); !errors.Is(err, errQuiet) {
		t.Fatalf("with a head it does not contain: %v", err)
	}
	if err := exportChain(dir, ch, buildAudit(ch, "", auditFilter{})); err == nil {
		t.Fatal("export overwrote an existing export")
	}
	if fi, _ := os.Stat(filepath.Join(dir, exportChainFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("export mode %v", fi.Mode().Perm())
	}

	edits := map[string]func(lines []string) []string{
		"last record cut":  func(l []string) []string { return l[:len(l)-1] },
		"records swapped":  func(l []string) []string { l[1], l[2] = l[2], l[1]; return l },
		"a record dropped": func(l []string) []string { return append(l[:3:3], l[4:]...) },
	}
	for name, edit := range edits {
		d := export()
		p := filepath.Join(d, exportChainFile)
		b, _ := os.ReadFile(p)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if err := os.WriteFile(p, []byte(strings.Join(edit(lines), "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyChain(d, "", "", ""); !errors.Is(err, errQuiet) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A different key history: the chain is not that AID's.
	other, _ := identity.Incept()
	kb, _ := identity.MarshalKEL(other.KEL())
	if err := verifyChain(export(), base64.StdEncoding.EncodeToString(kb), "", ""); !errors.Is(err, errQuiet) {
		t.Errorf("under another AID's key history: %v", err)
	}
}
