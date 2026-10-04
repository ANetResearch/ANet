package blackboard_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/wire"

	"github.com/ANetResearch/ANet/module/blackboard"
)

// CORE-S4: every key the unit's objects do not assign (the unassigned ones of 0-63, 64 and
// 65535), inserted with the value CBOR null into a valid encoding, is refused by Unmarshal with
// ErrUnitMalformed: in the wire form, in the body (signed by its author, so it is refused for the
// key and not for its signature) and in the body's stamp.
func TestCogUnitObjectsRefuseUnassignedKeys(t *testing.T) {
	r := wire.MustLoad()
	check := func(object string, valid []byte, decode func([]byte) error) {
		t.Helper()
		if err := decode(valid); err != nil {
			t.Fatalf("%s: the valid encoding is refused: %v", object, err)
		}
		negs, err := wire.Negatives(r, object, valid)
		if err != nil {
			t.Fatal(err)
		}
		if len(negs) < 50 {
			t.Fatalf("%s: only %d negatives", object, len(negs))
		}
		for _, n := range negs {
			if err := decode(n.Bytes); !errors.Is(err, blackboard.ErrUnitMalformed) {
				t.Errorf("%s with key %d (%s): %v, want ErrUnitMalformed", object, n.Key, n.Why, err)
			}
		}
	}

	suite := identity.SuiteController()
	u := &blackboard.CogUnit{TaskID: "task", Scope: "org", Type: "claim",
		Stamp: blackboard.HLC{Wall: 1767225600000, Logical: 1, NodeID: "n"}, Body: []byte("x"), BodyCID: "bafycid"}
	if err := u.Sign(suite); err != nil {
		t.Fatal(err)
	}
	w, err := u.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	check("CogUnitWire", w, func(b []byte) error { _, err := blackboard.Unmarshal(b); return err })
	body, err := u.CanonicalPreimage()
	if err != nil {
		t.Fatal(err)
	}
	check("CogUnitBody", body, func(b []byte) error { _, err := blackboard.Unmarshal(unitWire(t, b)); return err })
	stamp, err := coredet.Marshal(u.Stamp)
	if err != nil {
		t.Fatal(err)
	}
	check("HLC", stamp, func(b []byte) error {
		_, err := blackboard.Unmarshal(unitWire(t, rawMap(t, 0, blackboard.CogUnitType, 1, suite.AID(), 4, "claim", 5, rawCBOR(b))))
		return err
	})
}
