//go:build !no_x402

package x402

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// This node's chain of balance proofs: one line of liab_proof.jsonl per
// supply epoch checked, in the module's own state directory (HPP §3.5,
// 11-modularity §3.3). Only this module's audit reads it, so it is not part
// of the kernel's schema; the evidence event anet.supply.proof is what the
// node's own chain says of each check.

// liabProofFile is the file's name below the module's StateDir.
const liabProofFile = "liab_proof.jsonl"

// Results of one epoch's check. Any other value is the reason code of the
// finding the check made (balance_mismatch, proof_invalid, …).
const (
	resultOK      = "ok"
	resultExpired = "proof_expired"
)

// maxLiabRows bounds how many rows are read back; one epoch is one row.
const maxLiabRows = 1 << 16

// LiabProof is one row: what this node's hub said its balance was when the
// epoch closed, and what the check of it found.
type LiabProof struct {
	Hub         string `json:"hub"`
	ToDay       uint64 `json:"to_day"`
	FromDay     uint64 `json:"from_day"`
	Balance     uint64 `json:"balance"`
	Root        string `json:"root"`
	Outstanding uint64 `json:"outstanding"`
	LedgerHW    uint64 `json:"ledger_hw"`
	Record      string `json:"record,omitempty"`
	Result      string `json:"result"`
	CheckedAt   int64  `json:"checked_at"`
	// Finding is the first thing the check found, as it was told, when the
	// result is not ok.
	Finding *Finding `json:"finding,omitempty"`
}

// liabStore is the file, or only memory for a node that has no state
// directory. Rows are keyed by (hub, to_day).
type liabStore struct {
	mu   sync.Mutex
	path string
	rows []LiabProof
	read bool
}

func newLiabStore(dir string) *liabStore {
	s := &liabStore{}
	if dir != "" {
		s.path = filepath.Join(dir, liabProofFile)
	}
	return s
}

// load reads the file once. A line that does not parse is skipped: the file
// is this node's own and a torn last line must not stop the next check.
func (s *liabStore) load() {
	if s.read {
		return
	}
	s.read = true
	if s.path == "" {
		return
	}
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() && len(s.rows) < maxLiabRows {
		var r LiabProof
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Hub != "" {
			s.rows = append(s.rows, r)
		}
	}
}

// all returns the rows of one hub, oldest epoch first.
func (s *liabStore) all(hub string) []LiabProof {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var out []LiabProof
	for _, r := range s.rows {
		if r.Hub == hub {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ToDay < out[j].ToDay })
	return out
}

// put adds a row, or replaces the row of the same (hub, to_day), and writes
// the file whole through a temporary file, so a crash leaves the old one.
func (s *liabStore) put(r LiabProof) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	replaced := false
	for i := range s.rows {
		if s.rows[i].Hub == r.Hub && s.rows[i].ToDay == r.ToDay {
			s.rows[i], replaced = r, true
			break
		}
	}
	if !replaced {
		s.rows = append(s.rows, r)
	}
	if s.path == "" {
		return nil
	}
	var buf bytes.Buffer
	for _, row := range s.rows {
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("x402: writing %s: %w", liabProofFile, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
