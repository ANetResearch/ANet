//go:build !no_service

// Package service exposes an ordinary HTTP service as an ANet capability.
//
// This is the answer to the first question anyone asks: I have a service,
// how do I put it on the network? Until now there was none. The daemon
// could offer devices through ANetLink, or the three built-in modules, and
// nothing else — so the network could carry work between agents that
// already spoke ANet, and could not carry anything a person had written.
//
// A capability declared here is a URL. The daemon POSTs the call's
// arguments as JSON and reads the reply back; the service needs to know
// nothing about ANet, AIDs, receipts or CBOR. That is deliberate — asking
// people to adopt a protocol before they can try it is how a network stays
// empty.
//
// Trust is not the operator's to declare. A service that answers is a
// service that acknowledged (V1), and no configuration here can claim
// otherwise, because the daemon has no way to check whether the answer is
// correct — a caption is not verifiable by the machine that requested it.
// A service that CAN say more returns an `evidence` object and that is
// used instead, the same shape ANetLink puts on C1. Letting a config file
// assert V4 would make the trust axis a preference.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

const name = "service"

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil // compiled in, not configured
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if len(cfg.Capabilities) == 0 {
			return nil, fmt.Errorf("service: no capabilities declared")
		}
		seen := map[string]bool{}
		for i, c := range cfg.Capabilities {
			if c.ID == "" || c.URL == "" {
				return nil, fmt.Errorf("service: capability %d needs both id and url", i)
			}
			if seen[c.ID] {
				return nil, fmt.Errorf("service: capability %q declared twice", c.ID)
			}
			seen[c.ID] = true
		}
		return &Module{cfg: cfg}, nil
	})
}

// Config lists what this node offers and where each one lives.
type Config struct {
	Capabilities []Capability `json:"capabilities"`
	// TimeoutMS bounds one call. A capability that hangs holds a
	// delegation open, and the requester is waiting on the other side of a
	// hub.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// Capability is one offering: an id the network calls, and a URL behind it.
type Capability struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	// Price, when set, is what this capability costs in the hub's credit
	// units. Zero is free, and free means no 402 at all rather than a
	// price of nothing — a caller should not have to parse a payment
	// requirement to learn there is none.
	Price uint64 `json:"price,omitempty"`
	// Description is what a peer sees when deciding whether to call this.
	Description string `json:"description,omitempty"`
	// Protocol names what is on the far side, for the evidence record —
	// "http" unless the service is fronting something more specific.
	Protocol string `json:"protocol,omitempty"`
}

// Module registers the declared capabilities.
type Module struct {
	cfg Config
	cli *http.Client
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	to := time.Duration(m.cfg.TimeoutMS) * time.Millisecond
	if to == 0 {
		to = 2 * time.Minute
	}
	m.cli = &http.Client{Timeout: to}
	return h.Providers().Register(ctx, &svcProvider{m: m})
}

func (m *Module) Stop(context.Context) error { return nil }

type svcProvider struct{ m *Module }

func (p *svcProvider) ID() string { return name }

