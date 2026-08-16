package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	ErrDuplicateID        = errors.New("provider: duplicate provider id")
	ErrCapabilityConflict = errors.New("provider: capability already registered by another provider")
)

// Registry is the daemon-side provider table. Capability indexing happens at
// Register time; call Refresh after a provider's capability set changes.
type Registry struct {
	mu    sync.RWMutex
	provs map[string]CapabilityProvider
	caps  map[string]string // capability id -> provider id
}

func NewRegistry() *Registry {
	return &Registry{provs: map[string]CapabilityProvider{}, caps: map[string]string{}}
}

// Register adds a provider and indexes its capabilities. Registration is
// atomic: on any conflict nothing is registered.
func (r *Registry) Register(ctx context.Context, p CapabilityProvider) error {
	caps, err := p.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("provider %q: capabilities: %w", p.ID(), err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.provs[p.ID()]; dup {
		return fmt.Errorf("%w: %q", ErrDuplicateID, p.ID())
	}
	for _, c := range caps {
		if owner, taken := r.caps[c]; taken && owner != p.ID() {
			return fmt.Errorf("%w: %q held by %q", ErrCapabilityConflict, c, owner)
		}
	}
	r.provs[p.ID()] = p
	for _, c := range caps {
		r.caps[c] = p.ID()
	}
	return nil
}

// Refresh re-indexes one provider's capabilities.
func (r *Registry) Refresh(ctx context.Context, id string) error {
	r.mu.RLock()
	p, ok := r.provs[id]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("provider: unknown id %q", id)
	}
	caps, err := p.Capabilities(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for c, owner := range r.caps {
		if owner == id {
			delete(r.caps, c)
		}
	}
	for _, c := range caps {
		if owner, taken := r.caps[c]; taken && owner != id {
			return fmt.Errorf("%w: %q held by %q", ErrCapabilityConflict, c, owner)
		}
		r.caps[c] = id
	}
	return nil
}

// Deregister removes a provider and its capability index entries.
func (r *Registry) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.provs, id)
	for c, owner := range r.caps {
		if owner == id {
			delete(r.caps, c)
		}
	}
}

// Provider returns a provider by id.
func (r *Registry) Provider(id string) (CapabilityProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.provs[id]
	return p, ok
}

// IDs lists registered provider ids, sorted.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.provs))
	for id := range r.provs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Capabilities lists every indexed capability id, sorted — the daemon's raw
// material for ADP card aggregation.
func (r *Registry) Capabilities() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cs := make([]string, 0, len(r.caps))
	for c := range r.caps {
		cs = append(cs, c)
	}
	sort.Strings(cs)
	return cs
}

// Resolve maps a called capability id to its provider. Matching follows the
// ADP forms relevant on the invoke path: exact first, then parent walk on
// '.' boundaries — a provider registered for "light" serves "light.onoff",
// but "lightning" never matches "light".
func (r *Registry) Resolve(capability string) (CapabilityProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for c := capability; ; {
		if id, ok := r.caps[c]; ok {
			return r.provs[id], true
		}
		i := strings.LastIndexByte(c, '.')
		if i < 0 {
			return nil, false
		}
		c = c[:i]
	}
}
