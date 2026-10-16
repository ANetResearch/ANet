package daemon

import (
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
	"slices"
	"strings"
	"testing"
)

func setCaps(d *Daemon, caps ...PublicCapability) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in := d.cfg.inbound()
	in.PublicCapabilities = caps
	d.cfg.Inbound = &in
}

func skillOf(t *testing.T, card []byte, id string) map[string]any {
	t.Helper()
	c := decodeCard(t, card)
	skills, _ := c["skills"].([]any)
	for _, s := range skills {
		if m, _ := s.(map[string]any); m != nil && m["id"] == id {
			return m
		}
	}
	t.Fatalf("no skill %q on the card", id)
	return nil
}

// CUX-F31 and CUX-F30: every public capability ends its description with
// the per-caller limits; one that takes files lists the types as input modes
// and says how many and how large; discard says the inputs are not stored.
func TestCardSnapshotLimitsAndFiles(t *testing.T) {
	d, _ := extNode(t, []string{"face.detect", "face.compare", "plain.do"}, nil, nil)
	setCaps(d,
		PublicCapability{ID: "plain.do"},
		PublicCapability{ID: "face.detect", PerCallerPerMin: 10, PerCallerPerDay: 100, MaxInflight: 4, PerCallerInflight: 2,
			Files: &CapFilesConfig{Count: 1, MaxBytes: 5 << 20, Types: []string{"image/jpeg", "image/png"}}},
		PublicCapability{ID: "face.compare", Discard: true,
			Files: &CapFilesConfig{Count: 2, MaxBytes: 1 << 19, Types: []string{"image/png"}}},
	)
	card, err := d.NetworkCard()
	if err != nil {
		t.Fatal(err)
	}
	verifyOwn(t, d, card)

	desc := func(id string) string {
		s, _ := skillOf(t, card, id)["description"].(string)
		return s
	}
	if got := desc("plain.do"); !strings.HasSuffix(got, "Limits per caller: 60/min, 2000/day, 1 at a time.") {
		t.Errorf("plain.do: %q", got)
	}
	if got := desc("face.detect"); !strings.HasSuffix(got,
		"Limits per caller: 10/min, 100/day, 2 at a time. Input: 1 file of image/jpeg, image/png, each up to 5 MiB.") {
		t.Errorf("face.detect: %q", got)
	}
	if got := desc("face.compare"); !strings.HasSuffix(got,
		"Limits per caller: 60/min, 2000/day, 1 at a time. Input: 2 files of image/png, each up to 0.5 MiB. Inputs are not stored.") {
		t.Errorf("face.compare: %q", got)
	}
	// The global values are not on the card.
	if strings.Contains(desc("face.detect"), "global") || strings.Contains(desc("face.detect"), "1200") {
		t.Error("a global limit is on the card")
	}
	in, _ := skillOf(t, card, "face.detect")["inputModes"].([]any)
	if len(in) != 3 || in[0] != "application/json" || in[1] != "image/jpeg" || in[2] != "image/png" {
		t.Errorf("inputModes %v", in)
	}
	if _, has := skillOf(t, card, "plain.do")["inputModes"]; has {
		t.Error("a capability without files lists input modes")
	}
}

// CUX-F32: an allow_only capability is on no card the hub sees, and on the
// list variant of the extended card only.
func TestAllowOnlyOnlyInListVariant(t *testing.T) {
	d, _ := extNode(t, []string{"face.detect"}, []string{"face.compare"}, nil)
	if _, err := d.editPeerFile(ListAllow, "did:anet:bfriend", true); err != nil {
		t.Fatal(err)
	}
	pub, err := d.NetworkCard()
	if err != nil {
		t.Fatal(err)
	}
	if skills := extCardSkillIDs(t, pub); !slices.Equal(skills, []string{"face.detect"}) {
		t.Errorf("public card skills %v", skills)
	}
	if got := d.publicOnly([]string{"face.detect", "face.compare"}); !slices.Equal(got, []string{"face.detect"}) {
		t.Errorf("caps sent to the hub: %v", got)
	}
	list := answerOf(t, d, "did:anet:bfriend").Card
	if skills := extCardSkillIDs(t, list); !slices.Contains(skills, "face.compare") || !slices.Contains(skills, "face.detect") {
		t.Errorf("list variant skills %v", skills)
	}
	stranger := answerOf(t, d, "did:anet:bstranger").Card
	if len(stranger) > 0 && slices.Contains(extCardSkillIDs(t, stranger), "face.compare") {
		t.Errorf("a caller on no list sees the allow_only skill: %v", extCardSkillIDs(t, stranger))
	}
}

// CUX-F22: a code the service chose is repeated when it has the form of
// CORE-K19; an unknown one without that form is backend_failed; busy keeps
// its wait.
func TestServiceRefusalOutcome(t *testing.T) {
	st, o := providerRefusalOutcome(&provider.RefusalError{Reason: "no_face_found", Verbatim: true})
	if st != "FAILED" || o.reason != "no_face_found" || o.state != interactions.StateFailed {
		t.Errorf("%s %+v", st, o)
	}
	st, o = providerRefusalOutcome(&provider.RefusalError{Reason: "no_face_found"})
	if o.reason != "backend_failed" {
		t.Errorf("a code not marked verbatim: %s %+v", st, o)
	}
	st, o = providerRefusalOutcome(&provider.RefusalError{Reason: "busy", RetryAfterS: 15})
	if st != "UNAVAILABLE" || o.reason != "busy" || o.retryAfterMS != 15000 {
		t.Errorf("%s %+v", st, o)
	}
}
