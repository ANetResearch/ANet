package x402a2a

import (
	"errors"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/wire"
)

// TestPreimageRejectsNUL (core CORE-E9, ANet). PayBind (L34) separates the interaction id and the
// task nonce with 0x00 (CORE-L1), so a field holding U+0000 could move the boundary: ("a\0b", "c")
// and ("a", "b\0c") would bind alike, and a payment for one task would pass for the other. PayBind
// refuses such a field, as a second line behind the decoders (the inner ix at the seal layer, the
// TaskDoc's anet.nonce at the provider's step 9).
//
// One row per registered guard of this repository (registry nul_guard). A row's function puts
// field into each 0x00-separated text position it has and returns the function's error.
func TestPreimageRejectsNUL(t *testing.T) {
	rows := map[string]func(field string) error{
		"L34": nulPayBind, // PayBind: ix, taskNonce
	}
	r, err := wire.Load()
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, g := range r.NUL {
		if g.Repo != "ANet" {
			continue
		}
		listed[g.Label] = true
		fn, ok := rows[g.Label]
		if !ok {
			t.Errorf("registry nul_guard %s (%s) has no row here", g.Label, g.What)
			continue
		}
		t.Run(g.Label+" "+g.What, func(t *testing.T) {
			for _, f := range []string{"\x00", "a\x00", "\x00b", "a\x00b"} {
				if err := fn(f); err == nil {
					t.Errorf("%q accepted", f)
				}
			}
			if err := fn("ab"); err != nil {
				t.Errorf("a field without U+0000 refused: %v", err)
			}
		})
	}
	for label := range rows {
		if !listed[label] {
			t.Errorf("row %s is not a registered ANet nul_guard", label)
		}
	}
}

// nulPayBind puts field in each of PayBind's two positions: the error when either accepts it.
func nulPayBind(field string) error {
	if _, err := PayBind(field, "nonce"); err == nil {
		return nil
	}
	_, err := PayBind("ix-1", field)
	return err
}

// The two NUL inputs of the golden vector EV2-1 (ANetCore evidence/testdata/vec-ev2-1.json,
// nul_refused): the same bytes with the boundary moved. Both are refused, with ErrBindNUL and no
// binding; the clean task of the same vector binds to the value the vector states.
func TestPayBindRefusesTheEV2NULInputs(t *testing.T) {
	for _, in := range [][2]string{{"a\x00b", "c"}, {"a", "b\x00c"}} {
		got, err := PayBind(in[0], in[1])
		if !errors.Is(err, ErrBindNUL) || got != "" {
			t.Errorf("PayBind(%q, %q) = %q, %v; want no binding and ErrBindNUL", in[0], in[1], got, err)
		}
	}
	// EV2-1's task: ix "ix-golden-1", anet.nonce as written; pay_bind.hex of the vector.
	got, err := PayBind("ix-golden-1", "xklyE6aI1TJ-5H9CwongLQ")
	if err != nil || got != "750f7bb132a730cd9417a6e78437ce7f1f54325c8d34582c62724a39f52a46ad" {
		t.Fatalf("PayBind of the EV2-1 task = %s (%v)", got, err)
	}
	if strings.Contains(ErrBindNUL.Error(), "\x00") {
		t.Fatal("the error text carries a NUL")
	}
}
