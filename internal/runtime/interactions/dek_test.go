package interactions

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// LOCAL-1 (core §5.2): golden/py/gen_v016.py in ANetCore writes testdata/vec-local-1.json from its
// own implementation of core L35 and L36; every value is recomputed here with this package's codec
// and must agree byte for byte. The client key and client digest of the same vector (L37, L38) are
// UX-1's part.

type vecLocalField struct {
	Column    string `json:"column"`
	Kind      string `json:"kind"`
	Plaintext string `json:"plaintext"`
	Nonce     string `json:"nonce"`
	AAD       string `json:"aad"`
	Stored    string `json:"stored"`
}

type vecLocal1 struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
	IX     string            `json:"ix"`
	IXDEK  string            `json:"ix_dek"`
	Keys   map[string]struct {
		Info string `json:"info"`
		Salt string `json:"salt"`
		Key  string `json:"key"`
	} `json:"keys"`
	Fields    []vecLocalField `json:"fields"`
	Empty     vecLocalField   `json:"empty"`
	Negatives []struct {
		Name   string `json:"name"`
		Note   string `json:"note"`
		IX     string `json:"ix"`
		Column string `json:"column"`
		Stored string `json:"stored"`
	} `json:"negatives"`
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex %q: %v", s, err)
	}
	return b
}

