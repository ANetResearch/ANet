package main

import (
	"encoding/json"
	"testing"
	"time"
)

// A literal is read exactly, whatever its size: the canonical form is the
// same for every spelling of one value, and integer-ness does not depend
// on how the literal is written.
func TestDecimalOf(t *testing.T) {
	cases := []struct {
		lit, canonical string
		integer        bool
	}{
		{"0", "0", true},
		{"-0", "0", true},
		{"0.000", "0", true},
		{"0e99999999999999999999", "0", true},
		{"1", "1e0", true},
		{"1.0", "1e0", true},
		{"10e-1", "1e0", true},
		{"0.1e1", "1e0", true},
		{"100", "1e2", true},
		{"1.50e3", "15e2", true},
		{"-2.5", "-25e-1", false},
		{"0.001", "1e-3", false},
		{"1e999999", "1e999999", true},
		{"10e999998", "1e999999", true},
		{"1E+2", "1e2", true},
		{"1e-999999", "1e-999999", false},
		{"123456789012345678901234567890", "12345678901234567890123456789e1", true},
		{"1e99999999999999999999", "1e99999999999999999999", true},
	}
	for _, c := range cases {
		d, ok := decimalOf(c.lit)
		if !ok {
			t.Errorf("%s: not read", c.lit)
			continue
		}
		if got := d.canonical(); got != c.canonical {
			t.Errorf("%s: canonical %s, want %s", c.lit, got, c.canonical)
		}
		if d.isInteger() != c.integer {
			t.Errorf("%s: integer %v, want %v", c.lit, d.isInteger(), c.integer)
		}
	}
	for _, bad := range []string{"", "-", ".5", "1.", "1e", "1e+", "1e-", "1e++2", "1.2.3", "0x10", "1e2.5", "abc"} {
		if _, ok := decimalOf(bad); ok {
			t.Errorf("%q must not read as a number", bad)
		}
	}
}

// ratOf computes only with numbers of bounded size, and refuses the rest
// at once: big.Rat.SetString("1e999999") alone takes tens of milliseconds.
func TestRatOfIsBounded(t *testing.T) {
	for lit, want := range map[string]string{"0": "0", "-0.5": "-1/2", "1e3": "1000", "2.50": "5/2", "1e-3": "1/1000"} {
		r, ok := ratOf(json.Number(lit))
		if !ok || r.RatString() != want {
			t.Errorf("%s: %v %v, want %s", lit, r, ok, want)
		}
	}
	began := time.Now()
	for _, lit := range []string{"1e999999", "1e-999999", "1e1001", "1e99999999999999999999"} {
		for i := 0; i < 1000; i++ {
			if _, ok := ratOf(json.Number(lit)); ok {
				t.Fatalf("%s must be refused", lit)
			}
		}
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("refusing 4000 huge literals took %v", d)
	}
	if _, ok := ratOf(json.Number("1e1000")); !ok {
		t.Error("1e1000 is within the bound")
	}
}
