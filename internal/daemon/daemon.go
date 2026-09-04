package daemon

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// Daemon is one operator's anet v0.1 process: a self-certifying identity (KEL), a durable local
// delegation log (interactions), and a client of the official Hub relay. It runs NO model and holds NO
// P2P transport — all traffic (register, find, delegate, deliver, review) flows through the central Hub.
// The actual work is done by the operator's EXTERNAL agent (cursor/claude/openclaw, or any script),
// which reads tasks via the CLI (`inbox`/`thread`) and drives the conversation with `anet message` /
// `anet end`.
type Daemon struct {
	// peers remembers key histories verified on the ingest path.
	peers *peerKELs

	// pay is the payment subsystem, when one is compiled in and
	// configured. Nil is the ordinary state of a build with -tags
	// no_x402, and every payment surface says so rather than pretending.
	pay module.Payer

	// quietPeers remembers which peers the hub has reported as no longer
	// collecting their mail, so the warning is logged on the transition
	// rather than on every message.
	quietPeers map[string]bool
	// transportState carries the optional delivery paths modules add.
	transportState
	// modules are the optional subsystems this build carries.
	modules []module.Module
	// wireWarnOnce keeps the C2 version-mismatch notice to one line.
	wireWarnOnce sync.Once
	// lastCardSeq is the highest sequence this process has minted for its
	// own AgentCard. See cardSeq — the clock alone cannot keep the number
	// strictly increasing, and the hub refuses a card that does not.
	lastCardSeq atomic.Uint64
	// regMu serialises registrations. The card sequence has to increase as
	// the HUB sees it, and a monotonic counter only orders the minting: two
	// registrations in flight at once can arrive in the opposite order, and
	// the earlier-minted one is then refused as a rollback. Holding this
	// across mint-and-send is what makes arrival order match mint order.
	regMu  sync.Mutex
	layout Layout
	self   *identity.Controller
	ix     *interactions.Store

	// cachedHubAID names the ledger this node settles on, fetched once
	// from the hub. Two hubs are two networks and a credit on one is not
	// a credit on the other, so an authorization has to say which.
	cachedHubAID string
	// providers is the C1 capability registry (docs/CONTRACTS-zh.md): the only doorway
	// through which this daemon acquires callable capabilities.
	providers *provider.Registry

	// ledger is the local P6 evidence chain (C5): capability effects and
	// issued receipts, signed and fork-evident.
	ledger *evidenceLedger

	// ctx is the daemon's lifetime context (the relay poll loop runs under it); cancel stops it on Close.
	ctx    context.Context
	cancel context.CancelFunc

	// mu guards cfg and the relay-loop lifecycle (cfg + hub target can change via hub-register).
	mu            sync.Mutex
	cfg           Config
	relayStop     context.CancelFunc // cancels the currently-running relay poll loop, if any
	autoReplyStop context.CancelFunc // cancels the currently-running auto-reply loop, if any

	// autoReplyKick wakes the auto-reply loop the instant an inbound delegate/message lands (via the
	// relay poll), instead of waiting up to a full poll interval. Buffered (cap 1) so bursts coalesce
	// and a send never blocks the relay loop. Created in New; safe to signal even when auto-reply is off.
	autoReplyKick chan struct{}

	// pollMu serializes mailbox polls so the background loop (generous timeout, does the heavy inbound
	// transfers) and a read command's best-effort freshness poll never run concurrently — otherwise two
	// goroutines would download the same large attachment at once and could double-ingest it.
	pollMu sync.Mutex

	// stop is closed by RequestStop to ask ServeControl to shut down (the `anet stop` control command),
	// so a resident daemon can be stopped gracefully without kill/SIGTERM.
	stop     chan struct{}
	stopOnce sync.Once

	closeOnce sync.Once
}

