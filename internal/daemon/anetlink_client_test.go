package daemon

// The daemon's side of the C1 v1 client (LEC §4.3.12): what a provider's refusal does to the task
// (CC-4), which public capabilities are published when a provider refuses remote callers (CC-5),
// the socket the sandbox is kept from and the start checks of modules.anetlink (CC-6, §3.2).

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/provider"
)

// refusingProvider answers every call with a refusal for reason, its log detail naming what the
// far side said.
type refusingProvider struct {
	reason string
	calls  atomic.Int32
}

func (p *refusingProvider) ID() string { return "refusing" }
func (p *refusingProvider) Capabilities(context.Context) ([]string, error) {
	return []string{"door.open@sim/front"}, nil
}
func (p *refusingProvider) Describe(context.Context) (string, error) { return "", nil }
func (p *refusingProvider) Health(context.Context) error             { return nil }
func (p *refusingProvider) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	p.calls.Add(1)
	return effect.Effect{}, &provider.RefusalError{Reason: p.reason,
		Err: errors.New("C1 not_permitted (HTTP 403) on /run/secret-path/c1.sock")}
}

// CC-4: a provider's refusal ends the task as its reason's row says, on both sides, and the
// requester is sent the user's code and nothing of what the far side said: not ANetLink's C1 code,
// not the socket's path.
func TestAProviderRefusalEndsTheTaskWithItsReason(t *testing.T) {
	for _, c := range []struct {
		reason, sent string // what the provider says, what the task carries
		state        interactions.State
		effect       effect.Status
	}{
		{"provider_policy", "provider_policy", interactions.StateRejected, effect.Unavailable},
		{"capability_not_served", "capability_not_served", interactions.StateRejected, effect.Unavailable},
		{"args_too_large", "args_too_large", interactions.StateRejected, effect.Unavailable},
		{"unavailable", "unavailable", interactions.StateRejected, effect.Unavailable},
		{"backend_failed", "backend_failed", interactions.StateFailed, effect.Unverified},
		{"no_such_reason", "backend_failed", interactions.StateFailed, effect.Unverified},
	} {
		t.Run(c.reason, func(t *testing.T) {
			srv := newFakeHub(t)
			ctx := context.Background()
			req := newTestDaemon(t, srv.URL, false)
			prov := newTestDaemon(t, srv.URL, true)
			p := &refusingProvider{reason: c.reason}
			if err := prov.Providers().Register(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil); err != nil {
				t.Fatal(err)
			}
			if err := prov.RegisterWithHub(ctx, srv.URL, "Door", nil); err != nil {
				t.Fatal(err)
			}
			id, err := req.DelegateCapability(ctx, prov.AID(), "door.open@sim/front", map[string]any{"open": true})
			if err != nil {
				t.Fatal(err)
			}
			if err := prov.pollOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if p.calls.Load() != 1 {
				t.Fatalf("the provider was called %d times", p.calls.Load())
			}
			for side, d := range map[string]*Daemon{"provider": prov, "requester": req} {
				if side == "requester" {
					if err := req.pollOnce(ctx); err != nil {
						t.Fatal(err)
					}
				}
				ix, err := d.ix.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				var res capabilityResult
				_ = json.Unmarshal(ix.Result, &res)
				meta := decodeMeta([]byte(ix.ResultMeta))
				if ix.State != c.state || res.Status != string(c.effect) || meta["anet.reason"] != c.sent {
					t.Errorf("%s: state %s, effect %q, anet.reason %v; want %s, %s, %s", side, ix.State, res.Status,
						meta["anet.reason"], c.state, c.effect, c.sent)
				}
				for _, leak := range []string{"not_permitted", "secret-path", "403"} {
					if strings.Contains(string(ix.Result), leak) || strings.Contains(ix.ResultMeta, leak) {
						t.Errorf("%s: the far side's words reached the result: %s %s", side, ix.Result, ix.ResultMeta)
					}
				}
			}
		})
	}
}

// refusalOutcome follows the reason table, row by row: every code a provider may name ends the task
// the way its row's class and effect say.
func TestRefusalOutcomeFollowsTheReasonTable(t *testing.T) {
	for _, code := range []string{"provider_policy", "capability_not_served", "args_too_large", "unavailable", "backend_failed"} {
		row, ok := a2ashape.Lookup(code)
		if !ok {
			t.Fatalf("%s has no row", code)
		}
		status, opts := refusalOutcome(code)
		if status != row.A2A.Effect || opts.reason != code {
			t.Errorf("%s: effect %s reason %s; the row says %s", code, status, opts.reason, row.A2A.Effect)
		}
		if failed := opts.state == interactions.StateFailed; failed != (row.Class&a2ashape.ClassR == 0) {
			t.Errorf("%s: state %q for a row of class %v", code, opts.state, row.Class)
		}
	}
	// Every C1 reason a provider maps reaches one of them.
	for _, c1 := range []string{"not_permitted", "bad_request", "too_large", "unsupported_media", "unknown_capability",
		"c1_version", "not_found", "internal", ""} {
		if _, opts := refusalOutcome(a2ashape.C1Reason(c1)); opts.reason != a2ashape.C1Reason(c1) {
			t.Errorf("C1 %q: the task carries %s, not %s", c1, opts.reason, a2ashape.C1Reason(c1))
		}
	}
}

// remoteLamp serves two capabilities and lets remote callers use one of them (provider.RemotePolicy).
type remoteLamp struct {
	lampProvider
	asked atomic.Int32
	fail  bool
}

