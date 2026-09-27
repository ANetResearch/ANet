package main

import (
	"encoding/json"
	"math/big"
	"strings"
)

// JSON numbers are read two ways here. Equality and "is it an integer" are
// decided on the literal itself (decimalOf), which is exact for any number
// and costs time proportional to its text. Arithmetic (minimum, multipleOf,
// ...) needs a big.Rat, and big.Rat.SetString of "1e999999" builds a
// 3.3-million-bit integer: tens of milliseconds for a nine-byte literal,
// so a 64 KiB argument of them held a core for minutes. ratOf therefore
// refuses numbers beyond maxRatDigits and maxRatExponent, and its callers
// report such a number as one they cannot evaluate rather than guess.

const (
	// maxRatDigits bounds the significant digits ratOf accepts.
	maxRatDigits = 400
	// maxRatExponent bounds the decimal exponent ratOf accepts (after
	// normalisation), so a value is at most about 4,700 bits.
	maxRatExponent = 1000
)

// decimal is a JSON number literal as sign × digits × 10^exp, with no
// leading or trailing zeros in digits; zero is {digits: "0", exp: 0}.
type decimal struct {
	neg    bool
	digits string
	exp    *big.Int
}

// decimalOf reads a JSON number literal. The exponent is a big.Int because
// a literal may carry any number of exponent digits.
func decimalOf(s string) (decimal, bool) {
	d := decimal{exp: new(big.Int)}
	if strings.HasPrefix(s, "-") {
		d.neg, s = true, s[1:]
	}
	intPart, rest := s, ""
	if i := strings.IndexAny(s, ".eE"); i >= 0 {
		intPart, rest = s[:i], s[i:]
	}
	frac := ""
	if strings.HasPrefix(rest, ".") {
		rest = rest[1:]
		i := strings.IndexAny(rest, "eE")
		if i < 0 {
			i = len(rest)
		}
		frac, rest = rest[:i], rest[i:]
		if frac == "" {
			return d, false
		}
	}
	if rest != "" {
		if rest[0] != 'e' && rest[0] != 'E' {
			return d, false
		}
		e := rest[1:]
		if strings.HasPrefix(e, "+") {
			e = e[1:]
		}
		if e == "" || e == "-" || strings.HasPrefix(e, "+") {
			return d, false
		}
		if _, ok := d.exp.SetString(e, 10); !ok {
			return d, false
		}
	}
	if intPart == "" || !allDigits(intPart) || !allDigits(frac) {
		return d, false
	}
	digits := strings.TrimLeft(intPart+frac, "0")
	d.exp.Sub(d.exp, big.NewInt(int64(len(frac))))
	if digits == "" {
		return decimal{digits: "0", exp: new(big.Int)}, true
	}
	trimmed := strings.TrimRight(digits, "0")
	d.exp.Add(d.exp, big.NewInt(int64(len(digits)-len(trimmed))))
	d.digits = trimmed
	return d, true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (d decimal) isZero() bool    { return d.digits == "0" }
func (d decimal) isInteger() bool { return d.isZero() || d.exp.Sign() >= 0 }

// canonical renders the value so that two literals of one value render
// alike: "1", "1.0", "10e-1" and "0.1e1" all become "1e0"; -0 is 0.
func (d decimal) canonical() string {
	if d.isZero() {
		return "0"
	}
	s := d.digits + "e" + d.exp.String()
	if d.neg {
		return "-" + s
	}
	return s
}

// ratOf returns a JSON number as an exact rational, or false when it is
// not a number or is too large or too precise to compute with (see
// maxRatDigits, maxRatExponent).
func ratOf(v any) (*big.Rat, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return nil, false
	}
	d, ok := decimalOf(string(n))
	if !ok {
		return nil, false
	}
	if d.isZero() {
		return new(big.Rat), true
	}
	if len(d.digits) > maxRatDigits || !d.exp.IsInt64() {
		return nil, false
	}
	if e := d.exp.Int64(); e > maxRatExponent || e < -maxRatExponent {
		return nil, false
	}
	lit := d.digits + "e" + d.exp.String()
	if d.neg {
		lit = "-" + lit
	}
	return new(big.Rat).SetString(lit)
}

// numberIsInteger reports whether a JSON number literal has an integer
// value, however large.
func numberIsInteger(n json.Number) bool {
	d, ok := decimalOf(string(n))
	return ok && d.isInteger()
}
