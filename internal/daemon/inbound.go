package daemon

// inbound.go is the inbound policy (A2A-DESIGN §5): who may delegate to this
// node, who may drive its local agents, which capabilities anyone may call
// and with what quotas, and what a refused sender is told.
//
// The decision for an anet.delegate/1 is made in the receive pipeline's
// authorization step (§3.6 step 9) and follows §5.2 in order:
//
//	1 from ∈ deny                              refuse (closed: the same answer as row 6)
//	2 capability call, capability is public    kernel admission (Admit), then trust=public_cap
//	3 from ∈ allow ∪ trust                     trust=peer
//	4 policy approve                           hold in the pending queue
//	5 policy open                              capability call: refuse; otherwise trust=public
//	6 otherwise                                refuse, anet.reason=not_accepting
//
// The three peer files are read on every decision, so an edit takes effect
// on the next message without a restart. A missing file is an empty list.
//
// Refusals cost the refused party a bounded amount of this node's work: the
// reply is rate limited per peer and daemon-wide and dropped silently when
// either limit is reached; it is never queued or retried. Refusals are
// counted into one anet.delegation.refused_summary evidence event per
// window rather than one event each, and the per-request detail goes to a
// rotating local log that is not on the chain. The same aggregation applies
// to delegations accepted from parties this node did not name (public and
// public_cap), because anyone can cause those.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Inbound policies (A2A-DESIGN §5.1).
const (
	PolicyClosed  = "closed"
	PolicyApprove = "approve"
	PolicyOpen    = "open"
)

// InboundConfig is the inbound block of config.json.
type InboundConfig struct {
	// Policy is closed (default), approve or open.
	Policy string `json:"policy"`
	// AllowFile lists peers that may delegate; TrustFile peers that may in
	// addition drive this node's exec auto-reply and A2A backends; DenyFile
	// peers refused whatever else says. One AID per line, '#' starts a
	// comment. A relative path is relative to the data directory.
	AllowFile string `json:"allow_file"`
	DenyFile  string `json:"deny_file"`
	TrustFile string `json:"trust_file"`
	// PublicCapabilities are the capabilities anyone may call, each with
	// its quotas.
	PublicCapabilities []PublicCapability `json:"public_capabilities"`
	RejectNotice       RejectNoticeConfig `json:"reject_notice"`
	Pending            PendingQueueConfig `json:"pending"`
}

// PublicCapability is one capability anyone may call. A zero limit takes
// the default shown in A2A-DESIGN §5.1.
type PublicCapability struct {
	ID              string `json:"id"`
	PerCallerPerMin int    `json:"per_caller_per_min,omitempty"`
	PerCallerPerDay int    `json:"per_caller_per_day,omitempty"`
	GlobalPerMin    int    `json:"global_per_min,omitempty"`
	MaxInflight     int    `json:"max_inflight,omitempty"`
	MaxArgsBytes    int    `json:"max_args_bytes,omitempty"`
	// Evidence is what the chain keeps of a call of this capability
	// (trust public_cap, whoever the caller): "cid" (the default) keeps
	// the result CID and the metrics, "full" also keeps the provenance
	// with the observed state, which is the capability's whole answer.
	// See evidence_mode.go.
	Evidence string `json:"evidence,omitempty"`
}

// RejectNoticeConfig bounds refusal replies. The global rate is kept well
// below the hub's per-sender budget (20/s), so refusals cannot use up this
// node's ability to send results.
type RejectNoticeConfig struct {
	PerPeerPerHour int `json:"per_peer_per_hour"`
	GlobalPerMin   int `json:"global_per_min"`
}

// PendingQueueConfig bounds the approval queue.
type PendingQueueConfig struct {
	MaxTotal   int `json:"max_total"`
	MaxPerPeer int `json:"max_per_peer"`
	TTLHours   int `json:"ttl_hours"`
}

// Defaults of the inbound block.
const (
	defaultAllowFile     = "peers.allow"
	defaultDenyFile      = "peers.deny"
	defaultTrustFile     = "peers.trust"
	defaultPendingTotal  = 200
	defaultPendingPeer   = 3
	defaultPendingTTLHrs = 72
	// pendingFollowupsMax bounds the messages kept for one held delegation.
	pendingFollowupsMax = 5

	defaultCapPerCallerPerMin = 60
	defaultCapPerCallerPerDay = 2000
	defaultCapGlobalPerMin    = 1200
	defaultCapMaxInflight     = 16
	defaultCapMaxArgsBytes    = 4096
)

// defaultInbound is the inbound block of a fresh install: closed, empty
// lists, no public capability (SI-5).
func defaultInbound() InboundConfig {
	c := InboundConfig{Policy: PolicyClosed, PublicCapabilities: []PublicCapability{}}
	c.normalize()
	return c
}