func (l *remoteLamp) ID() string { return "anetlink" }
func (l *remoteLamp) Capabilities(context.Context) ([]string, error) {
	return []string{"light.onoff@sim/lamp-1", "sensor.temperature@modbus/plc-1"}, nil
}
func (l *remoteLamp) RemoteAllowed(context.Context) (map[string]bool, error) {
	l.asked.Add(1)
	if l.fail {
		return nil, errors.New("anetlinkd is not there")
	}
	return map[string]bool{"light.onoff@sim/lamp-1": true, "sensor.temperature@modbus/plc-1": false}, nil
}

// cardSkill is a capability another provider serves, with no say on remote callers.
type cardSkill struct{ lampProvider }

func (*cardSkill) ID() string { return "plain" }
func (*cardSkill) Capabilities(context.Context) ([]string, error) {
	return []string{"text.echo"}, nil
}

// CC-5: a public capability its provider refuses to remote callers is not published, on the card
// or in the registration's caps; one it allows is, and so is every capability of a provider that
// has no such say. The provider is asked once per publication. One that cannot answer has none of
// its capabilities published.
func TestOnlyCapabilitiesRemoteCallersMayUseArePublished(t *testing.T) {
	h := newFakeHub(t)
	d := newCardDaemon(t, "Lamp Agent", "Turns the lamp on and off.")
	l := &remoteLamp{}
	if err := d.Providers().Register(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	if err := d.Providers().Register(context.Background(), &cardSkill{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d, "light.onoff@sim/lamp-1", "sensor.temperature@modbus/plc-1", "text.echo")

	if got := d.publicSkillIDs(); !slices.Equal(got, []string{"light.onoff@sim/lamp-1", "text.echo"}) {
		t.Fatalf("card skills %v", got)
	}
	if n := l.asked.Load(); n != 1 {
		t.Fatalf("the provider was asked %d times for one card", n)
	}
	caps := []string{"sensor.temperature@modbus/plc-1", "light.onoff@sim/lamp-1", "text.echo", "not.public"}
	if got := d.publicOnly(caps); !slices.Equal(got, []string{"light.onoff@sim/lamp-1", "text.echo"}) {
		t.Fatalf("registered caps %v", got)
	}
	raw, _, err := d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	if v := verifyOwn(t, d, raw); len(v.Skills) != 2 || strings.Contains(string(raw), "sensor.temperature") {
		t.Fatalf("the card lists %+v:\n%s", v.Skills, raw)
	}

	l.fail = true
	if got := d.publicSkillIDs(); !slices.Equal(got, []string{"text.echo"}) {
		t.Fatalf("with the provider unable to answer, the card lists %v", got)
	}
	if got := d.publicOnly(caps); !slices.Equal(got, []string{"text.echo"}) {
		t.Fatalf("with the provider unable to answer, the registration lists %v", got)
	}
}

// LEC §3.2: modules.anetlink is checked at start. allow_tcp may not be true (C1 runs over a Unix
// socket only); socket is a path a Unix socket can have, and may be left out only where
// XDG_RUNTIME_DIR gives it a default. Each refusal is config_invalid naming the key.
func TestTheANetLinkBlockIsCheckedAtStart(t *testing.T) {
	for _, ok := range []any{
		map[string]any{},
		map[string]any{"socket": "/run/anetlink/c1.sock", "expected_user": "0"},
		map[string]any{"allow_tcp": false},
	} {
		if r := startRefusals(t, map[string]any{"anetlink": ok}); r != nil {
			t.Errorf("%v: refused: %v", ok, r[0].Args)
		}
	}
	for _, c := range []struct {
		block any
		key   string
		rule  string
	}{
		{map[string]any{"allow_tcp": true}, "modules.anetlink.allow_tcp", "C1 runs over a Unix socket only, never TCP"},
		{map[string]any{"socket": "run/c1.sock"}, "modules.anetlink.socket", "clean absolute path"},
		{map[string]any{"socket": "/run/a:b/c1.sock"}, "modules.anetlink.socket", "clean absolute path"},
		{map[string]any{"socket": 7}, "modules.anetlink.socket", "clean absolute path"},
		{"/run/anetlink/c1.sock", "modules.anetlink", "{} takes the defaults"},
	} {
		r := startRefusals(t, map[string]any{"anetlink": c.block})
		if len(r) != 1 || r[0].Code() != "config_invalid" || r[0].Args["key"] != c.key {
			t.Errorf("%v: %+v, want config_invalid for %s", c.block, r, c.key)
			continue
		}
		if s := a2ashape.Sentence("config_invalid", a2ashape.English, r[0].Args); !strings.Contains(s, c.rule) {
			t.Errorf("%v: %q does not say %q", c.block, s, c.rule)
		}
		if s := a2ashape.Sentence("config_invalid", a2ashape.Chinese, r[0].Args); !strings.Contains(s, c.key) {
			t.Errorf("%v: %q does not name %s", c.block, s, c.key)
		}
	}
	// Without XDG_RUNTIME_DIR the socket has no default.
	l := NewLayout(t.TempDir())
	m := freshConfigFile(t)
	m["modules"] = map[string]any{"anetlink": map[string]any{}}
	writeConfigMap(t, l, m)
	t.Setenv("XDG_RUNTIME_DIR", "")
	var cr *ConfigRefusal
	if err := CheckConfigForStart(l); !errors.As(err, &cr) || len(cr.Refusals) != 1 || cr.Refusals[0].Args["key"] != "modules.anetlink.socket" {
		t.Fatalf("{} without XDG_RUNTIME_DIR: %v", err)
	}
}
