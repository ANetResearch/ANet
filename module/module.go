// Package module is the daemon's optional-subsystem seam.
//
// The org lesson, made structural. In v1 the organisation feature grew into
// thirty-five daemon files and could not be pulled back out; the open-source
// release of 1.3.0-v3 needed an axe and 683,852 deleted lines. What was
// missing was never discipline — it was a seam. Code with nowhere to attach
// attaches everywhere.
//
// So a subsystem that is not the kernel attaches here, and only here:
//
//	provider registry ─┐
//	identity / ledger ─┼─ kernel, never optional
//	hub client ────────┘
//	                    ├─ module ─ p2p
//	                    ├─ module ─ blackboard (shared-brain)
//	                    ├─ module ─ store (distributed storage)
//	                    ├─ module ─ org
//	                    └─ module ─ anetlink (device capabilities)
//
// Two rules give the seam teeth:
//
//   - A module receives a Host, not the *Daemon. It can register providers,
//     read identity, append evidence, and nothing else. A module that needs
//     more is telling you it belongs in the kernel or does not belong at all
//     — and the compiler is what says so, at the moment the reach is written
//     rather than two years later.
//   - Registration happens under a build tag, so "pluggable" is a property
//     the build proves rather than a claim in a design document. A
//     distribution that ships alongside a hub and needs no peer-to-peer
//     transport is `-tags no_p2p`, not a fork.
package module

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/ANetResearch/ANet/provider"
)

// Host is everything a module may use of the daemon.
//
// Deliberately small, and deliberately an interface rather than a struct
// pointer: it is the list of things a subsystem is allowed to want. Growing
// it is a decision someone has to make on purpose, in this file, which is
// exactly the moment that went unmarked in v1.
type Host interface {
	// AID is this node's identity anchor.
	AID() string
	// Providers is the C1 registry — the only way a module offers
	// capabilities to the network.
	Providers() *provider.Registry
	// RecordEvidence appends to this node's own chain. Modules produce
	// evidence like everything else; they do not get a private ledger.
	RecordEvidence(eventType string, payload any) error
}

// Module is an optional daemon subsystem.
type Module interface {
	// Name is the module's stable name; it matches its build tag, so
	// `no_<name>` compiles it out.
	Name() string
	// Start brings the module up. An error is fatal to the daemon: a module
	// that was compiled in and configured but cannot run is an operator
	// error, and starting without it would be silent capability loss.
	Start(ctx context.Context, h Host) error
	// Stop shuts it down.
	Stop(ctx context.Context) error
}

// Factory builds a module from its configuration block. Returning (nil, nil)
// means "compiled in, not configured" — the ordinary case for a module the
// operator has not asked for.
type Factory func(raw []byte) (Module, error)

type registration struct {
	name    string
	factory Factory
}

var (
	regMu    sync.Mutex
	registry []registration
)

// Register adds a module factory. Called from an init() in a file carrying
// the module's build tag, which is what makes the tag the on/off switch.
func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, r := range registry {
		if r.name == name {
			panic("module: duplicate registration " + name)
		}
	}
	registry = append(registry, registration{name, f})
}

// Compiled lists the module names in this build, sorted. The daemon logs it
// at startup so an operator can see what their binary actually contains
// rather than what the documentation says it might.
func Compiled() []string {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]string, 0, len(registry))
	for _, r := range registry {
		out = append(out, r.name)
	}
	sort.Strings(out)
	return out
}

// Build instantiates every compiled-in module that is configured.
//
// A configured module that is not compiled in is an error, not a no-op. The
// alternative is a daemon that reads `"p2p": {...}` from a config, silently
// ignores it, and leaves an operator wondering why nothing is peering.
func Build(cfg map[string][]byte) ([]Module, error) {
	regMu.Lock()
	regs := append([]registration(nil), registry...)
	regMu.Unlock()

	known := make(map[string]bool, len(regs))
	var out []Module
	for _, r := range regs {
		known[r.name] = true
		m, err := r.factory(cfg[r.name])
		if err != nil {
			return nil, fmt.Errorf("module %q: %w", r.name, err)
		}
		if m != nil {
			out = append(out, m)
		}
	}
	for name := range cfg {
		if !known[name] {
			return nil, fmt.Errorf(
				"module %q is configured but not compiled into this build (built with no_%s?)",
				name, name)
		}
	}
	return out, nil
}