// normalize fills defaults in place. It does not change the policy of a
// config that names one; an unknown policy is left for validation to
// refuse.
func (c *InboundConfig) normalize() {
	if c.Policy == "" {
		c.Policy = PolicyClosed
	}
	if c.AllowFile == "" {
		c.AllowFile = defaultAllowFile
	}
	if c.DenyFile == "" {
		c.DenyFile = defaultDenyFile
	}
	if c.TrustFile == "" {
		c.TrustFile = defaultTrustFile
	}
	if c.PublicCapabilities == nil {
		c.PublicCapabilities = []PublicCapability{}
	}
	if c.RejectNotice.PerPeerPerHour <= 0 {
		c.RejectNotice.PerPeerPerHour = noticePerPeerPerHour
	}
	if c.RejectNotice.GlobalPerMin <= 0 {
		c.RejectNotice.GlobalPerMin = noticeGlobalPerMin
	}
	if c.Pending.MaxTotal <= 0 {
		c.Pending.MaxTotal = defaultPendingTotal
	}
	if c.Pending.MaxPerPeer <= 0 {
		c.Pending.MaxPerPeer = defaultPendingPeer
	}
	if c.Pending.TTLHours <= 0 {
		c.Pending.TTLHours = defaultPendingTTLHrs
	}
}

// limits returns p with zero fields replaced by the defaults.
func (p PublicCapability) limits() PublicCapability {
	if p.PerCallerPerMin <= 0 {
		p.PerCallerPerMin = defaultCapPerCallerPerMin
	}
	if p.PerCallerPerDay <= 0 {
		p.PerCallerPerDay = defaultCapPerCallerPerDay
	}
	if p.GlobalPerMin <= 0 {
		p.GlobalPerMin = defaultCapGlobalPerMin
	}
	if p.MaxInflight <= 0 {
		p.MaxInflight = defaultCapMaxInflight
	}
	if p.MaxArgsBytes <= 0 {
		p.MaxArgsBytes = defaultCapMaxArgsBytes
	}
	if p.Evidence == "" {
		p.Evidence = EvidenceCID
	}
	return p
}

// inbound returns the effective inbound block.
func (c Config) inbound() InboundConfig {
	if c.Inbound == nil {
		return defaultInbound()
	}
	in := *c.Inbound
	in.normalize()
	return in
}

// publicCapability returns the public capability entry for capID.
func (c InboundConfig) publicCapability(capID string) (PublicCapability, bool) {
	for _, p := range c.PublicCapabilities {
		if p.ID == capID && capID != "" {
			return p.limits(), true
		}
	}
	return PublicCapability{}, false
}

// --- configuration validation (A2A-DESIGN §5.1) ---

// ErrPolicyConflict is returned when a configuration would accept work from
// anyone (policy open) while also running a local agent, or forwarding to
// a backend, for peers this node does not trust. The control plane answers
// it with 409.
var ErrPolicyConflict = errors.New("anet: inbound policy conflict")

// validatePolicy is the one check of the inbound configuration. It runs at
// daemon start, on POST /autoreply and on every inbound policy write, so
// the outcome does not depend on which of two settings was written first.
//
// untrustedBackend is the declaration a module makes through
// module.Host.DeclareUntrustedBackend: a configured A2A backend accepts
// peers that are not on the trust list. The kernel reads that declaration
// and nothing of the module's own configuration.
func validatePolicy(c Config, untrustedBackend bool) error {
	in := c.inbound()
	switch in.Policy {
	case PolicyClosed, PolicyApprove, PolicyOpen:
	default:
		return fmt.Errorf("anet: inbound.policy %q is not one of closed, approve, open", in.Policy)
	}
	if ar := c.AutoReply; ar != nil {
		switch ar.Untrusted {
		case "", UntrustedOff, UntrustedSandbox:
		default:
			return fmt.Errorf("anet: auto_reply.untrusted %q is not one of off, sandbox", ar.Untrusted)
		}
	}
	for _, p := range in.PublicCapabilities {
		if strings.TrimSpace(p.ID) == "" {
			return fmt.Errorf("anet: inbound.public_capabilities has an entry without an id")
		}
		if err := validEvidenceMode(p); err != nil {
			return err
		}
	}
	if in.Policy != PolicyOpen {
		return nil
	}
	if ar := c.AutoReply; ar != nil && ar.Backend == "exec" && ar.UntrustedMode() != UntrustedOff {
		return fmt.Errorf("%w: policy open accepts tasks from anyone, and auto_reply.untrusted=%s runs the "+
			"local agent for peers not on the trust list; change one of the two", ErrPolicyConflict, ar.Untrusted)
	}
	if untrustedBackend {
		return fmt.Errorf("%w: policy open accepts tasks from anyone, and an A2A backend is configured with "+
			"accept_untrusted; change one of the two", ErrPolicyConflict)
	}
	return nil
}