func loadLocal1(t *testing.T) *vecLocal1 {
	t.Helper()
	b, err := os.ReadFile("testdata/vec-local-1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vecLocal1
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.ID != "LOCAL-1" {
		t.Fatalf("vector id %q", v.ID)
	}
	return &v
}

func TestLOCAL1(t *testing.T) {
	v := loadLocal1(t)
	if v.Labels["field"] != localFieldLabel || v.Labels["aead_key"] != localAEADInfo || v.Labels["mac_key"] != localMACInfo {
		t.Fatalf("labels %v", v.Labels)
	}
	dek, err := dekFrom(unhex(t, v.IXDEK))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(dek.aeadKey()); got != v.Keys["aead_key"].Key {
		t.Errorf("aead_key = %s, want %s", got, v.Keys["aead_key"].Key)
	}
	if got := hex.EncodeToString(dek.MACKey()); got != v.Keys["mac_key"].Key {
		t.Errorf("mac_key = %s, want %s", got, v.Keys["mac_key"].Key)
	}
	box, err := dek.box(v.IX)
	if err != nil {
		t.Fatal(err)
	}

	// One field per encrypted column, in FS-L2's order: a column added to the store without a vector
	// row, or a vector row for a column the store does not encrypt, fails here.
	cols := EncryptedColumns()
	if len(v.Fields) != len(cols) {
		t.Fatalf("vector has %d fields, the store encrypts %d columns", len(v.Fields), len(cols))
	}
	for i, f := range append(v.Fields, v.Empty) {
		col := Column(f.Column)
		if i < len(cols) && col != cols[i] {
			t.Errorf("field %d is %s, want %s", i, col, cols[i])
		}
		aad, err := fieldAAD(v.IX, col)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(aad); got != f.AAD {
			t.Errorf("%s: aad = %s, want %s", col, got, f.AAD)
		}
		pt := unhex(t, f.Plaintext)
		stored, err := box.sealFieldNonce(col, unhex(t, f.Nonce), pt)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(stored); got != f.Stored {
			t.Errorf("%s: stored = %s, want %s", col, got, f.Stored)
		}
		back, err := dek.OpenField(v.IX, col, unhex(t, f.Stored))
		if err != nil || !bytes.Equal(back, pt) {
			t.Errorf("%s: open = %x, %v; want %x", col, back, err, pt)
		}
	}
	if len(v.Negatives) == 0 {
		t.Fatal("no negatives")
	}
	for _, n := range v.Negatives {
		if pt, err := dek.OpenField(n.IX, Column(n.Column), unhex(t, n.Stored)); err == nil {
			t.Errorf("%s (%s): opened to %x", n.Name, n.Note, pt)
		}
	}
}

// The codec round-trips, binds every field to its task and column, and never reuses a nonce.
func TestFieldCodec(t *testing.T) {
	k, err := NewDEK()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewDEK()
	pts := [][]byte{{}, []byte("x"), bytes.Repeat([]byte("content "), 5000)}
	for _, col := range EncryptedColumns() {
		for _, pt := range pts {
			a, err := k.SealField("ix_a", col, pt)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := k.SealField("ix_a", col, pt)
			if len(a) != len(pt)+FieldOverhead || a[0] != fieldVersion {
				t.Fatalf("%s: stored %d bytes, version %#x", col, len(a), a[0])
			}
			if bytes.Equal(a[1:1+fieldNonceSize], b[1:1+fieldNonceSize]) {
				t.Fatalf("%s: two seals used one nonce", col)
			}
			if got, err := k.OpenField("ix_a", col, a); err != nil || !bytes.Equal(got, pt) {
				t.Fatalf("%s: round trip %v", col, err)
			}
			if _, err := k.OpenField("ix_b", col, a); !errors.Is(err, errFieldCorrupt) {
				t.Fatalf("%s: opened as another task's: %v", col, err)
			}
			if _, err := other.OpenField("ix_a", col, a); !errors.Is(err, errFieldCorrupt) {
				t.Fatalf("%s: opened under another key: %v", col, err)
			}
			for _, oc := range EncryptedColumns() {
				if oc == col {
					continue
				}
				if _, err := k.OpenField("ix_a", oc, a); !errors.Is(err, errFieldCorrupt) {
					t.Fatalf("%s: opened as %s: %v", col, oc, err)
				}
			}
		}
	}
	for name, c := range map[string]struct {
		ix  string
		col Column
	}{
		"no task":          {"", ColGoal},
		"task not UTF-8":   {"ix_\xff", ColGoal},
		"plain column":     {"ix_a", "interaction.receipt"},
		"unnamed column":   {"ix_a", ""},
		"table only":       {"ix_a", "interaction"},
		"column of a peer": {"ix_a", "interaction.peer_kel"},
	} {
		if _, err := k.SealField(c.ix, c.col, []byte("x")); !errors.Is(err, ErrBadInput) {
			t.Errorf("seal, %s: %v, want ErrBadInput", name, err)
		}
		if _, err := k.OpenField(c.ix, c.col, make([]byte, FieldOverhead)); !errors.Is(err, ErrBadInput) {
			t.Errorf("open, %s: %v, want ErrBadInput", name, err)
		}
	}
	if _, err := dekFrom(make([]byte, DEKSize-1)); err == nil {
		t.Fatal("a 31-byte data key was accepted")
	}
	// CORE-L2: the data key is a root only; the two derivations differ from it and from each other.
	raw := k.k[:]
	if bytes.Equal(k.aeadKey(), raw) || bytes.Equal(k.MACKey(), raw) || bytes.Equal(k.aeadKey(), k.MACKey()) {
		t.Fatal("aead_key, mac_key and ix_dek are not three different keys")
	}
	k.Wipe()
	if k.k != [DEKSize]byte{} {
		t.Fatal("Wipe left key bytes")
	}
}

// FuzzOpenField: any stored bytes, under any column and task, either fail to open with
// errFieldCorrupt or ErrBadInput, or open to a plaintext that seals back to the same bytes under the
// same nonce (so nothing but a field the codec wrote opens). A sealed fuzz plaintext always opens.
func FuzzOpenField(f *testing.F) {
	v := struct {
		IX     string `json:"ix"`
		IXDEK  string `json:"ix_dek"`
		Fields []struct {
			Column string `json:"column"`
			Stored string `json:"stored"`
		} `json:"fields"`
	}{}
	b, err := os.ReadFile("testdata/vec-local-1.json")
	if err != nil {
		f.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		f.Fatal(err)
	}
	keyBytes, err := hex.DecodeString(v.IXDEK)
	if err != nil {
		f.Fatal(err)
	}
	cols := EncryptedColumns()
	for i, fl := range v.Fields {
		s, _ := hex.DecodeString(fl.Stored)
		f.Add(s, uint8(i), v.IX, []byte("pt"))
		f.Add(s[:len(s)-1], uint8(i), v.IX, []byte{})
	}
	f.Add([]byte{0x01}, uint8(0), "ix", []byte(nil))
	f.Add([]byte{}, uint8(3), "", []byte("x"))
	f.Fuzz(func(t *testing.T, stored []byte, ci uint8, ix string, pt []byte) {
		k, err := dekFrom(keyBytes)
		if err != nil {
			t.Fatal(err)
		}
		col := cols[int(ci)%len(cols)]
		got, err := k.OpenField(ix, col, stored)
		if err != nil {
			if !errors.Is(err, errFieldCorrupt) && !errors.Is(err, ErrBadInput) {
				t.Fatalf("open: unexpected error %v", err)
			}
		} else {
			box, err := k.box(ix)
			if err != nil {
				t.Fatal(err)
			}
			again, err := box.sealFieldNonce(col, stored[1:1+fieldNonceSize], got)
			if err != nil || !bytes.Equal(again, stored) {
				t.Fatalf("opened %x to %x, which seals back to %x (%v)", stored, got, again, err)
			}
		}
		sealed, err := k.SealField(ix, col, pt)
		if err != nil {
			if !errors.Is(err, ErrBadInput) {
				t.Fatalf("seal: %v", err)
			}
			return
		}
		back, err := k.OpenField(ix, col, sealed)
		if err != nil || !bytes.Equal(back, pt) {
			t.Fatalf("round trip of %x: %x, %v", pt, back, err)
		}
	})
}

// A field sealed with the data key itself as the AES key — what an implementation that skips
// HKDF would write (FS mutation 31) — does not open: the field key is derived.
func TestRawDEKIsNotTheFieldKey(t *testing.T) {
	k, _ := NewDEK()
	blk, _ := aes.NewCipher(k.k[:])
	g, _ := cipher.NewGCM(blk)
	aad, _ := fieldAAD("ix_a", ColGoal)
	nonce := make([]byte, fieldNonceSize)
	stored := append([]byte{fieldVersion}, nonce...)
	stored = g.Seal(stored, nonce, []byte("goal"), aad)
	if _, err := k.OpenField("ix_a", ColGoal, stored); !errors.Is(err, errFieldCorrupt) {
		t.Fatalf("a field sealed under ix_dek itself opened: %v", err)
	}
}
