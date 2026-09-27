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
	// enc is this node's encryption key ring (enckeys.go); publishedKeySeq
	// is the seq of the key set the hub last confirmed holding.
	enc             *encKeyRing
	publishedKeySeq atomic.Uint64
	// clock, when set, replaces the wall clock (unix ms) for the sealed
	// wire. Tests only, through setClock (testhooks.go): the background
	// loops read it while a test sets it, so it is atomic.
	clock atomic.Pointer[func() uint64]
	// hubIDs caches each hub's pinned identity by URL (hub_client.go).
	hubIDMu sync.Mutex
	hubIDs  map[string]hubIdent
	// wireRefuseOnce keeps the "hub below wire 2" notice to one line.
	wireRefuseOnce sync.Once
	// lastSignTS is the time of the last relayauth v2 signature, kept
	// strictly increasing (hubSigned).
	lastSignTS atomic.Uint64

	// Receive pipeline state (receive.go): the per-message lock, the
	// refused-envelope list, outcome counters, the transport rate limit,
	// the refusal-notice limit, the sender keys a refusal is encrypted to,
	// and the capability calls this process is executing.
	rxLocks     keyedLocks
	refused     boundedSet
	rxStats     rxCounters
	p2pLimit    p2pLimiter
	notices     noticeLimiter
	strangers   strangerCache
	reval       revalidating
	running     sync.Map // interaction id -> *runningCall
	logOnceMu   sync.Mutex
	logOnceSeen map[string]bool
	// rxFault, when set, is called inside the receive transaction after the
	// business writes; an error rolls the transaction back. Tests only,
	// through setRxFault (testhooks.go), atomic for the same reason as
	// clock.
	rxFault atomic.Pointer[func(typ string) error]
	// bgWG counts background goroutines that use the store (goBackground),
	// so Close can wait for them before closing it. bgMu orders their
	// start against Close's cancel.
	bgWG sync.WaitGroup
	bgMu sync.Mutex

	// Inbound policy state (inbound.go): admission counters, the window of
	// refusal and acceptance counts, the peer-file write lock, and whether
	// a module declared a backend that accepts untrusted peers.
	admission        admission
	inAgg            inboundAgg
	peerLists        peerLists
	untrustedBackend atomic.Bool
	// bus publishes interaction changes to watchers (eventbus.go).
	bus eventBus
	// outboxKick wakes the retry loop; outboxLocks serializes attempts at
	// one queued message (retry.go).
	outboxKick  chan struct{}
	outboxLocks keyedLocks
	// sbRefused remembers outbound auto-reply turns that failed closed
	// because the sandbox was unavailable (autoreply.go).
	sbRefused sandboxRefused

	// spend holds the payments signed in the last 24 hours, for the
	// spending policy (spend.go); settling tracks the task payments whose
	// settlement is being retried (x402task.go).
	spend    spendBook
	settling sync.Map // interaction id -> struct{}

	// pay is the payment subsystem, when one is compiled in and
	// configured. Nil is the ordinary state of a build with -tags
	// no_x402, and every payment surface says so rather than pretending.
	pay module.Payer

	// quietPeers maps a peer AID to the hub's most recent "this recipient is
	// not collecting its mail" sentence about it. Presence is the mark, so
	// the warning is logged on the transition rather than on every message,
	// and the text is kept rather than discarded because the caller that
	// just sent to that peer is who needs to read it (see QuietPeer).
	quietPeers map[string]string
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
	// netCard is this node's A2A network card as last issued, and what the
	// hub last said about it (a2a_card.go).
	netCard netCardState
	// longCalls bounds how many long-running capability invocations run
	// at once. A buffered channel rather than a counter because the
	// bound has to be enforced at the moment of accepting, and a
	// non-blocking send is exactly "take a slot if one is free, and say
	// so if not" — see runCapabilityCall and maxConcurrentLongCalls.
	longCalls chan struct{}
	// longCallsWG counts the long-running invocations in flight, so
	// shutdown can wait for them instead of pulling the interaction store
	// out from under them. Without it a call still delivering its result
	// when Close ran found d.ix already nil.
	longCallsWG sync.WaitGroup
	// regMu serialises registrations. The card sequence has to increase as
	// the HUB sees it, and a monotonic counter only orders the minting: two
	// registrations in flight at once can arrive in the opposite order, and
	// the earlier-minted one is then refused as a rollback. Holding this
	// across mint-and-send is what makes arrival order match mint order.
	regMu  sync.Mutex
	layout Layout
	self   *identity.Controller
	ix     *interactions.Store

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
	d := &Daemon{layout: layout, cfg: cfg, self: self, ix: ix, ctx: ctx, cancel: cancel,
		stop: make(chan struct{}), autoReplyKick: make(chan struct{}, 1), outboxKick: make(chan struct{}, 1),
		longCalls: make(chan struct{}, maxConcurrentLongCalls)}
	if cfg.migratedInbound {
		// Logged once: the migrated config is saved below without the old
		// key, so the next start reads an explicit inbound block.
		log.Printf("anet: accept_delegations is replaced by inbound.policy (A2A-DESIGN §5.1); this node now runs " +
			"policy closed. Allow a peer with `anet peers allow <aid>` or list public capabilities under inbound.public_capabilities")
	}
	if cfg.rewriteConfig {
		if err := SaveConfig(layout, cfg); err != nil {
			log.Printf("anet: save migrated config: %v", err)
		}
	}
	in := cfg.inbound()
	d.notices.configure(in.RejectNotice.PerPeerPerHour, in.RejectNotice.GlobalPerMin)
	// From here on a failed start goes through Close, which releases
	// whatever had been opened by then — the context, the background
	// loops, the modules started so far, the ledger and the store — in
	// the order it does on an ordinary shutdown. In the daemon process a
	// failed New exits anyway; in a test it would leak all of that.
	//
	// The key ring exists before anything can publish or receive: a peer
	// can only seal to keys this node holds on disk.
	if err := d.setupKeyRing(); err != nil {
		_ = d.Close()
		return nil, err
	}
	led, err := openEvidenceLedger(layout.EvidenceLedgerPath(), self)
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	d.ledger = led
	d.purgeReplay()
	d.goBackground(func() { d.receiveMaintenance(ctx) })
	d.loadCardSeq()
	d.providers = provider.NewRegistry()
	if err := d.startModules(ctx, cfg); err != nil {
		// The modules that did start are stopped; the one that failed
		// cleans up after itself.
		_ = d.Close()
		return nil, err
	}
	// The configuration check runs after the modules start, because a
	// module declares an untrusted backend from Start (A2A-DESIGN §5.1).
	if err := d.validate(cfg); err != nil {
		_ = d.Close()
		return nil, err
	}
	d.recoverInterrupted()
	d.startPayments(ctx)
	d.goBackground(func() { d.outboxLoop(ctx) })
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
	d.goBackground(func() {
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
		if err := d.registerWithHubLocked(ctx, cfg.HubURL, cfg.Name, caps, ""); err != nil {
			// Not an error the operator must act on: the node runs either
			// way, and the next explicit hub-register will carry it.
			log.Printf("anet: could not refresh this node's registration with %s: %v", cfg.HubURL, err)
			return
		}
		if !sameStrings(cfg.Caps, caps) {
			log.Printf("anet: refreshed capabilities at %s: %v", cfg.HubURL, caps)
		}
	})
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

