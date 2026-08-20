package inv2_test

import (
	"errors"
	"testing"

	"github.com/ANetResearch/ANet/module/inv2"
)

func TestScreenCatchesALeakedToken(t *testing.T) {
	orgID := "bafyreicaw2uaernqv2rnhunxgkjwgsxrundhy4o2f2v423i27bk6yjh7ge"
	// The realistic shape: nobody adds an org id to a public field on
	// purpose. It arrives inside prose an agent wrote about itself.
	profile := []byte(`{"summary":"I coordinate work for ` + orgID + `","readme":""}`)
	if err := inv2.Screen(profile, []string{orgID}); !errors.Is(err, inv2.ErrLeak) {
		t.Errorf("a leaked org id must be refused, got %v", err)
	}
	clean := []byte(`{"summary":"I translate documents","readme":""}`)
	if err := inv2.Screen(clean, []string{orgID}); err != nil {
		t.Errorf("an ordinary profile must publish, got %v", err)
	}
	// No org, nothing forbidden, nothing to refuse.
	if err := inv2.Screen(profile, nil); err != nil {
		t.Errorf("a node in no org must not be blocked: %v", err)
	}
	if err := inv2.Screen(profile, []string{""}); err != nil {
		t.Errorf("an empty token must not match everything: %v", err)
	}
}

// The structural half. A publishable type must have nowhere to put org
// data, at any depth — which is how the leak would really arrive: someone
// adds an OrgID field because it was convenient, and every node starts
// publishing one.
func TestPublishableTypesHaveNoOrgField(t *testing.T) {
	type nested struct {
		Note string
	}
	type publishable struct {
		AID     string
		Summary string
		Extra   []nested
	}
	if got := inv2.Fields(publishable{}); len(got) != 0 {
		t.Errorf("a clean type reported org fields: %v", got)
	}

	type leaky struct {
		AID   string
		Inner struct {
			Membership struct{ OrgID string }
		}
		Creds []struct{ Credential string }
	}
	got := inv2.Fields(leaky{})
	want := map[string]bool{
		"Inner.Membership":       true,
		"Inner.Membership.OrgID": true,
		"Creds.Credential":       true,
	}
	if len(got) != len(want) {
		t.Fatalf("found %v, want %d org fields", got, len(want))
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
}

// A self-referential type must not hang the walk.
func TestFieldsTerminatesOnRecursiveTypes(t *testing.T) {
	type node struct {
		Name string
		Next *node
	}
	if got := inv2.Fields(node{}); len(got) != 0 {
		t.Errorf("clean recursive type reported %v", got)
	}
}
