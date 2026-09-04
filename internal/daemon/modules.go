package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/module/inv1"
	"github.com/ANetResearch/ANet/module/inv2"
	"github.com/ANetResearch/ANet/provider"
)

// startModules builds and starts every optional subsystem in this build.
//
// This is the seam actually carrying load. It used to be a block of inline
// wiring right here — a config check, a Register call, a log line — one
// subsystem at a time, which is how a daemon ends up with thirty-five files
// that know about a feature. Now the daemon knows only that modules exist,
// and each subsystem lives behind its own build tag.
func (d *Daemon) startModules(ctx context.Context, cfg Config) error {
	raw, err := moduleConfig(cfg)
	if err != nil {
		return err
	}
	mods, err := module.Build(raw)
	if err != nil {
		return err
	}
	for _, m := range mods {
		if err := m.Start(ctx, moduleHost{d}); err != nil {
			// Compiled in AND configured AND unable to start is an operator
			// error. Coming up without the capabilities they asked for is
			// the silent kind of failure, found later by a delegation that
			// mysteriously resolves to no provider.
			return err
		}
		d.modules = append(d.modules, m)
		// A module that can take and make payments becomes this node's
		// payer. Type-asserted rather than imported, exactly like
		// Confidential above: the kernel knows that something may be able
		// to price and settle, and never which package it came from.
		if p, ok := m.(module.Payer); ok {
			d.mu.Lock()
			d.pay = p
			d.mu.Unlock()
		}
	}
	if names := module.Compiled(); len(names) > 0 {
		log.Printf("anet: modules compiled in: %s (started: %d)",
			strings.Join(names, ","), len(d.modules))
	}
	return nil
}

// screenPublication refuses to publish anything carrying a module's
// confidential data — INV-2.
//
// The check runs at the chokepoint rather than at each call site, because
// the leak that matters is the one nobody thought about: a summary an
// agent wrote for itself, a capability name generated from an internal
// id. Every public publication passes through here, and a module that
// holds a secret declares it rather than the daemon guessing.
func (d *Daemon) screenPublication(what string, body any) error {
	// INV-1 first: no org-scoped object may reach a path a third party
	// can read.
	//
	// GuardCommonsPublish had no call site anywhere in this repository,
	// while its own documentation said every publish boundary calls it.
	// The boundaries it was written for — a gossip announce, the commons
	// boards — belong to the previous generation and were not carried
	// over, so the invariant held vacuously and the guard stood watch
	// over nothing.
	//
	// This is the boundary anet4 actually has. What a node registers is
	// synced to federation peers and served from a browsable directory,
	// so it is read by parties the node never chose. The peer-to-peer
	// transport is NOT such a path and is deliberately not guarded: it
	// addresses one named peer over a direct connection, which is the
	// same exposure as the hub relay that already carries CogUnits by
	// design. Guarding it would block a legitimate delivery while proving
	// nothing — the payload there is already bytes, and the guard reads
	// static types.
	//
	// The two invariants sit together because they answer the same
	// question about the same bytes: INV-1 asks whether the TYPE may go
	// out, INV-2 whether these VALUES may.
	if err := inv1.GuardCommonsPublish(body); err != nil {
		return fmt.Errorf("anet: refusing to publish %s: %w", what, err)
	}
	var forbidden []string
	for _, m := range d.modules {
		if c, ok := m.(module.Confidential); ok {
			forbidden = append(forbidden, c.ForbiddenTokens()...)
		}
	}
	if len(forbidden) == 0 {
		return nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := inv2.Screen(b, forbidden); err != nil {
		return fmt.Errorf("anet: refusing to publish %s: %w", what, err)
	}
	return nil
}

// stopModules shuts them down in reverse order.
func (d *Daemon) stopModules(ctx context.Context) {
	for i := len(d.modules) - 1; i >= 0; i-- {
		if err := d.modules[i].Stop(ctx); err != nil {
			log.Printf("anet: module %s: stop: %v", d.modules[i].Name(), err)
		}
	}
	d.modules = nil
}

// moduleConfig projects the daemon's typed config into the per-module raw
// blocks module.Build expects.
//
// The typed fields stay for the modules that ship today, so an existing
// config file keeps working; a module that lands later can take its block
// from cfg.Modules without the daemon growing a field for it.
func moduleConfig(cfg Config) (map[string][]byte, error) {
	out := map[string][]byte{}
	for name, raw := range cfg.Modules {
		out[name] = raw
	}
	if pc := cfg.Providers; pc != nil && pc.ANetLink != nil && pc.ANetLink.Socket != "" {
		b, err := json.Marshal(map[string]any{"socket": pc.ANetLink.Socket})
		if err != nil {
			return nil, err
		}
		out["anetlink"] = b
	}
	return out, nil
}

// moduleHost is the narrow surface a module gets. It is a wrapper rather
// than the *Daemon itself so that widening it takes an edit here — which is
// the moment the org lesson says has to be visible.
type moduleHost struct{ d *Daemon }

func (h moduleHost) AID() string                   { return h.d.AID() }
func (h moduleHost) Providers() *provider.Registry { return h.d.providers }

// ResolveKEL answers for peers this node has verified itself, and for the
// node itself. See peerkel.go for why the first bound is the point rather
// than a limitation.
//
// This node's own key history was missing, and the omission was not
// harmless: the peers table records only key histories seen on the INBOUND
// delegation path, so a node never has its own. `org.verify` therefore
// returned FAILED "org: issuer KEL unresolvable" for every credential the
// verifying node had itself issued — which is exactly the single-node
// organisation that `anetfixture org-genesis --home <data dir>` produces.
// The same credential verified fine on a second node configured with the
// same genesis, so the failing condition was "the issuer is me", not
// "the issuer is the founder".
//
// A node vouching for its own key history is the strongest case of the
// rule this method exists to enforce, not an exception to it: it holds the
// private key. Found by the release matrix on the full variant.
func (h moduleHost) ResolveKEL(aid string) ([]identity.SignedEvent, bool) {
	if aid != "" && aid == h.d.AID() {
		return h.d.self.KEL(), true
	}
	return h.d.peers.resolve(aid)
}
func (h moduleHost) RecordEvidence(kind string, payload any) error {
	_, err := h.d.ledger.Append(kind, payload)
	return err
}

var _ module.Host = moduleHost{}