func (p *svcProvider) Capabilities(context.Context) ([]string, error) {
	out := make([]string, 0, len(p.m.cfg.Capabilities))
	for _, c := range p.m.cfg.Capabilities {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out, nil
}

func (p *svcProvider) Describe(context.Context) (string, error) { return "", nil }

// Health reports the module up. It deliberately does not probe the
// services: a capability whose backend is down should fail when called,
// with the reason, rather than removing every capability on this node
// because one of them is restarting.
func (p *svcProvider) Health(context.Context) error { return nil }

func (p *svcProvider) Invoke(ctx context.Context, call provider.Call) (effect.Effect, error) {
	var target *Capability
	for i := range p.m.cfg.Capabilities {
		if p.m.cfg.Capabilities[i].ID == call.Capability {
			target = &p.m.cfg.Capabilities[i]
			break
		}
	}
	if target == nil {
		return effect.Effect{}, fmt.Errorf("service: no capability %q on this node", call.Capability)
	}

	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return effect.Effect{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return effect.Effect{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	started := time.Now()
	resp, err := p.m.cli.Do(req)
	if err != nil {
		// The service is unreachable. UNAVAILABLE, not FAILED: nothing was
		// attempted at the far end, and a requester deciding whether to
		// retry elsewhere needs that distinction.
		return effect.Effect{
			Status:   effect.Unavailable,
			Message:  fmt.Sprintf("service %s: %v", call.Capability, err),
			Evidence: &effect.Evidence{Protocol: protoOf(target), Requested: call.Capability},
		}, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	latency := time.Since(started).Milliseconds()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return effect.Effect{
			Status:  effect.Failed,
			Message: fmt.Sprintf("service %s: HTTP %d: %s", call.Capability, resp.StatusCode, snippet(raw)),
			Evidence: &effect.Evidence{
				Protocol: protoOf(target), Requested: call.Capability, LatencyMS: latency,
			},
		}, nil
	}

	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		return effect.Effect{
			Status:  effect.Failed,
			Message: fmt.Sprintf("service %s: reply is not a JSON object: %s", call.Capability, snippet(raw)),
			Evidence: &effect.Evidence{
				Protocol: protoOf(target), Requested: call.Capability, LatencyMS: latency,
			},
		}, nil
	}

	ev := &effect.Evidence{
		Protocol: protoOf(target), Requested: call.Capability, NativeAck: true,
		LatencyMS: latency,
		// The answer is what the caller came for, so it travels as the
		// observed state — the only channel that can carry a string.
		ObservedState: string(raw),
		// V1 and no higher. The service answered; whether the answer is
		// right is not something this daemon can establish.
		VerifyTrust: 1,
	}
	// A service that can say more about how it knows is believed about
	// itself — the same latitude ANetLink's adapters have — except for
	// trust, which is capped at what was actually established.
	if declared, ok := reply["evidence"].(map[string]any); ok {
		applyDeclared(ev, declared)
	}

	return effect.Effect{
		Status:   effect.OK,
		Record:   &tsir.EffectRecord{Metrics: numbersIn(reply)},
		Evidence: ev,
	}, nil
}

const maxReplyBytes = 1 << 20

// applyDeclared folds a service's own evidence in. Trust is taken as the
// lower of what it claims and what a plain HTTP answer establishes: a
// service asserting V4 over an unauthenticated POST is asserting something
// the transport cannot support, and believing it would make the trust axis
// decorative.
func applyDeclared(ev *effect.Evidence, declared map[string]any) {
	if s, ok := declared["observed_state"].(string); ok && s != "" {
		ev.ObservedState = s
	}
	if s, ok := declared["protocol"].(string); ok && s != "" {
		ev.Protocol = s
	}
	if s, ok := declared["quirk"].(string); ok && s != "" {
		ev.Quirk = s
	}
	if b, ok := declared["native_ack"].(bool); ok {
		ev.NativeAck = b
	}
}

// numbersIn lifts the reply's top-level numbers into metrics, which is
// what a TSIR acceptance predicate can evaluate. Nested objects are left
// in the observed state: flattening them would invent field names.
func numbersIn(reply map[string]any) map[string]float64 {
	out := map[string]float64{}
	for k, v := range reply {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func protoOf(c *Capability) string {
	if c.Protocol != "" {
		return c.Protocol
	}
	return "http"
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

var _ provider.CapabilityProvider = (*svcProvider)(nil)

// Price reports what a declared capability costs. Part of provider.Priced.
func (p *svcProvider) Price(capability string) (uint64, bool) {
	for i := range p.m.cfg.Capabilities {
		c := &p.m.cfg.Capabilities[i]
		if c.ID == capability && c.Price > 0 {
			return c.Price, true
		}
	}
	return 0, false
}

var _ provider.Priced = (*svcProvider)(nil)
