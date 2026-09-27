package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyLayout copies the identity and the evidence chain of l into a new
// data directory, so a test can damage the copy.
func copyLayout(t *testing.T, l Layout, edit func([][]byte) [][]byte) Layout {
	t.Helper()
	dst := NewLayout(t.TempDir())
	id, err := os.ReadFile(l.IdentityPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst.IdentityPath(), id, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(l.EvidenceLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := edit(splitLines(b))
	if err := os.WriteFile(dst.EvidenceLedgerPath(), append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

// ReadEvidenceChain returns what the daemon wrote, verified; a record
// altered, dropped or reordered anywhere is refused; an incomplete last line
// is left out and reported, as the daemon itself treats it.
func TestReadEvidenceChainVerifiesTheWholeChain(t *testing.T) {
	d := newBareDaemon(t)
	base := d.ledger.nextSeq
	for i := 0; i < 4; i++ {
		if _, err := d.ledger.Append(EvPolicyChanged, map[string]any{"field": "test", "n": i}); err != nil {
			t.Fatal(err)
		}
	}
	ch, err := ReadEvidenceChain(d.layout)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := d.ledger.Evidence(EvidenceQuery{})
	if ch.Head.HeadID != head.HeadID || ch.Head.Length != base+4 || ch.SignerAID != d.AID() || ch.TornTail {
		t.Fatalf("read %+v, daemon head %+v", ch.Head, head)
	}
	if len(ch.Records) != int(ch.Head.Length) || len(ch.Lines) != len(ch.Records) {
		t.Fatalf("%d records, %d lines", len(ch.Records), len(ch.Lines))
	}

	keep := func(ls [][]byte) [][]byte { return ls }
	if _, err := ReadEvidenceChain(copyLayout(t, d.layout, keep)); err != nil {
		t.Fatalf("an unaltered copy: %v", err)
	}
	damage := map[string]func([][]byte) [][]byte{
		"a record dropped": func(ls [][]byte) [][]byte { return append(ls[:1:1], ls[2:]...) },
		"two records swapped": func(ls [][]byte) [][]byte {
			ls[1], ls[2] = ls[2], ls[1]
			return ls
		},
		"a record altered": func(ls [][]byte) [][]byte {
			ls[1] = bytes.Replace(ls[1], ls[1][len(ls[1])/2:len(ls[1])/2+1], []byte{'A' + (ls[1][len(ls[1])/2]-'A'+1)%26}, 1)
			return ls
		},
		"a line of garbage in the middle": func(ls [][]byte) [][]byte {
			return append(ls[:2:2], append([][]byte{[]byte("not a record")}, ls[2:]...)...)
		},
	}
	for name, edit := range damage {
		if _, err := ReadEvidenceChain(copyLayout(t, d.layout, edit)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	torn, err := ReadEvidenceChain(copyLayout(t, d.layout, func(ls [][]byte) [][]byte {
		return append(ls, []byte("oWFpZ"))
	}))
	if err != nil || !torn.TornTail || torn.Head.HeadID != head.HeadID {
		t.Fatalf("torn tail: %v %+v", err, torn)
	}
}

// Without an identity there is no chain to read, and the error says why.
func TestReadEvidenceChainWithoutIdentity(t *testing.T) {
	_, err := ReadEvidenceChain(NewLayout(filepath.Join(t.TempDir(), "x")))
	if err == nil || !strings.Contains(err.Error(), "no identity") {
		t.Fatalf("err = %v", err)
	}
}
