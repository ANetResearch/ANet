package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ANetResearch/ANetCore/aobj"
	"github.com/ANetResearch/ANetCore/coredet"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/official"
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
	// clk is what the clock gate knows besides the ring: the latest check
	// against the hub and the previous maintenance period (enckeys.go).
	clk clockState
	// mbox is this node's mailbox signing key and mboxSt what the daemon
	// knows about getting it to the hub (mbox.go).
	mbox   *mboxKey
	mboxSt mboxState
	// caps is the delivery credential policy's state in memory: recent
	// downgrades, the direct delivery buckets and the withdrawal push
	// (caps.go, revoke_push.go).
	caps capsState
	// clock, when set, replaces the wall clock (unix ms) for the sealed
	// wire. Tests only, through setClock (testhooks.go): the background
	// loops read it while a test sets it, so it is atomic.
	clock atomic.Pointer[func() uint64]
	// monoClock, when set, replaces the boot clock (monoMS, bootclock.go)
	// the clock gate counts running time on. Tests only, through
	// setMonoClock.
	monoClock atomic.Pointer[func() uint64]
	// keyHold, when set, answers keysHeld (enckeys.go): whether key sets
	// are held back between an emergency rotation's E1 and E2. Tests only,
	// through setKeyHold, until the emergency rotation answers it.
	keyHold atomic.Pointer[func() bool]
	// drain is what the hub mailbox polls say about the epoch keys: how far
	// each hub's mailbox was read, and what it holds back (relay.go).
	drain drainState
	// startLevel is the clock gate's level at the first maintenance period
	// of the process (setupKeyRing).
	startLevel clockLevel
	// hubIDs caches each hub's pinned identity by URL (hub_client.go).
	hubIDMu sync.Mutex
	hubIDs  map[string]hubIdent
	// hubc is the daemon's HTTP client of its hub: the signed and the
	// anonymous transports and the first-contact sessions (hub_client.go,
	// session_client.go).
	hubc hubClient
	// lastSignTS is the time of the last relayauth v2 signature, kept
	// strictly increasing (hubSigned).
	lastSignTS atomic.Uint64

	// Receive pipeline state (receive.go): the per-message lock, the
	// refused-envelope list, outcome counters, the limit on notices after
	// the fact, the limit on answers hurried for redeliveries, and the
	// capability calls this process is executing. The direct path's rate
	// buckets are caps.rx (caps.go).
	rxLocks   keyedLocks
	rxIXLocks keyedLocks // (from, ix) of a delegation, taken after rxLocks
	refused   refusedLRU
	rxStats   rxCounters
	notices   noticeLimiter
	resends   resendLimiter
	// unknownKeyDrops counts the mailbox envelopes sealed to an epoch key
	// this node does not hold (unknownKeyDrops24h).
	unknownKeyDrops hourCounts
	// peers is the resolver's state: where this node learns another
	// agent's material without naming it to a hub (peer_resolve.go).
	peers       peerResolver
	running     sync.Map // interaction id -> *runningCall
	logOnceMu   sync.Mutex
	logOnceSeen map[string]bool
	// join is the hub join loop's state (hubjoin_daemon.go).
	join hubJoin
	// idGate is what the outbound gate reads besides the identity file and
	// the join record (identity_state.go).
	idGate identityGateInputs
	// rxFault, when set, is called inside the receive transaction after the
	// business writes; an error rolls the transaction back. Tests only,
	// through setRxFault (testhooks.go), atomic for the same reason as
	// clock.
	rxFault atomic.Pointer[func(typ string) error]
	// rxUnlockHook, when set, is called each time the receive pipeline has
	// released an envelope's locks. Tests only, through setRxUnlockHook.
	rxUnlockHook atomic.Pointer[func()]
	// ixLookup, when set, is the interaction key lookup receive step 3a
	// uses (ixKeys). Tests only, through setIXLookup (testhooks.go).
	ixLookup atomic.Pointer[seal.IXKeyLookup]
	// cardGrants are the reply grants of card requests waiting for their
	// answer (rx_seams.go).
	cardGrants cardReplyGrants
	// cardAnswerHook, when set, is handed every card answer the receive
	// pipeline would send, in place of sending it. Tests only, through
	// setCardAnswerHook.
	cardAnswerHook atomic.Pointer[func(to, ix string, body []byte)]
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
	// inFeed is the modules subscribed to inbound tasks (inbound_tasks.go).
	inFeed inboundFeed
	// officials is the official-agent manifest built into this binary,
	// once verified (official.go); nil when it did not verify, which marks
	// no agent official.
	officials atomic.Pointer[official.Manifest]
	// outboxKick wakes the retry loop; outboxLocks serializes attempts at
	// one queued message (retry.go); outboxMem is what the engine keeps in
	// memory (outbox_attempt.go).
	outboxKick  chan struct{}
	outboxLocks keyedLocks
	outboxMem   outboxMem
	// outboxHold, set by a test (setOutboxHold), skips the passes of the
	// outbox loop and of the no-response sweep.
	outboxHold atomic.Bool
	// previewer is the stream_preview coalescer and preview cache
	// (preview.go, set with setPreviewer); none leaves previews
	// unavailable (previewsUnavailable).
	previewer atomic.Pointer[previewCoalescer]
	// streamPreviewFn is this node's stream_preview setting ("off", "on"
	// or "with_reasoning"); nil reads as "off" (preview_card.go).
	// v030-todo(UX-9): the config key reads through here.
	streamPreviewFn func() string
	// sbRefused remembers outbound auto-reply turns that failed closed
	// because the sandbox was unavailable (autoreply.go).
	sbRefused sandboxRefused
	// budget counts the auto-reply runs for untrusted peers per local day
	// and holds their slots (sandbox_budget.go, LEC BU); sandboxSwept is
	// the sweep of what an earlier process's sandboxed runs left (SB-9).
	budget       runBudget
	sandboxSwept sync.Once

	// spend holds the payments signed in the last 24 hours, for the
	// spending policy (spend.go); settling tracks the task payments whose
	// settlement is being retried (x402task.go).
	spend    spendBook
	settling sync.Map // interaction id -> struct{}

	// pay is the payment subsystem, when one is compiled in and
	// configured. Nil is the ordinary state of a build with -tags
	// no_x402, and every payment surface says so rather than pretending.
	pay module.Payer

	// quietPeers holds the peers the hub last said have stopped collecting
	// their mail (recipient_quiet). The mark is logged on the transition
	// rather than on every message, and kept because the caller that just
	// sent to that peer is who needs to read it (see QuietPeer).
	quietPeers map[string]bool
	// transportState carries the optional delivery paths modules add.
	transportState
	// modules are the optional subsystems this build carries.
	modules []module.Module
	// ctlAuth is the control plane's credential in force (control_api.go); rotateLocalTokens replaces
	// it.
	ctlAuth atomic.Pointer[ctlAuth]
	// autoReplyWrite makes POST /autoreply's read of the configuration, its merge and its save one step,
	// so that two settings sent at once both land (control_api.go hAutoReply).
	autoReplyWrite sync.Mutex
	// lastCardSeq is the highest sequence this process has minted for its
	// own AgentCard. See cardSeq — the clock alone cannot keep the number
	// strictly increasing, and the hub refuses a card that does not.
	lastCardSeq atomic.Uint64
	// netCard is this node's A2A network card as last issued, and what the
	// hub last said about it (a2a_card.go).
	netCard netCardState
	// extCard is this node's signed extended cards and who asked for one
	// lately (extcard.go).
	extCard extCardState
	// extFetch is the extended cards this node asked agents for and waits
	// on (extcard_fetch.go).
	extFetch extFetchState
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
	mu  sync.Mutex
	cfg Config
	// cfgWrite serializes every write of config.json (policy writes,
	// spending limits, auto-reply, profile, hub registration, the control
	// address): each reads the config, saves the changed copy, and only
	// then puts the field it changed in force (updateConfig). A write that
	// failed to save is not in force and a later save does not carry it to
	// disk; and two writes at once can neither leave memory and disk
	// disagreeing — one saving a copy read before the other took effect —
	// nor lose one of the two (redteam F9).
	cfgWrite      sync.Mutex
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
	// relayCur is where the next mailbox poll starts (relay.go).
	relayCur relayCursor

	// stop is closed by RequestStop to ask ServeControl to shut down (the `anet stop` control command),
	// so a resident daemon can be stopped gracefully without kill/SIGTERM.
	stop     chan struct{}
	stopOnce sync.Once

	closeOnce sync.Once

	// started is when this process opened the store; an interaction
	// created before it was left by an earlier process (wire_edges.go).
	started time.Time

	// idOps is the state of the identity's key events (identity_ops.go).
	idOps identityOps

	// ready is closed when New has finished: every module started, startup
	// recovery (recoverInterrupted, startPayments) done. A transport module
	// delivers from the moment its Start runs, so its deliveries wait for
	// this (awaitReady, [redteam:F30]).
	ready chan struct{}
}

