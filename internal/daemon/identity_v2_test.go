package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/identity"
)

// A node's first start incepts an agent identity (toad 1, KEL C-5): its icp names no witness yet
// (the first hub it joins becomes its witness), carries the time of inception, and is written as
// an identity file v2 that the next start reads back.
func TestDaemonInceptsAnAgentIdentity(t *testing.T) {
	l := NewLayout(t.TempDir())
	before := uint64(time.Now().UnixMilli())
	c, err := LoadOrGenerateIdentity(l)
	if err != nil {
		t.Fatal(err)
	}
	icp := c.KEL()[0].Event
	if c.Toad() != identity.ToadAgent || len(icp.Witnesses) != 0 || icp.Timestamp < before || icp.Timestamp > uint64(time.Now().UnixMilli()) {
		t.Fatalf("toad %d, icp %+v", c.Toad(), icp)
	}
	b, err := os.ReadFile(l.IdentityPath())
	if err != nil {
		t.Fatal(err)
	}
	f, err := identity.ParseIdentityFile(b)
	if err != nil || f.State != identity.StateOK || f.HotNext == nil {
		t.Fatalf("identity file %+v %v", f, err)
	}
	again, err := LoadOrGenerateIdentity(l)
	if err != nil || again.AID() != c.AID() {
		t.Fatalf("restart: %v, AID %s then %s", err, c.AID(), again.AID())
	}
	if ReadIdentityAID(l) != c.AID() {
		t.Fatal("ReadIdentityAID")
	}
}
