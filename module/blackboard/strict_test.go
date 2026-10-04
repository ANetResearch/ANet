package blackboard_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module/blackboard"
)

type rawCBOR []byte

func encOne(t *testing.T, v any) []byte {
	t.Helper()
	if r, ok := v.(rawCBOR); ok {
		return r
	}
	b, err := coredet.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawMap encodes the key/value pairs kv as one CBOR map in the order given (fewer than 24 pairs).
func rawMap(t *testing.T, kv ...any) []byte {
	t.Helper()
	out := []byte{0xa0 | byte(len(kv)/2)}
	for _, x := range kv {
		out = append(out, encOne(t, x)...)
	}
	return out
}

// unitWire wraps body in a CogUnitWire signed by the conformance identity, so that a body is
// refused for its encoding and not for its signature.
func unitWire(t *testing.T, body []byte) []byte {
	t.Helper()
	c := identity.SuiteController()
	sig, seq := c.Sign(body)
	return rawMap(t, 1, body, 2, &aobj.Envelope{SignerAID: c.AID(), KeyStateSeq: seq, Alg: aobj.AlgEdDSA, Sig: sig})
}

// Core CORE-K26, CORE-E2: a unit decodes only from exactly the bytes this package writes, and an
// unsigned unit is not written at all. Each case is signed by its author, so it is refused for its
// encoding. None of them reaches the board.
func TestCogUnitDecodeIsStrict(t *testing.T) {
	author := identity.SuiteController().AID()
	stamp := map[uint64]any{1: int64(1767225600000), 2: 1, 3: "n"}
	tag := blackboard.CogUnitType
	good := rawMap(t, 0, tag, 1, author, 2, "task", 3, "org", 4, "claim", 5, stamp, 6, []byte("x"))
	u, err := blackboard.Unmarshal(unitWire(t, good))
	if err != nil {
		t.Fatalf("the base case is refused: %v", err)
	}
	if u.Author != author || u.TaskID != "task" || u.Scope != "org" || u.Type != "claim" || u.Stamp.Logical != 1 || string(u.Body) != "x" {
		t.Fatalf("decoded %+v", u)
	}
	if _, err := (&blackboard.CogUnit{Author: author, Type: "claim", Stamp: blackboard.HLC{Wall: 1}}).Marshal(); !errors.Is(err, blackboard.ErrUnitUnsigned) {
		t.Fatalf("an unsigned unit is written: %v", err)
	}
	for name, body := range map[string][]byte{
		"no key 0":                rawMap(t, 1, author, 2, "task", 3, "org", 4, "claim", 5, stamp, 6, []byte("x")),
		"another tag":             rawMap(t, 0, "anet.cogunit/2", 1, author, 2, "task", 3, "org", 4, "claim", 5, stamp, 6, []byte("x")),
		"no stamp":                rawMap(t, 0, tag, 1, author, 2, "task", 3, "org", 4, "claim", 6, []byte("x")),
		"an empty body written":   rawMap(t, 0, tag, 1, author, 2, "task", 3, "org", 4, "claim", 5, stamp, 6, []byte{}),
		"logical 0 written":       rawMap(t, 0, tag, 1, author, 4, "claim", 5, map[uint64]any{1: int64(5), 2: 0}),
		"logical over 65535":      rawMap(t, 0, tag, 1, author, 4, "claim", 5, map[uint64]any{1: int64(5), 2: 65536}),
		"an unknown stamp key":    rawMap(t, 0, tag, 1, author, 4, "claim", 5, map[uint64]any{1: int64(5), 4: 0}),
		"keys out of order":       rawMap(t, 0, tag, 1, author, 3, "org", 2, "task", 4, "claim", 5, stamp),
		"key 4 twice":             rawMap(t, 0, tag, 1, author, 4, "claim", 4, "intent", 5, stamp),
		"an unregistered key 8":   rawMap(t, 0, tag, 1, author, 4, "claim", 5, stamp, 8, 0),
		"the 0.2 envelope key 8":  rawMap(t, 1, author, 4, "claim", 5, stamp, 8, map[uint64]any{1: author}),
		"text keys (a Go struct)": encOne(t, map[string]any{"Author": author}),
	} {
		got, err := blackboard.Unmarshal(unitWire(t, body))
		if !errors.Is(err, blackboard.ErrUnitMalformed) || got != nil {
			t.Errorf("%s: want ErrUnitMalformed, got %v", name, err)
		}
	}
	w := unitWire(t, good)
	var parts struct {
		Body []byte        `cbor:"1,keyasint"`
		Env  aobj.Envelope `cbor:"2,keyasint"`
	}
	if err := coredet.Unmarshal(w, &parts); err != nil {
		t.Fatal(err)
	}
	env := encOne(t, parts.Env)
	for name, wire := range map[string][]byte{
		"envelope first":      rawMap(t, 2, rawCBOR(env), 1, good),
		"no envelope":         rawMap(t, 1, good),
		"no body":             rawMap(t, 2, rawCBOR(env)),
		"an unregistered key": rawMap(t, 1, good, 2, rawCBOR(env), 3, 0),
		"body inline":         rawMap(t, 1, rawCBOR(good), 2, rawCBOR(env)),
	} {
		if _, err := blackboard.Unmarshal(wire); !errors.Is(err, blackboard.ErrUnitMalformed) {
			t.Errorf("wire %s: want ErrUnitMalformed, got %v", name, err)
		}
	}
}
