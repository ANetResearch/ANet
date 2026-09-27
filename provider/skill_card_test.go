package provider

import (
	"context"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
)

// What the network card builder (internal/netcard) relies on SkillInfoOf
// for: every field A2A requires filled, declared values kept.

type cardgenPlain struct{}

func (cardgenPlain) ID() string                                          { return "plain" }
func (cardgenPlain) Capabilities(context.Context) ([]string, error)      { return nil, nil }
func (cardgenPlain) Describe(context.Context) (string, error)            { return "", nil }
func (cardgenPlain) Invoke(context.Context, Call) (effect.Effect, error) { return effect.Effect{}, nil }
func (cardgenPlain) Health(context.Context) error                        { return nil }

type cardgenDescribed struct{ cardgenPlain }

func (cardgenDescribed) SkillInfo(c string) (SkillInfo, bool) {
	if c != "text.digest" {
		return SkillInfo{}, false
	}
	return SkillInfo{Name: "Digest", Description: "Summarises text.", Tags: []string{"summary", " "},
		Examples: []string{"digest this", ""}, InputModes: []string{"text/plain"}}, true
}

func TestSkillInfoOfFillsEveryRequiredField(t *testing.T) {
	for _, p := range []CapabilityProvider{cardgenPlain{}, cardgenDescribed{}} {
		got := SkillInfoOf(p, "light.onoff@sim/lamp-1")
		if got.Name != "light.onoff@sim/lamp-1" || got.Description == "" || len(got.Tags) == 0 {
			t.Fatalf("%T: %+v", p, got)
		}
		for _, tag := range got.Tags {
			if tag == "" {
				t.Fatalf("%T: empty tag in %v", p, got.Tags)
			}
		}
		if got.Examples != nil || got.InputModes != nil || got.OutputModes != nil {
			t.Fatalf("%T: undeclared lists must stay nil so the card leaves them out: %+v", p, got)
		}
	}
}

func TestSkillInfoOfKeepsWhatAProviderDeclares(t *testing.T) {
	got := SkillInfoOf(cardgenDescribed{}, "text.digest")
	if got.Name != "Digest" || got.Description != "Summarises text." {
		t.Fatalf("%+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "summary" || len(got.Examples) != 1 || got.Examples[0] != "digest this" {
		t.Fatalf("empty entries not dropped: %+v", got)
	}
	if len(got.InputModes) != 1 || got.InputModes[0] != "text/plain" || got.OutputModes != nil {
		t.Fatalf("modes: %+v", got)
	}
}
