//go:build !no_x402

package x402

import (
	"os"
	"path/filepath"
	"testing"
)

// The proof chain is read back from a file the module wrote; a torn or
// damaged file must cost its bad lines and nothing else.
func FuzzLiabProofFile(f *testing.F) {
	f.Add([]byte(`{"hub":"h","to_day":1,"result":"ok"}` + "\n"))
	f.Add([]byte("{\"hub\":\"h\",\"to_day\":18446744073709551615,\"balance\":-1}\n{"))
	f.Add([]byte("\x00\xff\n\n{}\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, liabProofFile), b, 0o600); err != nil {
			t.Skip()
		}
		s := newLiabStore(dir)
		rows := s.all("h")
		for _, r := range rows {
			if r.Hub != "h" {
				t.Fatalf("a row of another hub: %+v", r)
			}
		}
		if err := s.put(LiabProof{Hub: "h", ToDay: 7, Result: resultOK}); err != nil {
			t.Fatal(err)
		}
		if got := newLiabStore(dir).all("h"); len(got) == 0 {
			t.Fatal("the row just written is gone")
		}
	})
}

// The file is private, and a torn last line costs only that line.
func TestTheProofFileIsPrivateAndToleratesATornLine(t *testing.T) {
	dir := t.TempDir()
	s := newLiabStore(dir)
	if err := s.put(LiabProof{Hub: "h", ToDay: 1, Result: resultOK}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, liabProofFile)
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.WriteString(`{"hub":"h","to_day":2,"res`)
	fh.Close()
	rows := newLiabStore(dir).all("h")
	if len(rows) != 1 || rows[0].ToDay != 1 {
		t.Errorf("rows: %+v", rows)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
}
