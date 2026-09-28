package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// blockCap is served by blockLamp: a short capability call (no declared
// timeout) that runs until its gate opens or its context ends.
const blockCap = "relay.pulse@sim/board-2"

type blockLamp struct {
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
	runs    atomic.Int32
}

func newBlockLamp(open bool) *blockLamp {
	l := &blockLamp{gate: make(chan struct{}), started: make(chan struct{})}
	if open {
		close(l.gate)
	}
	return l
}

func (p *blockLamp) ID() string { return "blocklamp" }
func (p *blockLamp) Capabilities(context.Context) ([]string, error) {
	return []string{blockCap}, nil
}
func (p *blockLamp) Describe(context.Context) (string, error) { return "", nil }
func (p *blockLamp) Health(context.Context) error             { return nil }
func (p *blockLamp) Invoke(ctx context.Context, _ provider.Call) (effect.Effect, error) {
	p.runs.Add(1)
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.gate:
		return effect.Effect{Status: effect.OK, Record: &tsir.EffectRecord{Metrics: map[string]float64{"pulsed": 1}}}, nil
	case <-ctx.Done():
		return effect.Effect{}, ctx.Err()
	}
}

// 0017 Q29 (docs/notes/0025 N3): a delivery over a direct transport is
// acknowledged as soon as step 10 has committed it, not after the work that
// follows. A short capability call that takes longer than the sender waits
// used to hold the ack until it had run and its answer had been sent, so the
// sender counted the delivery failed and sent the delegation again through
// the hub. Acknowledged before it runs, the call is recorded working, so a
// process that stops before answering runs it again at the next start: no
// redelivery will come for it (at-least-once, as before).
func TestADirectDeliveryIsAcknowledgedAtItsCommit(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	lamp := newBlockLamp(false)
	t.Cleanup(func() {
		select {
		case <-lamp.gate:
		default:
			close(lamp.gate)
		}
	})
	if err := prov.Providers().Register(ctx, lamp); err != nil {
		t.Fatal(err)
	}
	id, err := req.DelegateCapability(ctx, prov.AID(), blockCap, map[string]any{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())

	errc := make(chan error, 1)
	go func() { errc <- prov.Inbound().Receive(ctx, env) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("the committed delivery was not acknowledged: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery was acknowledged only after its call ran; the sender would have sent it again through the hub")
	}
	<-lamp.started
	pix, err := prov.ix.Get(id)
	if err != nil || pix.State != interactions.StateWorking || len(pix.Receipt) > 0 {
		t.Fatalf("provider while the call runs: %+v (%v), want working, unanswered", pix, err)
	}

	// The process stops before the call answers. Nothing is recorded for
	// it, and the acknowledged delegation will not come again.
	layout := prov.layout
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}
	lamp2 := newBlockLamp(true)
	probe := &startupProbe{provider: lamp2}
	startupProbes.Store(t.Name(), probe)
	t.Cleanup(func() { startupProbes.Delete(t.Name()) })
	addModuleConfig(t, layout.Root, "rt_startup_probe", map[string]any{"id": t.Name()})
	prov2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	prov2.stopRelayLoop()
	t.Cleanup(func() { prov2.Close() })

	waitUntil(t, "startup recovery to run the call again", func() bool {
		pix, err := prov2.ix.Get(id)
		return err == nil && pix.State == interactions.StateCompleted && len(pix.Receipt) > 0
	})
	if n := lamp2.runs.Load(); n != 1 {
		t.Fatalf("the call ran %d times after the restart, want 1", n)
	}
	waitUntil(t, "the answer at the hub", func() bool { return len(queuedFor(t, srv, req.AID())) > 0 })
	if err := req.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cur, _ := req.ix.Get(id); cur.State != interactions.StateCompleted {
		t.Fatalf("requester: %s, want completed", cur.State)
	}
}
