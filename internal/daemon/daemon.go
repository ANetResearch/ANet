package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/protocol/aobj"
	"github.com/ANetResearch/ANet/internal/protocol/coredet"
	"github.com/ANetResearch/ANet/internal/protocol/identity"
	"github.com/ANetResearch/ANet/internal/protocol/tsir"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Daemon is one operator's anet v0.1 process: a self-certifying identity (KEL), a durable local
// delegation log (interactions), and a client of the official Hub relay. It runs NO model and holds NO
// P2P transport — all traffic (register, find, delegate, deliver, review) flows through the central Hub.
// The actual work is done by the operator's EXTERNAL agent (cursor/claude/openclaw, or any script),
// which reads tasks via the CLI (`inbox`/`thread`) and drives the conversation with `anet message` /
// `anet end`.
type Daemon struct {
	layout Layout
	self   *identity.Controller
	ix     *interactions.Store

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
	d := &Daemon{layout: layout, cfg: cfg, self: self, ix: ix, ctx: ctx, cancel: cancel,
		stop: make(chan struct{}), autoReplyKick: make(chan struct{}, 1)}
	if cfg.HubURL != "" {
		d.startRelayLoop(cfg.HubURL)
	}
	if cfg.AutoReply != nil {
		d.startAutoReply(*cfg.AutoReply)
	}
	return d, nil
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