// New builds the daemon: load config + identity, open the interactions store, and (if a Hub is
// configured) start the relay poll loop. It does not block; call Close to stop.
func New(layout Layout) (*Daemon, error) {
	// A data directory an earlier or a newer anet wrote is refused before
	// anything is written to it (FS-S1, core CORE-V4).
	if err := CheckDataDirForStart(layout); err != nil {
		return nil, err
	}
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
	if errors.Is(err, interactions.ErrOldStore) {
		return nil, oldDataDirRefusal()
	}
	if errors.Is(err, interactions.ErrNewerStore) {
		return nil, newerDataDirRefusal()
	}
	if err != nil {
		return nil, fmt.Errorf("anet: open interactions store: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{layout: layout, cfg: cfg, self: self, ix: ix, ctx: ctx, cancel: cancel, started: time.Now(),
		stop: make(chan struct{}), autoReplyKick: make(chan struct{}, 1), outboxKick: make(chan struct{}, 1),
		longCalls: make(chan struct{}, maxConcurrentLongCalls), ready: make(chan struct{})}
	d.caps.push.kick = make(chan struct{}, 1)
	d.loadOfficials()
	// This node's latest witness notice, checked again, so the outbound gate
	// does not wait for the first hub answer after a restart (KEL D-50).
	d.loadKeyStateNotice()
	if cfg.rewriteConfig {
		if err := SaveConfig(layout, cfg); err != nil {
			log.Printf("anet: save migrated config: %v", err)
		}
	}
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
	// Where the identity's key events stand: what a crash cut short is
	// finished first (KEL D-35), by the identity loop started below.
	d.initIdentityOps()
	d.purgeReplay()
	d.startPreviews(ctx) // before anything can arrive (preview.go)
	d.goBackground(func() { d.receiveMaintenance(ctx) })
	d.goBackground(func() { d.keyMaintenanceLoop(ctx) })
	d.goBackground(func() { d.mboxLoop(ctx) }) // mbox.go
	// The peer lists may have changed while the daemon was stopped, or it
	// stopped between a list write and its withdrawal (SS §4.3.8 item 1).
	if _, err := d.reconcileCapsWithLists(); err != nil {
		log.Printf("anet: withdraw delivery credentials by the peer lists: %v", err)
	}
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
	// A stopped or deactivated identity whose close a crash cut short is
	// closed first (engine §6.3 step 2); then the rows an attempt left
	// marked count as handed over (step 3), before anything is sent.
	d.closeLeftAtStart()
	gone := d.identityRefusesInbound()
	if gone {
		// A stopped or deactivated identity sends and receives nothing
		// directly (KEL D-1): the transports close, and no poll starts.
		d.closeP2P()
	}
	d.finishLeftRollback() // a local state rollback a crash cut short (FS-B3)
	d.recoverOutboxAttempts()
	d.recoverInterrupted()
	d.startPayments(ctx)
	d.goBackground(func() { d.outboxLoop(ctx) })
	d.goBackground(func() { d.noResponseLoop(ctx) }) // no_response.go
	// The join loop registers again at every start (hubjoin_daemon.go,
	// LEC AD-5); the relay loop polls once the hub has registered this
	// node.
	d.initJoin(cfg)
	// The withdrawal push reads the join record, so it starts after it.
	d.goBackground(func() { d.revokeLoop(ctx) }) // revoke_push.go
	// The federation hub KEL set follows the hub's /hub/kels (KEL D-24).
	d.goBackground(func() { d.hubKELSyncLoop(ctx) }) // peerkel.go
	if cfg.HubURL != "" && !gone {
		d.startRelayLoop(cfg.HubURL)
	}
	d.startJoinLoop()
	d.startIdentityLoop()
	if cfg.AutoReply != nil {
		d.startAutoReply(*cfg.AutoReply)
	}
	d.inFeed.open() // modules' A2A backends get tasks from here on (inbound_tasks.go)
	// Deliveries a transport module received during start-up are processed
	// from here on, after recovery has classified what the previous process
	// left: processed earlier, a call accepted in that window could be
	// taken for a leftover, and a capability or payment module not started
	// yet answered for as missing ([redteam:F30]).
	close(d.ready)
	return d, nil
}

// refreshRegistration re-publishes what this node serves, in the
// background: the join loop registers again now (LEC AD-5).
//
// The capability list a hub advertises is folded in at registration time.
// Modules are wired at start, so their capabilities change whenever the
// build or the config changes — and a restart, which is exactly how an
// operator applies such a change, has to tell the hub, or the hub goes on
// advertising a capability the node has dropped and a delegation sent on
// the strength of that lands where nobody serves it. The start-up attempt of
// the join loop is that refresh; this one is for a change while running
// (cardInputsChanged).
//
// Only a registered node re-registers here: a node waiting for admission or
// for its hub carries the change in its next scheduled attempt, and a
// refused one waits for `anet hub-register` (AD-3).
func (d *Daemon) refreshRegistration() {
	hub := d.config().HubURL
	if hub == "" {
		return
	}
	d.join.mu.Lock()
	r := d.join.rec
	if r == nil || !sameHub(r.Hub, hub) {
		// A hub the loop has no record of (config.json changed under the
		// running daemon): its first attempt is this one.
		d.join.rec = freshJoinRecord(hub, time.Now())
		r = d.join.rec
	}
	registered, fresh := r.State == JoinRegistered, r.RegisteredAt == 0 && r.Attempts == 0
	d.join.mu.Unlock()
	if registered || fresh {
		d.kickJoin()
	}
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
