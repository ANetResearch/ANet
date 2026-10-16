package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
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
	// A module that is turned off is not built (MOD-P5): its factory is not
	// called, it is not registered and not started. A configuration block
	// for a module that is not available is the refusal module_absent.
	// configured, whichever way it is absent (MOD-F1).
	// The auto-reply block keeps its historical top-level key; the module is handed it as its own. In a
	// build or a configuration without the module the block is not passed on (ModuleRefusals has refused
	// it: module_absent.configured).
	if blk := blockOrNil(cfg.AutoReply); blk != nil && d.avail.Available("autoreply") {
		raw["autoreply"] = blk
	}
	mods, err := module.BuildWith(d.avail, raw)
	if err != nil {
		return err
	}
	for _, m := range mods {
		h, st := newModuleHost(d, m.Name())
		if err := m.Start(ctx, h); err != nil {
			// Compiled in AND configured AND unable to start is an operator
			// error. Coming up without the capabilities they asked for is
			// the silent kind of failure, found later by a delegation that
			// mysteriously resolves to no provider.
			return err
		}
		if what := st.deniedCall(); what != "" {
			// A call of a Host method the name table does not grant this
			// module, one with no result to refuse with (K-16, MOD-F8): the
			// module does not start, and neither does the node.
			_ = m.Stop(ctx)
			return fmt.Errorf("anet: module %s called %s, which the name table does not grant it", m.Name(), what)
		}
		d.modules = append(d.modules, m)
		// The preview module is this node's Previewer from here on: frames
		// that arrive from now on are judged by it (the kernel answers as a
		// node without previews until then).
		if p, ok := m.(module.Previewer); ok && m.Name() == "preview" {
			d.setPreviewer(p)
		}
		// A module that can take and make payments becomes this node's
		// payer. Type-asserted rather than imported, exactly like
		// Confidential above: the kernel knows that something may be able
		// to price and settle, and never which package it came from.
		if p, ok := m.(module.Payer); ok && grantedTo(m.Name(), module.GrantPaymentSeam) {
			d.mu.Lock()
			d.pay = p
			d.mu.Unlock()
		}
	}
	if names := d.avail.Compiled(); len(names) > 0 {
		log.Printf("anet: modules compiled in: %s (started: %d; without: %s)",
			strings.Join(names, ","), len(d.modules), strings.Join(d.avail.Without(), ","))
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
		return fmt.Errorf("%w %s: %w", errPublicationBlocked, what, err)
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
		return fmt.Errorf("%w %s: %w", errPublicationBlocked, what, err)
	}
	return nil
}

// errPublicationBlocked is a publication screenPublication refused: this
// node's own data, which no retry changes (hub_publication_blocked).
var errPublicationBlocked = errors.New("anet: refusing to publish")

// moduleStopTimeout is the deadline the modules' Stop calls share. It
// bounds a Stop that honours its context (an http.Server.Shutdown, say);
// one that ignores it still holds up Close.
const moduleStopTimeout = 10 * time.Second

// stopModules shuts them down in reverse order of starting. Close calls
// it, once, on every shutdown — including a failed New, for the modules
// that had started.
//
// d.modules is left as it is. It is written only while New runs, so a
// reader needs no lock; a control request that outlives the server's
// shutdown can still be reading it here. And screenPublication takes the
// confidential tokens from it: emptied, a publication made on the way
// out would go unscreened (INV-2).
func (d *Daemon) stopModules(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, moduleStopTimeout)
	defer cancel()
	for i := len(d.modules) - 1; i >= 0; i-- {
		if err := d.modules[i].Stop(ctx); err != nil {
			log.Printf("anet: module %s: stop: %v", d.modules[i].Name(), err)
		}
	}
}

// moduleConfig projects the daemon's config into the per-module raw blocks
// module.Build expects: each module takes its block from cfg.Modules, and
// the daemon grows no typed field for one. (providers.anetlink was the last
// such field; 0.3.0 refuses it as a removed key, and the block is
// modules.anetlink.)
func moduleConfig(cfg Config) (map[string][]byte, error) {
	out := map[string][]byte{}
	for name, raw := range cfg.Modules {
		out[name] = raw
	}
	return out, nil
}

// moduleHost is the narrow surface a module gets, bound to the module's
// name (K-16). It is a wrapper rather than the *Daemon itself so that
// widening it takes an edit here — which is the moment the org lesson says
// has to be visible — and it is bound to a name so that what it hands over
// is decided by the name table row for that name (module.Known(): Grants,
// EvidenceTypes), not by what the module asks for. hostgrants.go composes
// it with the optional interfaces the row grants.
type moduleHost struct {
	d    *Daemon
	name string
	st   *hostState
}

// hostState is what a Host remembers of calls it had to refuse.
type hostState struct {
	mu     sync.Mutex
	denied string
}

func (s *hostState) deniedCall() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.denied
}

// grants reports whether this Host's module holds g in the name table.
func (h moduleHost) grants(g module.Grant) bool { return grantedTo(h.name, g) }