// goBackground runs f on its own goroutine and makes Close wait for it.
// After Close has cancelled the daemon context nothing new is started,
// because f would find the store closed.
func (d *Daemon) goBackground(f func()) bool {
	d.bgMu.Lock()
	defer d.bgMu.Unlock()
	if d.ctx.Err() != nil {
		return false
	}
	d.bgWG.Add(1)
	go func() {
		defer d.bgWG.Done()
		f()
	}()
	return true
}

// AID is the daemon's agent identifier.
func (d *Daemon) AID() string { return d.self.AID() }

// RequestStop asks ServeControl to shut down gracefully (used by the `anet stop` control command). Safe
// to call multiple times / concurrently; ServeControl returns after this, unwinding runDaemon's Close.
func (d *Daemon) RequestStop() { d.stopOnce.Do(func() { close(d.stop) }) }

// Close stops the background loops and the modules, then closes the
// evidence ledger and the interactions store. Idempotent.
func (d *Daemon) Close() error {
	var err error
	d.closeOnce.Do(func() {
		// Cancel first: a long-running invocation derives its context
		// from d.ctx, so this is what tells one to stop. Then wait for it
		// to actually be done, because what it does on the way out is
		// write its result — to the store this function is about to
		// close.
		d.bgMu.Lock()
		d.cancel()
		d.bgMu.Unlock()
		d.bgWG.Wait()
		drained := waitFor(&d.longCallsWG, longCallDrainTimeout)
		// The modules stop once the kernel's own work is done (a long
		// call that did not drain may still be in one), and before the
		// ledger closes, because a module may record evidence on its way
		// out. Their Start context is d.ctx, already cancelled, so Stop
		// is for what that does not reach: a connection, a listener, a
		// goroutine of the module's own.
		d.stopModules(context.Background())
		if d.ledger != nil {
			_ = d.ledger.Close()
		}
		d.mu.Lock()
		ix := d.ix
		// Only clear the handle when nothing is still using it. A
		// straggler that finds the store closed gets an error it can
		// log; one that finds it nil takes the process down with it, and
		// shutdown is exactly when that is least useful.
		if drained {
			d.ix = nil
		}
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

// signTaskDoc builds a minimal signed TaskDoc for a goal (the delegation request object). nonce is
// the task nonce (A2A-DESIGN §2 X4), carried as a private context inside the signed preimage.
func (d *Daemon) signTaskDoc(goal, nonce string) ([]byte, *aobj.Envelope, error) {
	task := tsir.Task{Intent: tsir.Intent{Summary: goal, Body: goal}}
	if nonce != "" {
		task.Contexts = []tsir.Context{nonceContext(nonce)}
	}
	td := &tsir.TaskDoc{Version: tsir.VersionPair{Major: 1}, Tasks: []tsir.Task{task}}
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

// longCallDrainTimeout bounds how long shutdown waits for in-flight
// long-running invocations. Generous enough for a cancelled provider to
// return and write its result, short enough that `anet stop` does not
// appear to hang — a caller who has to wait out an hour-long build to
// stop the daemon will reach for SIGKILL, which is the outcome the wait
// exists to avoid.
const longCallDrainTimeout = 10 * time.Second

// waitFor waits on wg for at most d, reporting whether it drained.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}