// New builds the daemon: load config + identity, open the interactions store, and (if a Hub is
// configured) start the relay poll loop. It does not block; call Close to stop.
func New(layout Layout) (*Daemon, error) {
	if err := layout.EnsureRoot(); err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(layout)
	if err != nil {
		return nil, err
	}
	self, err := LoadOrGenerateIdentity(layout)
	if err != nil {
		return nil, err
	}
	ix, err := interactions.Open(layout.InteractionsDir())
	if err != nil {
		return nil, fmt.Errorf("anet: open interactions store: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{layout: layout, cfg: cfg, self: self, ix: ix, ctx: ctx, cancel: cancel, peers: newPeerKELs(),
		stop: make(chan struct{}), autoReplyKick: make(chan struct{}, 1)}
	led, err := openEvidenceLedger(layout.EvidenceLedgerPath(), self)
	if err != nil {
		return nil, err
	}
	d.ledger = led
	d.loadCardSeq()
	d.providers = provider.NewRegistry()
	if err := d.startModules(ctx, cfg); err != nil {
		cancel()
		return nil, err
	}
	if cfg.HubURL != "" {
		d.startRelayLoop(cfg.HubURL)
		d.refreshRegistration()
	}
	if cfg.AutoReply != nil {
		d.startAutoReply(*cfg.AutoReply)
	}
	return d, nil
}

// refreshRegistration re-publishes what this node serves, in the background.
//
// The capability list a hub advertises is folded in at hub-register time
// and never afterwards. Modules are wired at start, so their capabilities
// change whenever the build or the config changes — and a restart, which
// is exactly how an operator applies such a change, did not tell the hub.
// The heartbeat kept refreshing last_seen, so the node looked healthy
// while its advertised capabilities were whatever they had been at the
// last explicit registration.
//
// The removal direction is the worse one. A hub goes on advertising a
// capability the node has dropped; `anet find --cap` returns it as a live
// answer; a delegation sent on the strength of that lands in the path
// nobody serves, where the requester gets no error, the provider logs
// nothing and neither chain records anything.
//
// Background and non-fatal on purpose: a daemon must come up when its hub
// is unreachable, and this is a refresh of something the hub already has,
// not a precondition for running. Found by the release matrix.
// The config is read inside the goroutine, not captured at start: an
// explicit `hub-register` can land first, and a refresh carrying the
// snapshot from before it would overwrite what the operator just set.
func (d *Daemon) refreshRegistration() {
	go func() {
		ctx, cancel := context.WithTimeout(d.ctx, hubCallTimeout)
		defer cancel()
		// Read and register under one lock, so an explicit hub-register
		// racing this cannot be overwritten by a stale snapshot.
		d.regMu.Lock()
		defer d.regMu.Unlock()
		cfg := d.config()
		if cfg.HubURL == "" {
			return // left the hub between start and now
		}
		caps := withServedCapabilities(cfg.Caps, d.providers)
		if err := d.registerWithHubLocked(ctx, cfg.HubURL, cfg.Name, caps, cfg.GuestQuota(), ""); err != nil {
			// Not an error the operator must act on: the node runs either
			// way, and the next explicit hub-register will carry it.
			log.Printf("anet: could not refresh this node's registration with %s: %v", cfg.HubURL, err)
			return
		}
		if !sameStrings(cfg.Caps, caps) {
			log.Printf("anet: refreshed capabilities at %s: %v", cfg.HubURL, caps)
		}
	}()
}

// sameStrings reports whether two capability lists have the same members.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
		if seen[x] < 0 {
			return false
		}
	}
	return true
}

// AID is the daemon's agent identifier.
func (d *Daemon) AID() string { return d.self.AID() }

// RequestStop asks ServeControl to shut down gracefully (used by the `anet stop` control command). Safe
// to call multiple times / concurrently; ServeControl returns after this, unwinding runDaemon's Close.
func (d *Daemon) RequestStop() { d.stopOnce.Do(func() { close(d.stop) }) }

// Close stops the relay loop and closes the interactions store. Idempotent.
func (d *Daemon) Close() error {
	var err error
	d.closeOnce.Do(func() {
		d.cancel()
		if d.ledger != nil {
			_ = d.ledger.Close()
		}
		d.mu.Lock()
		ix := d.ix
		d.ix = nil
		d.mu.Unlock()
		if ix != nil {
			err = ix.Close()
		}
	})
	return err
}

// config returns a snapshot of the current config under the lock.
func (d *Daemon) config() Config {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
}

// SetAcceptDelegations toggles whether this daemon stores inbound tasks delegated to it, and persists the
// change (config.json) so it survives restarts. It takes effect immediately: the running relay loop reads
// the live config on each inbound message, so no restart is needed. Returns the new value.
func (d *Daemon) SetAcceptDelegations(enabled bool) (bool, error) {
	d.mu.Lock()
	d.cfg.AcceptDelegations = &enabled
	cfg := d.cfg
	d.mu.Unlock()
	if err := SaveConfig(d.layout, cfg); err != nil {
		return false, err
	}
	return enabled, nil
}

// signTaskDoc builds a minimal signed TaskDoc for a goal (the delegation request object).
func (d *Daemon) signTaskDoc(goal string) ([]byte, *aobj.Envelope, error) {
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{{Intent: tsir.Intent{Summary: goal, Body: goal}}}}
	if err := td.Sign(d.self); err != nil {
		return nil, nil, err
	}
	doc, err := coredet.Marshal(td)
	if err != nil {
		return nil, nil, err
	}
	return doc, td.Envelope, nil
}

func nowMillis() int64 { return time.Now().UnixMilli() }