// deny records a call of a method that has no result to refuse with.
func (h moduleHost) deny(what string) {
	log.Printf("anet: module %s: %s is not granted", h.name, what)
	if h.st == nil {
		return
	}
	h.st.mu.Lock()
	if h.st.denied == "" {
		h.st.denied = what
	}
	h.st.mu.Unlock()
}

func (h moduleHost) AID() string { return h.d.AID() }

// Providers is the C1 registry, for a module the name table grants it.
func (h moduleHost) Providers() *provider.Registry {
	if !h.grants(module.GrantProviders) {
		return nil
	}
	return h.d.providers
}

// Available is the daemon's own Availability value (K-1).
func (h moduleHost) Available(name string) bool { return h.d.avail.Available(name) }

// ResolveKEL answers for peers this node holds a record of in
// peer_identity (peerkel.go), and for the node itself. The record exists
// only for peers this node accepted a message from or contacted, so the
// bound is the point rather than a limitation: this node vouches for key
// histories it verified and for nothing else.
//
// A row a module resolves is pinned (reason "issuer") so it is not
// evicted: a module asks because it is verifying an object signed under
// that key history — an organisation credential, a blackboard contribution
// — and losing the record would make the same object unverifiable later.
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
	if !h.grants(module.GrantResolveKEL) {
		return nil, false
	}
	if aid != "" && aid == h.d.AID() {
		return h.d.self.KEL(), true
	}
	kel, ok := h.d.peerKEL(aid)
	if !ok {
		return nil, false
	}
	if row, err := h.d.ix.PeerIdentity(aid); err == nil && row.PinnedReason == "" {
		if err := h.d.notePeer(aid, nil, nil, interactions.PinIssuer, false); err != nil {
			log.Printf("anet: pin %s: %v", aid, err)
		}
	}
	return kel, true
}

// RecordEvidence appends to this node's own chain, for the types the name
// table lists for the module. A type it does not list, and anything under
// anet.policy.* (which only the kernel writes), is module.ErrNotGranted.
func (h moduleHost) RecordEvidence(kind string, payload any) error {
	if info, ok := rowOf(h.name); !ok || !info.MayRecord(kind) {
		return fmt.Errorf("%w: %s may not record %q", module.ErrNotGranted, h.name, kind)
	}
	_, err := h.d.ledger.Append(kind, payload)
	return err
}

var _ module.Host = moduleHost{}

// moduleRouteGuard wraps the handler of a control-plane route: a route the
// name table gives to a module answers 409 module_absent while that module
// is not available, before anything of the request is read (MOD-F2, MOD-P6).
// The route is registered in every build and under every `without`, so the
// route set does not change with either; only the answer does. A route the
// table does not own, which is every kernel route, is returned as it is.
func (d *Daemon) moduleRouteGuard(pattern string, h http.Handler) http.Handler {
	owner, ok := module.RouteOwner(pattern)
	if !ok {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ref := d.avail.Absent(owner, ""); ref != nil {
			writeRefusal(w, http.StatusConflict, ref)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// moduleRoute is the handler of a route whose module answers it through
// module.ControlHandler (K-3): the kernel has authenticated the request and
// bounded its body; the module, when it is started and implements the
// interface, gets the route and the body, and its answer is written here.
// The module that is not there answers nothing: moduleRouteGuard has refused
// the request before this runs, and a started module without the interface
// is a 404.
func (d *Daemon) moduleRoute(owner, route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ch module.ControlHandler
		for _, m := range d.modules {
			if h, ok := m.(module.ControlHandler); ok && m.Name() == owner && module.OwnsRoute(owner, route) {
				ch = h
			}
		}
		if ch == nil {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxControlBody))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request body too large or unreadable"})
			return
		}
		status, resp, err := ch.HandleControl(r.Context(), route, body)
		var ref *a2ashape.Refusal
		switch {
		case errors.As(err, &ref):
			writeRefusal(w, status, ref)
		case err != nil:
			writeJSON(w, status, map[string]string{"error": err.Error()})
		default:
			switch v := resp.(type) {
			case json.RawMessage:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write(v)
			default:
				writeJSON(w, status, resp)
			}
		}
	}
}

// statusMembers adds to a /status answer what the started modules contribute
// through module.StatusContributor (K-4), and the SI-5 values they hold. The
// kernel takes only the key the name table lists for the module; any other
// is dropped and logged.
func (d *Daemon) statusMembers(out map[string]any) {
	for _, m := range d.modules {
		sc, ok := m.(module.StatusContributor)
		if !ok {
			continue
		}
		info, _ := module.Lookup(m.Name())
		key, v := sc.StatusMember()
		if v == nil {
			continue // the module has no member to show now (auto-reply not configured)
		}
		if !info.MayStatus(key) {
			log.Printf("anet: module %s: /status member %q is not in its name-table row; dropped", m.Name(), key)
			continue
		}
		out[key] = v
	}
}

// statusBuild is /status.build (K-4): the modules this binary contains and
// the ones the configuration turned off.
func (d *Daemon) statusBuild() map[string]any {
	return map[string]any{"modules": d.avail.Compiled(), "without": d.avail.Without()}
}
