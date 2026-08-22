// Package provider defines C1 — the CapabilityProvider contract (docs/CONTRACTS-zh.md §3):
// the only doorway through which the ANet daemon acquires callable
// capabilities. Native agents, external agent connectors, and physical-world
// runtimes (ANetLink) all enter through this one interface.
//
// Red line (the org lesson, docs/CONTRACTS-zh.md): the daemon must never know what stands
// behind a provider — in particular it must never know the concept of a
// "device". A provider declares capabilities, can be invoked, and returns
// effects. Nothing else crosses the boundary.
package provider

import (
	"context"

	"github.com/ANetResearch/ANetCore/effect"
)

// Call is one capability invocation crossing C1.
type Call struct {
	// Capability is the exact capability id being invoked (taxonomy form,
	// e.g. "light.onoff"). Resolution to a provider happens before Invoke.
	Capability string
	// Args are CoreDet-encodable invocation arguments.
	Args map[string]any
	// CallID is a caller-chosen idempotency key; providers MAY use it to
	// de-duplicate retries.
	CallID string
	// CallerAID is the verified AID of the calling identity, or "" when the
	// call originates from a local surface (CLI/MCP) before any delegation.
	CallerAID string
}

// CapabilityProvider is C1. Implementations must be safe for concurrent use.
type CapabilityProvider interface {
	// ID is the stable provider identifier, unique within one daemon.
	ID() string
	// Capabilities lists the capability ids this provider currently offers.
	Capabilities(ctx context.Context) ([]string, error)
	// Describe returns the CID of the provider's descriptor object in the
	// local CAS ("" when the provider publishes none). The daemon mounts it
	// on the agent's ADP card without interpreting it.
	Describe(ctx context.Context) (string, error)
	// Invoke executes one capability call and reports its effect. Transport
	// or execution errors are returned as error; a reachable target whose
	// effect cannot be verified is NOT an error (effect.Unverified).
	Invoke(ctx context.Context, call Call) (effect.Effect, error)
	// Health reports provider liveness; a non-nil error marks every
	// capability of this provider temporarily unavailable.
	Health(ctx context.Context) error
}
