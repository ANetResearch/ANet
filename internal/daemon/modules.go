package daemon

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/module"
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
	}
	if names := module.Compiled(); len(names) > 0 {
		log.Printf("anet: modules compiled in: %s (started: %d)",
			strings.Join(names, ","), len(d.modules))
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

// ResolveKEL answers only for peers this node has verified itself. See
// peerkel.go for why that bound is the point rather than a limitation.
func (h moduleHost) ResolveKEL(aid string) ([]identity.SignedEvent, bool) {
	return h.d.peers.resolve(aid)
}
func (h moduleHost) RecordEvidence(kind string, payload any) error {
	_, err := h.d.ledger.Append(kind, payload)
	return err
}

var _ module.Host = moduleHost{}
