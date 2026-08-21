package inv1_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/adp"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module/blackboard"
	"github.com/ANetResearch/ANet/module/inv1"
)

// The runtime INV-1 guard rejects org-scoped objects on a commons publish path — by value, by pointer,
// and inside a slice — while public commons types (a gossip card, a TaskDoc) pass.
func TestGuardCommonsPublish(t *testing.T) {
	orgScoped := []any{
		blackboard.CogUnit{},
		&blackboard.CogUnit{},
		// org.Credential joins this list with the org module; the guard's
		// reach into slices is what matters and CogUnit exercises it.
		[]*blackboard.CogUnit{{}},
	}
	for _, v := range orgScoped {
		if err := inv1.GuardCommonsPublish(v); !errors.Is(err, inv1.ErrOrgScopedOnCommons) {
			t.Errorf("GuardCommonsPublish(%T) = %v, want ErrOrgScopedOnCommons", v, err)
		}
	}

	public := []any{
		nil,
		&adp.GossipCardMessage{},
		adp.GossipCardMessage{},
		&tsir.TaskDoc{},
		[]byte("encoded record"),
		"a plain string",
	}
	for _, v := range public {
		if err := inv1.GuardCommonsPublish(v); err != nil {
			t.Errorf("GuardCommonsPublish(%T) = %v, want nil (public/neutral type)", v, err)
		}
	}
}