// validate runs validatePolicy against this daemon's module declarations.
func (d *Daemon) validate(c Config) error {
	return validatePolicy(c, d.untrustedBackend.Load())
}

// --- peer lists ---

// peerLists serializes writes to the three peer files. Reads are not
// locked: a file is replaced atomically, so a reader sees the old or the
// new list.
type peerLists struct {
	mu sync.Mutex
	// skippedPaid holds the paid interactions a deny already reported as
	// left running (0017 Q10), so the minute sweep reports each once.
	skippedPaid sync.Map // interaction id -> struct{}
}

// peerFile resolves a configured list path against the data directory.
func (d *Daemon) peerFile(name string) string {
	if name == "" || filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(d.layout.Root, name)
}

// readPeerFile reads one AID per line. A missing file is an empty list;
// any other read error is returned, so a list that cannot be read is not
// mistaken for an empty one.
func readPeerFile(path string) (map[string]bool, error) {
	out := map[string]bool{}
	if path == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			out[fields[0]] = true
		}
	}
	return out, sc.Err()
}

// peerSets is the three lists read at one moment.
type peerSets struct {
	allow, deny, trust map[string]bool
}

// readPeers reads the three lists. A list that cannot be read is logged and
// treated as follows: deny as containing everyone (fail closed), allow and
// trust as empty.
func (d *Daemon) readPeers() peerSets {
	in := d.config().inbound()
	var ps peerSets
	var err error
	if ps.deny, err = readPeerFile(d.peerFile(in.DenyFile)); err != nil {
		d.logOnce("peers-deny-read", "anet: cannot read %s (%v); refusing every peer until it can be read", in.DenyFile, err)
		ps.deny = nil
	}
	if ps.allow, err = readPeerFile(d.peerFile(in.AllowFile)); err != nil {
		d.logOnce("peers-allow-read", "anet: cannot read %s (%v); treating it as empty", in.AllowFile, err)
		ps.allow = map[string]bool{}
	}
	if ps.trust, err = readPeerFile(d.peerFile(in.TrustFile)); err != nil {
		d.logOnce("peers-trust-read", "anet: cannot read %s (%v); treating it as empty", in.TrustFile, err)
		ps.trust = map[string]bool{}
	}
	return ps
}

// denied reports whether aid is on the deny list. An unreadable deny list
// denies everyone.
func (ps peerSets) denied(aid string) bool { return ps.deny == nil || ps.deny[aid] }

// allowed reports whether aid may delegate by name: on the allow or the
// trust list and not denied. Trust is the larger grant and includes the
// smaller.
func (ps peerSets) allowed(aid string) bool {
	return !ps.denied(aid) && (ps.allow[aid] || ps.trust[aid])
}

// trusted reports whether aid may drive this node's exec auto-reply and
// A2A backends.
func (ps peerSets) trusted(aid string) bool { return !ps.denied(aid) && ps.trust[aid] }

// --- the delegation decision (§5.2) ---

// Actions of inboundDecision.
const (
	actAccept = "accept"
	actHold   = "hold"
	actRefuse = "refuse"
)

// Refusal reasons carried as anet.reason. Admission refusals use the codes
// of admit.
const (
	reasonNotAccepting      = "not_accepting"
	reasonDenied            = "denied"
	reasonCapabilityNotPub  = "capability_not_public"
	reasonPendingFull       = "pending_full"
	reasonPendingExpired    = "pending_expired"
	reasonOperatorRejected  = "operator_rejected"
	reasonSandboxUnavailble = "sandbox_unavailable"
)

// inboundDecision is the outcome of §5.2 for one delegation.
type inboundDecision struct {
	action string
	trust  string // for actAccept
	reason string // for actRefuse
	// retryAfterMS is set for a refusal that may succeed later (quota).
	retryAfterMS int64
	// release frees the admission slot of a public capability call; nil
	// otherwise. The caller must call it exactly once.
	release func()
}

