package daemon

// Red-team finding F10, on review: a provider that does not name the rule
// still has its timeouts reported as "not known". A call whose deadline
// passed while it ran may have had its effect whatever provider ran it;
// the daemon reported the bare error as FAILED ("it did not happen"),
// which a requester may read as safe to send again. provider.OutcomeOf
// reads a deadline as an unknown outcome for every provider.

import (
	"context"
	"fmt"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/provider"
)

// deadlineProvider acts, then runs out of time before it can answer, and
// says only that.
type deadlineProvider struct{ acted int }

func (p *deadlineProvider) ID() string { return "slowdoor" }
func (p *deadlineProvider) Capabilities(context.Context) ([]string, error) {
	return []string{"door.open@sim/front"}, nil
}
func (p *deadlineProvider) Describe(context.Context) (string, error) { return "", nil }
func (p *deadlineProvider) Health(context.Context) error             { return nil }
func (p *deadlineProvider) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	p.acted++
	return effect.Effect{}, fmt.Errorf("waiting for the device: %w", context.DeadlineExceeded)
}

func TestADeadlineFromAnyProviderIsAnUnknownOutcome(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	door := &deadlineProvider{}
	if err := prov.Providers().Register(ctx, door); err != nil {
		t.Fatal(err)
	}
	if err := req.RegisterWithHub(ctx, srv.URL, "Req", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Door", nil, ""); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), "door.open@sim/front", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the provider's answer", func() bool {
		_ = req.pollOnce(ctx)
		return mustIX(t, req, id).IsTerminal()
	})
	if door.acted != 1 {
		t.Fatalf("acted %d times", door.acted)
	}
	task, err := req.taskView(mustIX(t, req, id), viewOpts{artifacts: true})
	if err != nil {
		t.Fatal(err)
	}
	es, reason := task.Metadata[a2ashape.KeyEffectStatus], task.Metadata[a2ashape.KeyReason]
	if task.Status.State != a2ashape.TaskStateFailed || es != string(effect.Unverified) || reason != provider.ReasonTimeout {
		t.Fatalf("state=%v effect_status=%v reason=%v; want failed, UNVERIFIED, timeout (never FAILED: "+
			"the door may be open)", task.Status.State, es, reason)
	}
}
