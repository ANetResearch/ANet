package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"
)

type fake struct {
	id   string
	caps []string
}

func (f *fake) ID() string                                        { return f.id }
func (f *fake) Capabilities(context.Context) ([]string, error)    { return f.caps, nil }
func (f *fake) Describe(context.Context) (string, error)          { return "", nil }
func (f *fake) Health(context.Context) error                      { return nil }
func (f *fake) Invoke(_ context.Context, c Call) (effect.Effect, error) {
	return effect.Effect{Status: effect.Unverified, Message: c.Capability}, nil
}

func TestRegisterAndResolve(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()
	if err := r.Register(ctx, &fake{id: "link", caps: []string{"light", "sensor.temp"}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(ctx, &fake{id: "agent", caps: []string{"code.review"}}); err != nil {
		t.Fatal(err)
	}

	if p, ok := r.Resolve("sensor.temp"); !ok || p.ID() != "link" {
		t.Fatalf("exact resolve failed: %v %v", p, ok)
	}
	if p, ok := r.Resolve("light.onoff.toggle"); !ok || p.ID() != "link" {
		t.Fatalf("parent walk failed: %v %v", p, ok)
	}
	if _, ok := r.Resolve("lightning.strike"); ok {
		t.Fatal("false prefix: \"lightning\" must not match \"light\"")
	}
	if _, ok := r.Resolve("nothing"); ok {
		t.Fatal("unknown capability resolved")
	}
}

func TestConflictsAreAtomic(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()
	if err := r.Register(ctx, &fake{id: "a", caps: []string{"x", "y"}}); err != nil {
		t.Fatal(err)
	}
	err := r.Register(ctx, &fake{id: "a", caps: []string{"z"}})
	if !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("want ErrDuplicateID, got %v", err)
	}
	err = r.Register(ctx, &fake{id: "b", caps: []string{"z", "y"}})
	if !errors.Is(err, ErrCapabilityConflict) {
		t.Fatalf("want ErrCapabilityConflict, got %v", err)
	}
	if _, ok := r.Resolve("z"); ok {
		t.Fatal("failed registration must leave no index entries")
	}
}

func TestDeregisterCleansIndex(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()
	_ = r.Register(ctx, &fake{id: "a", caps: []string{"x"}})
	r.Deregister("a")
	if _, ok := r.Resolve("x"); ok {
		t.Fatal("index entry survived deregister")
	}
	if len(r.IDs()) != 0 {
		t.Fatal("provider survived deregister")
	}
}

func TestRefreshReindexes(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry()
	f := &fake{id: "a", caps: []string{"x"}}
	_ = r.Register(ctx, f)
	f.caps = []string{"y"}
	if err := r.Refresh(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Resolve("x"); ok {
		t.Fatal("stale capability survived refresh")
	}
	if p, ok := r.Resolve("y"); !ok || p.ID() != "a" {
		t.Fatal("new capability not indexed")
	}
}