// decideDelegate applies §5.2 to a delegation from `from`. capID is the
// capability id of a capability call (empty for a natural-language task).
func (d *Daemon) decideDelegate(from, capID string, argsLen int) inboundDecision {
	in := d.config().inbound()
	ps := d.readPeers()
	// Row 1. Under closed a denied peer is told exactly what a stranger is
	// told (same reason, same rate limit), so the deny list is not
	// observable there. Under approve and open it is (§2 X2).
	if ps.denied(from) {
		if in.Policy == PolicyClosed {
			return inboundDecision{action: actRefuse, reason: reasonNotAccepting}
		}
		return inboundDecision{action: actRefuse, reason: reasonDenied}
	}
	// Row 2.
	if capID != "" {
		if _, public := in.publicCapability(capID); public {
			release, refusal, retry := d.admit(from, capID, argsLen)
			if refusal != "" {
				return inboundDecision{action: actRefuse, reason: refusal, retryAfterMS: retry}
			}
			return inboundDecision{action: actAccept, trust: interactions.TrustPublicCap, release: release}
		}
	}
	// Row 3.
	if ps.allowed(from) {
		return inboundDecision{action: actAccept, trust: interactions.TrustPeer}
	}
	switch in.Policy {
	case PolicyApprove: // row 4
		return inboundDecision{action: actHold}
	case PolicyOpen: // row 5
		if capID != "" {
			return inboundDecision{action: actRefuse, reason: reasonCapabilityNotPub}
		}
		return inboundDecision{action: actAccept, trust: interactions.TrustPublic}
	}
	return inboundDecision{action: actRefuse, reason: reasonNotAccepting} // row 6
}

// --- kernel admission (§5.4) ---

// Admission refusal codes (module.Host.Admit, anet.voucher.refused).
const (
	admitDenied        = "denied"
	admitNotPublic     = "capability_not_public"
	admitArgsTooLarge  = "args_too_large"
	admitCallerMinute  = "quota_caller_per_min"
	admitCallerDay     = "quota_caller_per_day"
	admitGlobalMinute  = "quota_global_per_min"
	admitInflightLimit = "max_inflight"
)

// admission holds the counters behind admit. Windows are fixed (the current
// minute, the current day), which is coarse and cheap: a caller can make at
// most twice the per-minute quota across a window boundary.
type admission struct {
	mu       sync.Mutex
	callers  map[string]*callerCount // key: capID + "\x00" + caller
	order    []string
	global   map[string]*window // key: capID
	inflight map[string]int     // key: capID
}

type window struct {
	start int64 // unix ms at the start of the window
	n     int
}

type callerCount struct {
	minute, day window
}

// admissionCallerLimit bounds the per-caller counters. A caller whose
// counters were evicted starts again from zero, which is what a fresh AID
// gets anyway; the global counter is what bounds a flood of fresh AIDs.
const admissionCallerLimit = 10000

func (w *window) take(now, size int64, limit int) bool {
	if now-w.start >= size || now < w.start {
		w.start, w.n = now-now%size, 0
	}
	if w.n >= limit {
		return false
	}
	w.n++
	return true
}

func (w *window) retryAfter(now, size int64) int64 {
	if r := w.start + size - now; r > 0 {
		return r
	}
	return 0
}

// admit is the kernel's admission check for a public capability call
// (A2A-DESIGN §5.4): the deny list (read now), membership of
// public_capabilities, the argument size, the per-caller and global quotas
// and the in-flight bound. On success it returns a release function that
// must be called once when the invocation ends; on refusal it returns the
// refusal code and, for a quota, how long until it may succeed.
func (d *Daemon) admit(caller, capID string, argsLen int) (release func(), refusal string, retryAfterMS int64) {
	return d.admitAt(int64(d.nowMS()), caller, capID, argsLen)
}

// admitAt is admit at a given time (unix ms).
func (d *Daemon) admitAt(now int64, caller, capID string, argsLen int) (release func(), refusal string, retryAfterMS int64) {
	in := d.config().inbound()
	if d.readPeers().denied(caller) {
		return nil, admitDenied, 0
	}
	pc, ok := in.publicCapability(capID)
	if !ok {
		return nil, admitNotPublic, 0
	}
	if argsLen > pc.MaxArgsBytes {
		return nil, admitArgsTooLarge, 0
	}
	const minute, day = int64(60_000), int64(86_400_000)
	a := &d.admission
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.callers == nil {
		a.callers, a.global, a.inflight = map[string]*callerCount{}, map[string]*window{}, map[string]int{}
	}
	if a.inflight[capID] >= pc.MaxInflight {
		return nil, admitInflightLimit, 1000
	}
	key := capID + "\x00" + caller
	cc := a.callers[key]
	if cc == nil {
		cc = &callerCount{}
		a.callers[key] = cc
		a.order = append(a.order, key)
		if len(a.order) > admissionCallerLimit {
			delete(a.callers, a.order[0])
			a.order = a.order[1:]
		}
	}
	g := a.global[capID]
	if g == nil {
		g = &window{}
		a.global[capID] = g
	}
	// Check every limit before taking from any, so a refused call does not
	// consume quota.
	probe := func(w window, size int64, limit int) bool { return w.take(now, size, limit) }
	switch {
	case !probe(cc.minute, minute, pc.PerCallerPerMin):
		return nil, admitCallerMinute, cc.minute.retryAfter(now, minute)
	case !probe(cc.day, day, pc.PerCallerPerDay):
		return nil, admitCallerDay, cc.day.retryAfter(now, day)
	case !probe(*g, minute, pc.GlobalPerMin):
		return nil, admitGlobalMinute, g.retryAfter(now, minute)
	}
	cc.minute.take(now, minute, pc.PerCallerPerMin)
	cc.day.take(now, day, pc.PerCallerPerDay)
	g.take(now, minute, pc.GlobalPerMin)
	a.inflight[capID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.inflight[capID]--
			a.mu.Unlock()
		})
	}, "", 0
}

