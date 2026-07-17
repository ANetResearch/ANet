package identity_test

import (
	"testing"

	"github.com/ANetResearch/ANet/internal/protocol/identity"
)

// An object signed by a key that was later rotated away is honored only if it can be established to
// predate the retirement: rejected when the rotation carried no timestamp, and gated by the grace
// window when it did. (Closes the rotation-gate gap where a no-timestamp rotation left an old key able
// to sign new objects with a future msg_time.)
func TestRotationGate(t *testing.T) {
	obj := []byte("an object")

	// (1) rotation with NO timestamp → an old-key object with any msg_time is rejected.
	c, _ := identity.Incept()
	sig0, seq0 := c.Sign(obj)
	if err := c.Rotate(0); err != nil { // no clock
		t.Fatal(err)
	}
	if err := identity.VerifyObject(c.KEL(), c.AID(), seq0, 1000, obj, sig0); err == nil {
		t.Fatal("old key + untimestamped rotation + msg_time>0 must be REJECTED")
	}

	// (2) rotation WITH a timestamp → grace window: honored before, rejected at/after.
	c2, _ := identity.Incept()
	s0, q0 := c2.Sign(obj)
	if err := c2.Rotate(5000); err != nil {
		t.Fatal(err)
	}
	if err := identity.VerifyObject(c2.KEL(), c2.AID(), q0, 4000, obj, s0); err != nil {
		t.Fatalf("object bound before retirement must be honored: %v", err)
	}
	if err := identity.VerifyObject(c2.KEL(), c2.AID(), q0, 6000, obj, s0); err == nil {
		t.Fatal("object bound at/after retirement must be REJECTED")
	}

	// (3) the CURRENT key still verifies normally.
	cs, cq := c2.Sign(obj)
	if err := identity.VerifyObject(c2.KEL(), c2.AID(), cq, 7000, obj, cs); err != nil {
		t.Fatalf("current key must verify: %v", err)
	}
}
