//go:build !no_blackboard

package blackboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// The shared-cognition module: anet3's org blackboard, offered through C1.
//
// The store itself came across from anet3's v3 runtime unchanged — the CRDT,
// the signed CogUnit, the per-task phase machine and all nine of its tests.
// It needed no adaptation beyond its imports, because everything it depends
// on (identity, coredet, CIDs) is what became ANetCore.
//
// What is new is how it reaches the network. anet3 wired the blackboard into
// org-central, which knew about boards, cards, credentials and members all
// at once. Here it is a CapabilityProvider and nothing else: it offers
// `blackboard.add`, `blackboard.snapshot` and `blackboard.conclude`, and the
// daemon resolves and invokes those exactly as it resolves a light switch.
// A peer contributes to shared cognition by delegating a capability call —
// no new path through the daemon, no second notion of what a peer is.
//
// INV-1 rides along: CogUnit is marked org-scoped, so the guard rejects it
// on any commons publish path. That marker matters more here than it did in
// anet3, because anet4's transport seam lets a module add a delivery path
// the daemon knows nothing about.
const name = "blackboard"

// Capability ids this module offers.
const (
	CapAdd      = "blackboard.add"
	CapSnapshot = "blackboard.snapshot"
	CapConclude = "blackboard.conclude"
)

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil // compiled in, not configured
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		return &Module{cfg: cfg}, nil
	})
}

// Config configures the board.
type Config struct {
	// Enabled turns the board on. Present so a configuration block can
	// disable it without being deleted.
	Enabled *bool `json:"enabled"`
}

// Module offers one board as a capability provider.
type Module struct {
	cfg   Config
	board *Blackboard
	host  module.Host
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	if m.cfg.Enabled != nil && !*m.cfg.Enabled {
		return nil
	}
	// The board's HLC is tagged with this node's AID, so a stamp says which
	// node authored it without a separate node-id concept to keep in sync.
	m.board = New(h.AID())
	m.host = h
	return h.Providers().Register(ctx, &boardProvider{m: m})
}

func (m *Module) Stop(context.Context) error { return nil }

// Board exposes the store, for a daemon that wants it in-process.
func (m *Module) Board() *Blackboard { return m.board }

// boardProvider is the C1 face of the board.
type boardProvider struct{ m *Module }

func (p *boardProvider) ID() string { return name }

func (p *boardProvider) Capabilities(context.Context) ([]string, error) {
	return []string{CapAdd, CapSnapshot, CapConclude}, nil
}

func (p *boardProvider) Describe(context.Context) (string, error) { return "", nil }

func (p *boardProvider) Health(context.Context) error {
	if p.m.board == nil {
		return fmt.Errorf("blackboard: not started")
	}
	return nil
}

func (p *boardProvider) Invoke(_ context.Context, call provider.Call) (effect.Effect, error) {
	if p.m.board == nil {
		return effect.Effect{}, fmt.Errorf("blackboard: not started")
	}
	switch call.Capability {
	case CapAdd:
		return p.add(call)
	case CapSnapshot:
		return p.snapshot(call)
	case CapConclude:
		return p.conclude(call)
	}
	return effect.Effect{}, fmt.Errorf("blackboard: unknown capability %q", call.Capability)
}

// add merges one signed contribution.
//
// The unit arrives already signed by its author — this node verifies, it
// does not sign. That is the property that makes a shared board safe to
// merge from anyone: a contribution nobody can attribute is one this board
// refuses, and re-stamping a pulled unit here would destroy the causal
// order the author established.
func (p *boardProvider) add(call provider.Call) (effect.Effect, error) {
	raw, _ := call.Args["unit"].(string)
	if raw == "" {
		return effect.Effect{}, fmt.Errorf("blackboard: add needs a base64 CogUnit in args.unit")
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return effect.Effect{}, fmt.Errorf("blackboard: unit not base64: %w", err)
	}
	u, err := Unmarshal(b)
	if err != nil {
		return effect.Effect{}, fmt.Errorf("blackboard: unit malformed: %w", err)
	}
	added, err := p.m.board.Add(u, p.resolver(), time.Now().UnixMilli())
	if err != nil {
		// A rejected contribution is an honest failure, not an error to
		// swallow: the caller asked to add something this board will not
		// accept, and needs to know which.
		return effect.Effect{
			Status:  effect.Failed,
			Message: err.Error(),
			Evidence: &effect.Evidence{
				Protocol: name, Requested: CapAdd, VerifyTrust: 3,
			},
		}, nil
	}
	n := 0.0
	if added {
		n = 1
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{
			"added": n, "units": float64(p.m.board.Len()),
		}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapAdd, NativeAck: true,
			// V3: the board read its own state back after merging, by a
			// path independent of the caller's claim.
			VerifyTrust: 3,
		},
	}, nil
}

// snapshot returns the board's units in causal order.
func (p *boardProvider) snapshot(call provider.Call) (effect.Effect, error) {
	// A snapshot is either the whole board or one task's slice of it —
	// "the board for task T" is the subset tagged with T, which is what
	// UnitsForTask returns.
	taskID, _ := call.Args["task_id"].(string)
	units := p.m.board.Snapshot()
	if taskID != "" {
		units = p.m.board.UnitsForTask(taskID)
	}
	out := make([]string, 0, len(units))
	for _, u := range units {
		b, err := u.Marshal()
		if err != nil {
			return effect.Effect{}, err
		}
		out = append(out, base64.StdEncoding.EncodeToString(b))
	}
	blob, err := json.Marshal(out)
	if err != nil {
		return effect.Effect{}, err
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"units": float64(len(out))}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapSnapshot, NativeAck: true,
			ObservedState: string(blob), VerifyTrust: 3,
		},
	}, nil
}

// conclude freezes a task's cognition so it can be sedimented.
func (p *boardProvider) conclude(call provider.Call) (effect.Effect, error) {
	taskID, _ := call.Args["task_id"].(string)
	if taskID == "" {
		return effect.Effect{}, fmt.Errorf("blackboard: conclude needs args.task_id")
	}
	if err := p.m.board.Conclude(taskID); err != nil {
		return effect.Effect{Status: effect.Failed, Message: err.Error(),
			Evidence: &effect.Evidence{Protocol: name, Requested: CapConclude}}, nil
	}
	return effect.Effect{
		Status: effect.OK,
		Record: &tsir.EffectRecord{Metrics: map[string]float64{"concluded": 1}},
		Evidence: &effect.Evidence{
			Protocol: name, Requested: CapConclude, NativeAck: true,
			ObservedState: string(p.m.board.Phase(taskID)), VerifyTrust: 3,
		},
	}, nil
}

// resolver answers "whose key signed this" for the units this board merges.
//
// A board that cannot resolve an author must refuse the unit rather than
// accept it unverified — anet3 drew that line with a distinct error, and it
// is the line that keeps a shared board from becoming a place anyone can
// write anything.
func (p *boardProvider) resolver() KELResolver {
	return func(authorAID string) ([]identity.SignedEvent, bool) {
		if p.m.host == nil {
			return nil, false
		}
		return p.m.host.ResolveKEL(authorAID)
	}
}

var _ provider.CapabilityProvider = (*boardProvider)(nil)
