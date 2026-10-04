package blackboard_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module/blackboard"
)

// FuzzUnmarshalCogUnit: the strict decoder of a transported unit (core CORE-K26) never panics, and
// what it accepts is a unit as this package writes it: Marshal gives back the same bytes, and
// verifying it does not panic. Seeds: the MODOBJ-1 unit and its negatives.
func FuzzUnmarshalCogUnit(f *testing.F) {
	b, err := os.ReadFile("testdata/vec-cogunit-1.json")
	if err != nil {
		f.Fatal(err)
	}
	var v unitVector
	if err := json.Unmarshal(b, &v); err != nil {
		f.Fatal(err)
	}
	f.Add([]byte(v.Unit.Wire))
	for _, n := range v.Negatives {
		f.Add([]byte(n.Wire))
	}
	f.Add([]byte{0xa0})
	kel, err := identity.UnmarshalKEL(v.Author.KEL)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		u, err := blackboard.Unmarshal(raw)
		if err != nil {
			if !errors.Is(err, blackboard.ErrUnitMalformed) {
				t.Fatalf("a refusal that is not ErrUnitMalformed: %v", err)
			}
			return
		}
		again, err := u.Marshal()
		if err != nil || !bytes.Equal(again, raw) {
			t.Fatalf("accepted bytes that are not the unit's encoding (%v)\n in  %x\n out %x", err, raw, again)
		}
		if _, err := u.ID(); err != nil {
			t.Fatalf("an accepted unit has no id: %v", err)
		}
		_ = u.Verify(kel, v.Unit.VerifyAt)
	})
}