// Admit implements module.Host.Admit.
func (h moduleHost) Admit(callerAID, capID string, argsLen int) (func(), string) {
	release, refusal, _ := h.d.admit(callerAID, capID, argsLen)
	return release, refusal
}

// DeclareUntrustedBackend implements module.Host.DeclareUntrustedBackend.
func (h moduleHost) DeclareUntrustedBackend() { h.d.untrustedBackend.Store(true) }

// --- refusals: reply, aggregate evidence, local log ---

// EvDelegationRefusedSummary aggregates the delegations this node refused in
// one window: count, counts by reason, and the first few AIDs.
const EvDelegationRefusedSummary = "anet.delegation.refused_summary"

// EvDelegationReceived records an accepted delegation. For a peer this node
// named (allow, trust, approved) it is written per delegation; for public
// and public_cap delegations it is aggregated per window like refusals.
const EvDelegationReceived = "anet.delegation.received"

// EvPolicyChanged records a write to the inbound policy, the peer lists or
// the auto-reply configuration, and the interactions a deny canceled.
const EvPolicyChanged = "anet.policy.changed"

// inboundWindow is the aggregation window of the two summary events.
const inboundWindow = 10 * time.Minute

// summaryAIDs is how many AIDs a summary names.
const summaryAIDs = 10

// inboundAgg accumulates one window of refusals and unnamed acceptances.
type inboundAgg struct {
	mu            sync.Mutex
	start         int64
	refused       int
	refusedBy     map[string]int
	refusedAIDs   []string
	received      int
	receivedBy    map[string]int
	receivedAIDs  []string
	msg           msgAgg // messages on public and public_cap interactions (evidence_msg.go)
	refusedLogMu  sync.Mutex
	refusedLogMax int64 // bytes before rotation; 0 = default
}

func addAID(list []string, aid string) []string {
	if len(list) >= summaryAIDs {
		return list
	}
	for _, a := range list {
		if a == aid {
			return list
		}
	}
	return append(list, aid)
}

// noteRefused counts one refusal into the current window and writes its
// detail to the local refusal log.
func (d *Daemon) noteRefused(from, ix, typ, reason, capID string) {
	a := &d.inAgg
	a.mu.Lock()
	if a.start == 0 {
		a.start = int64(d.nowMS())
	}
	a.refused++
	if a.refusedBy == nil {
		a.refusedBy = map[string]int{}
	}
	a.refusedBy[reason]++
	a.refusedAIDs = addAID(a.refusedAIDs, from)
	a.mu.Unlock()
	d.logRefused(map[string]any{"at": d.nowMS(), "from": from, "interaction_id": ix, "type": typ,
		"reason": reason, "capability": capID})
}

// noteReceivedPublic counts a public or public_cap acceptance into the
// current window.
func (d *Daemon) noteReceivedPublic(from, trust string) {
	a := &d.inAgg
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.start == 0 {
		a.start = int64(d.nowMS())
	}
	a.received++
	if a.receivedBy == nil {
		a.receivedBy = map[string]int{}
	}
	a.receivedBy[trust]++
	a.receivedAIDs = addAID(a.receivedAIDs, from)
}

// flushInboundSummary writes the window's summary events, when the window
// has ended (or force is set) and it counted anything.
func (d *Daemon) flushInboundSummary(force bool) {
	a := &d.inAgg
	now := int64(d.nowMS())
	a.mu.Lock()
	if a.start == 0 || (!force && now-a.start < inboundWindow.Milliseconds()) {
		a.mu.Unlock()
		return
	}
	start := a.start
	refused, refusedBy, refusedAIDs := a.refused, a.refusedBy, a.refusedAIDs
	received, receivedBy, receivedAIDs := a.received, a.receivedBy, a.receivedAIDs
	msgs := a.msg
	a.start, a.refused, a.refusedBy, a.refusedAIDs = 0, 0, nil, nil
	a.received, a.receivedBy, a.receivedAIDs = 0, nil, nil
	a.msg = msgAgg{}
	a.mu.Unlock()
	if d.ledger == nil {
		return
	}
	if refused > 0 {
		if _, err := d.ledger.Append(EvDelegationRefusedSummary, map[string]any{
			"window_start": start, "window_end": now, "count": refused,
			"by_reason": refusedBy, "first_aids": refusedAIDs,
		}); err != nil {
			log.Printf("anet: refusal summary evidence: %v", err)
		}
	}
	if received > 0 {
		if _, err := d.ledger.Append(EvDelegationReceived, map[string]any{
			"aggregated": true, "window_start": start, "window_end": now, "count": received,
			"by_trust": receivedBy, "first_aids": receivedAIDs,
		}); err != nil {
			log.Printf("anet: received summary evidence: %v", err)
		}
	}
	d.appendMessageSummary(msgs, start, now)
}

