package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// rt_startup_probe exists only in this test binary and stays off unless a
// test configures it. It stands in for a transport module whose peer
// process is already connected when the daemon starts: from inside its
// Start it hands one envelope to the daemon's Inbound, the way module/p2p's
// deliverInbound does the moment a delivery arrives, and records what
// Receive answered. It can also register a capability provider first, the
// way a module that serves capabilities does from its Start.
func init() {
	module.Register("rt_startup_probe", func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil
		}
		var c struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" {
			return nil, errors.New("rt_startup_probe: config needs an id")
		}
		v, ok := startupProbes.Load(c.ID)
		if !ok {
			return nil, errors.New("rt_startup_probe: no probe " + c.ID)
		}
		return v.(*startupProbe), nil
	})
}

// startupProbes maps a configured id to its probe.
var startupProbes sync.Map // id -> *startupProbe

type startupProbe struct {
	env      []byte
	provider provider.CapabilityProvider
	// wait bounds how long Receive may hold the delivery.
	wait time.Duration

	mu       sync.Mutex
	returned bool
	err      error
}

func (p *startupProbe) Name() string { return "rt_startup_probe" }

func (p *startupProbe) Start(ctx context.Context, h module.Host) error {
	if p.provider != nil {
		if err := h.Providers().Register(ctx, p.provider); err != nil {
			return err
		}
	}
	if p.env == nil {
		return nil // only a provider to register
	}
	th, ok := h.(module.TransportHost)
	if !ok {
		return errors.New("host does not accept transports")
	}
	rctx, cancel := context.WithTimeout(ctx, p.wait)
	defer cancel()
	err := th.Inbound().Receive(rctx, p.env)
	p.mu.Lock()
	p.returned, p.err = true, err
	p.mu.Unlock()
	return nil
}

func (p *startupProbe) Stop(context.Context) error { return nil }

// addModuleConfig adds one module's configuration to the config file of a
// stopped daemon's data directory, keeping everything else in it.
func addModuleConfig(t *testing.T, root, name string, conf any) {
	t.Helper()
	path := filepath.Join(root, "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	mods, _ := cfg["modules"].(map[string]any)
	if mods == nil {
		mods = map[string]any{}
	}
	mods[name] = conf
	cfg["modules"] = mods
	writeTestConfig(t, root, cfg)
}

// [redteam:F30] C1 / SI-10 / SI-6: a transport module delivers from the
// moment its Start runs, which is before startup recovery and before the
// modules after it have started. A long capability call delivered in that
// window used to be accepted, started, and then reported interrupted
// (failed, effect UNVERIFIED) by recoverInterrupted, its real result thrown
// away. Now a delivery during start-up waits until New has finished: one
// that cannot wait (here its context ends first) is refused temporarily,
// so the sender retries or falls back to the hub, and nothing is stored;
// delivered again afterwards it runs as new work and its real result is
// kept.
func TestADeliveryDuringStartupWaitsForRecovery(t *testing.T) {
	srv, req, prov := registeredPair(t)
	ctx := context.Background()
	const capID = "flash.firmware@sim/board-1"
	id, err := req.DelegateCapability(ctx, prov.AID(), capID, map[string]any{"image": "v2"})
	if err != nil {
		t.Fatal(err)
	}
	env := onlyQueuedEnvelope(t, srv, prov.AID())
	clearMailbox(t, srv, prov.AID())
	layout := prov.layout
	if err := prov.Close(); err != nil {
		t.Fatal(err)
	}

	slow := &slowLamp{gate: make(chan struct{}), started: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-slow.gate:
		default:
			close(slow.gate)
		}
	})
	probe := &startupProbe{env: env, provider: slow, wait: 300 * time.Millisecond}
	startupProbes.Store(t.Name(), probe)
	t.Cleanup(func() { startupProbes.Delete(t.Name()) })
	addModuleConfig(t, layout.Root, "rt_startup_probe", map[string]any{"id": t.Name()})
	prov2, err := New(layout)
	if err != nil {
		t.Fatal(err)
	}
	prov2.stopRelayLoop()
	t.Cleanup(func() { prov2.Close() })

	probe.mu.Lock()
	returned, rerr := probe.returned, probe.err
	probe.mu.Unlock()
	if !returned {
		t.Fatal("setup: the probe did not deliver during start-up")
	}
	if rerr == nil {
		t.Fatal("a delivery during start-up was acknowledged before startup recovery ran")
	}
	if _, err := prov2.ix.Get(id); !errors.Is(err, interactions.ErrNotFound) {
		pix, _ := prov2.ix.Get(id)
		t.Fatalf("a delivery during start-up was processed before recovery: %+v (%v)", pix, err)
	}
	if n := slow.runs.Load(); n != 0 {
		t.Fatalf("the call ran %d times during start-up", n)
	}

	// The sender tries again once the node is up: new work, run once, and
	// its real result kept.
	if err := prov2.Inbound().Receive(ctx, env); err != nil {
		t.Fatalf("the delivery after start-up: %v", err)
	}
	<-slow.started
	if pix, err := prov2.ix.Get(id); err != nil || pix.State != interactions.StateWorking {
		t.Fatalf("provider: %+v (%v), want working", pix, err)
	}
	close(slow.gate)
	waitUntil(t, "the call to finish", func() bool {
		pix, err := prov2.ix.Get(id)
		return err == nil && pix.State == interactions.StateCompleted
	})
	pix, _ := prov2.ix.Get(id)
	var res capabilityResult
	_ = json.Unmarshal(pix.Result, &res)
	if res.Status != string(effect.OK) || len(pix.Receipt) == 0 {
		t.Fatalf("stored result %+v, receipt %d bytes; want the real result", res, len(pix.Receipt))
	}
	if n := slow.runs.Load(); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
}