// refusedLogName is the local refusal log in the data directory.
const refusedLogName = "inbound-refused.log"

// refusedLogMaxBytes is the size at which the refusal log is rotated; one
// older file is kept.
const refusedLogMaxBytes = 1 << 20

// logRefused appends one JSON line to the refusal log, rotating it when it
// is over the size limit.
func (d *Daemon) logRefused(entry map[string]any) {
	a := &d.inAgg
	a.refusedLogMu.Lock()
	defer a.refusedLogMu.Unlock()
	limit := a.refusedLogMax
	if limit <= 0 {
		limit = refusedLogMaxBytes
	}
	path := filepath.Join(d.layout.Root, refusedLogName)
	if fi, err := os.Stat(path); err == nil && fi.Size() >= limit {
		_ = os.Rename(path, path+".1")
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

// replyRejected tells a refused requester so, through the notice limiter:
// status{rejected, anet.reason}. It is sent once, never queued, and dropped
// silently when either the per-peer or the daemon-wide bucket is empty.
func (d *Daemon) replyRejected(m *rxMsg, reason string, retryAfterMS int64) {
	meta := map[string]any{"anet.reason": reason}
	if retryAfterMS > 0 {
		meta["anet.retry_after_ms"] = retryAfterMS
	}
	d.sendNoticeStatus(m, delegation.StateRejected, "this node did not accept the task: "+reason, meta)
}

// sendNoticeStatus seals a status notice to the sender of m with the key set
// it carried or the stored one, rate limited. It is the common path of
// TaskNotFound and refusal replies.
func (d *Daemon) sendNoticeStatus(m *rxMsg, state, text string, meta map[string]any) bool {
	if m.noticeKeys == nil || !d.notices.allow(m.from, d.nowMS()) {
		d.count(noticeSuppressed)
		return false
	}
	d.strangers.put(m.from, m.noticeKeys)
	mb, _ := json.Marshal(meta)
	body, err := (&delegation.StatusMsg{State: state, Text: text, Metadata: mb, At: d.nowMS()}).Marshal()
	if err != nil {
		return false
	}
	d.count(noticeSent)
	d.sendNotice(m.from, m.ix, body)
	return true
}

// --- policy writes ---

// InboundState is the inbound configuration and lists as the control plane
// reports them.
type InboundState struct {
	Policy             string             `json:"policy"`
	AllowFile          string             `json:"allow_file"`
	DenyFile           string             `json:"deny_file"`
	TrustFile          string             `json:"trust_file"`
	Allow              []string           `json:"allow"`
	Deny               []string           `json:"deny"`
	Trust              []string           `json:"trust"`
	PublicCapabilities []PublicCapability `json:"public_capabilities"`
	Pending            int                `json:"pending"`
	UntrustedAutoReply string             `json:"auto_reply_untrusted"`
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// InboundStatus reports the effective inbound configuration and the three
// peer lists as read now.
func (d *Daemon) InboundStatus() InboundState {
	cfg := d.config()
	in := cfg.inbound()
	ps := d.readPeers()
	st := InboundState{Policy: in.Policy, AllowFile: in.AllowFile, DenyFile: in.DenyFile, TrustFile: in.TrustFile,
		Allow: sortedKeys(ps.allow), Deny: sortedKeys(ps.deny), Trust: sortedKeys(ps.trust),
		PublicCapabilities: in.PublicCapabilities, UntrustedAutoReply: UntrustedOff}
	if cfg.AutoReply != nil {
		st.UntrustedAutoReply = cfg.AutoReply.UntrustedMode()
	}
	if items, err := d.ix.ListPending(); err == nil {
		st.Pending = len(items)
	}
	return st
}

// SetInboundPolicy changes inbound.policy. A change that conflicts with the
// auto-reply or backend configuration is refused with ErrPolicyConflict.
//
// The change is saved first and in force after: a caller told it failed
// must not find it in force (an open policy the operator was told did not
// take, while config.json and doctor say closed), nor have the next config
// write of any kind carry it to disk.
func (d *Daemon) SetInboundPolicy(policy string) error {
	d.policyWrite.Lock()
	defer d.policyWrite.Unlock()
	next := d.config()
	in := next.inbound()
	from := in.Policy
	in.Policy = policy
	next.Inbound = &in
	if err := validatePolicy(next, d.untrustedBackend.Load()); err != nil {
		return err
	}
	if err := SaveConfig(d.layout, next); err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg.Inbound = next.Inbound
	d.mu.Unlock()
	d.recordPolicyChange("inbound.policy", from, policy, nil)
	if (from == PolicyOpen) != (policy == PolicyOpen) {
		// An open node lists the chat skill on its network card (0017
		// Q27): published on opening; on closing, taken off, and the card
		// withdrawn when nothing else is left on it.
		d.cardInputsChanged()
	}
	return nil
}

// SetPublicCapabilities replaces inbound.public_capabilities. Saved first
// and in force after, as SetInboundPolicy.
func (d *Daemon) SetPublicCapabilities(caps []PublicCapability) error {
	d.policyWrite.Lock()
	defer d.policyWrite.Unlock()
	next := d.config()
	in := next.inbound()
	from := in.PublicCapabilities
	in.PublicCapabilities = caps
	next.Inbound = &in
	if err := validatePolicy(next, d.untrustedBackend.Load()); err != nil {
		return err
	}
	if err := SaveConfig(d.layout, next); err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg.Inbound = next.Inbound
	d.mu.Unlock()
	d.recordPolicyChange("inbound.public_capabilities", from, caps, nil)
	// The network card lists the public capabilities (A2A-DESIGN §10.2).
	d.cardInputsChanged()
	return nil
}

func (d *Daemon) recordPolicyChange(field string, from, to any, extra map[string]any) {
	if d.ledger == nil {
		return
	}
	ev := map[string]any{"field": field, "from": from, "to": to}
	for k, v := range extra {
		ev[k] = v
	}
	if _, err := d.ledger.Append(EvPolicyChanged, ev); err != nil {
		log.Printf("anet: policy change evidence: %v", err)
	}
}

// Peer list names for PeerListEdit.
const (
	ListAllow = "allow"
	ListDeny  = "deny"
	ListTrust = "trust"
)

// editPeerFile adds or removes aid in one list file, rewriting it
// atomically and keeping comments and other lines.
func (d *Daemon) editPeerFile(list, aid string, add bool) (changed bool, err error) {
	if !validAIDSyntax(aid) {
		return false, fmt.Errorf("anet: %q is not an AID", aid)
	}
	in := d.config().inbound()
	var name string
	switch list {
	case ListAllow:
		name = in.AllowFile
	case ListDeny:
		name = in.DenyFile
	case ListTrust:
		name = in.TrustFile
	case ListPayees:
		// The spending policy's payee list (payees.go), kept like these.
		if name = d.config().Payments.limits().PayeesFile; name == "" {
			return false, ErrPayeesOff
		}
	default:
		return false, fmt.Errorf("anet: unknown peer list %q", list)
	}
	path := d.peerFile(name)
	d.peerLists.mu.Lock()
	defer d.peerLists.mu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	var lines []string
	present := false
	for _, line := range strings.Split(string(raw), "\n") {
		body := line
		if i := strings.IndexByte(body, '#'); i >= 0 {
			body = body[:i]
		}
		if f := strings.Fields(body); len(f) > 0 && f[0] == aid {
			present = true
			if !add {
				continue
			}
		}
		lines = append(lines, line)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if add == present {
		return false, nil
	}
	if add {
		lines = append(lines, aid)
	}
	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	if err := writeFileAtomic(path, []byte(out), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// validAIDSyntax is a shape check for an AID given on the command line: one
// token of printable characters, of a plausible length. It keeps a stray
// argument or a comment from being written into a list file; it does not
// check that the AID exists.
func validAIDSyntax(aid string) bool {
	if len(aid) < 8 || len(aid) > 256 {
		return false
	}
	for _, r := range aid {
		if r <= ' ' || r == '#' || r > '~' {
			return false
		}
	}
	return true
}

// PeerListResult is the outcome of a peer list edit.
type PeerListResult struct {
	AID      string   `json:"aid"`
	List     string   `json:"list"`
	Changed  bool     `json:"changed"`
	Pinned   string   `json:"pinned,omitempty"`
	PinError string   `json:"pin_error,omitempty"`
	Canceled []string `json:"canceled,omitempty"`
	// SkippedPaid are the peer's interactions left running because a
	// payment was submitted or settled on them (0017 Q10).
	SkippedPaid []string `json:"skipped_paid,omitempty"`
}

// AllowPeer adds aid to the allow list (or the trust list when list is
// ListTrust) and pins its KEL and key set in peer_identity when they can be
// fetched (A2A-DESIGN §3.8). When they cannot, the first valid message from
// the peer is trusted on first use and pinned then.
func (d *Daemon) AllowPeer(ctx context.Context, aid, list string) (PeerListResult, error) {
	if list != ListAllow && list != ListTrust {
		return PeerListResult{}, fmt.Errorf("anet: AllowPeer takes the allow or trust list")
	}
	changed, err := d.editPeerFile(list, aid, true)
	if err != nil {
		return PeerListResult{}, err
	}
	res := PeerListResult{AID: aid, List: list, Changed: changed}
	reason := interactions.PinAllow
	if list == ListTrust {
		reason = interactions.PinTrust
	}
	if perr := d.PinPeer(ctx, aid, reason); perr != nil {
		res.PinError = perr.Error()
	} else {
		res.Pinned = reason
	}
	if changed {
		d.recordPolicyChange("peers."+list, nil, aid, nil)
	}
	return res, nil
}

// DenyPeer adds aid to the deny list and cancels its active interactions in
// both roles (A2A-DESIGN §5.1 last bullet).
func (d *Daemon) DenyPeer(ctx context.Context, aid string) (PeerListResult, error) {
	changed, err := d.editPeerFile(ListDeny, aid, true)
	if err != nil {
		return PeerListResult{}, err
	}
	res := PeerListResult{AID: aid, List: ListDeny, Changed: changed}
	res.Canceled, res.SkippedPaid = d.cancelForPolicy(ctx, aid)
	d.skippedPaidNew(res.SkippedPaid)
	if changed || len(res.Canceled) > 0 || len(res.SkippedPaid) > 0 {
		d.recordPolicyChange("peers.deny", nil, aid, denyExtra(res.Canceled, res.SkippedPaid))
	}
	return res, nil
}

// RemovePeer removes aid from every list.
func (d *Daemon) RemovePeer(aid string) (PeerListResult, error) {
	res := PeerListResult{AID: aid, List: "all"}
	for _, l := range []string{ListAllow, ListDeny, ListTrust} {
		ch, err := d.editPeerFile(l, aid, false)
		if err != nil {
			return res, err
		}
		if ch {
			res.Changed = true
			d.recordPolicyChange("peers."+l, aid, nil, nil)
		}
	}
	return res, nil
}

// cancelForPolicy cancels every active interaction with aid, in both roles.
// Inbound ones tell the requester (status canceled); outbound ones tell the
// provider (cancel). It returns the canceled interaction ids, and the ids
// it left running because a payment was submitted or settled on them.
func (d *Daemon) cancelForPolicy(ctx context.Context, aid string) (canceled, skippedPaid []string) {
	list, err := d.ix.ListAll(interactions.ListFilter{PeerAID: aid, Active: true})
	if err != nil {
		log.Printf("anet: list interactions with %s: %v", aid, err)
		return nil, nil
	}
	for _, ix := range list {
		if ix.PayState == interactions.PaySubmitted || ix.PayState == interactions.PayCompleted {
			// A task whose payment was submitted is not canceled by either
			// side (§4.2): paid work is completed and delivered (0017
			// Q10), and a requester's cancel leaves its task open. Sending
			// that cancel again on every sweep would only repeat it.
			skippedPaid = append(skippedPaid, ix.ID)
			continue
		}
		if _, err := d.CancelTask(ctx, ix.ID); err != nil {
			log.Printf("anet: cancel %s after denying %s: %v", ix.ID, aid, err)
			continue
		}
		canceled = append(canceled, ix.ID)
	}
	return canceled, skippedPaid
}

// revocationSweep cancels active interactions whose peer is on the deny
// list. It covers a deny list edited outside the CLI; the CLI path cancels
// at once. Run from the maintenance loop.
func (d *Daemon) revocationSweep() {
	// Only what was reported before this sweep read the list can be
	// forgotten by it: a deny made meanwhile reports its own, and the
	// list read below may be older than that deny.
	reported := d.skippedPaidReported()
	ps := d.readPeers()
	if ps.deny == nil {
		return
	}
	current := map[string]bool{}
	for aid := range ps.deny {
		ids, skipped := d.cancelForPolicy(d.ctx, aid)
		for _, id := range skipped {
			current[id] = true
		}
		if fresh := d.skippedPaidNew(skipped); len(ids) > 0 || len(fresh) > 0 {
			extra := denyExtra(ids, fresh)
			extra["detected"] = "sweep"
			d.recordPolicyChange("peers.deny", nil, aid, extra)
		}
	}
	d.skippedPaidForget(reported, current)
}
