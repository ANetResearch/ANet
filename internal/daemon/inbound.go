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
// The three peer files (peers.allow, peers.deny and peers.trust in the data
// directory) are read on every decision, so an edit takes effect on the
// next message without a restart. A missing file is an empty list.
//
// A refusal sends nothing: the requester is told in the delivery token of
// its delegation, whose note carries the reason and when to try again
// (engine ENG-V15, CUX-Q1), and it costs this node no envelope. Refusals
// are counted into one anet.delegation.refused_summary evidence event per
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

	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// Inbound policies (A2A-DESIGN §5.1).
const (
	PolicyClosed  = "closed"
	PolicyApprove = "approve"
	PolicyOpen    = "open"
)

// InboundConfig is the inbound block of config.json.
//
// The three peer lists live in the data directory under fixed names
// (peersAllowFile, peersDenyFile, peersTrustFile); the approval queue's
// bounds are constants (pendingMaxTotal, pendingMaxPerPeer, pendingTTL)
// and a refusal is told in its delivery token, so 0.3.0 has no setting for
// any of them (ux §2.2, §2.3): a key that named one stops the daemon.
type InboundConfig struct {
	// Policy is closed (default), approve or open.
	Policy string `json:"policy"`
	// PublicCapabilities are the capabilities anyone may call, each with
	// its quotas; an allow_only one only the peers on the allow and trust
	// lists (CUX-F12).
	PublicCapabilities []PublicCapability `json:"public_capabilities"`
}

// PublicCapability is one capability anyone may call. A zero limit takes
// the default shown in A2A-DESIGN §5.1.
type PublicCapability struct {
	ID              string `json:"id"`
	PerCallerPerMin int    `json:"per_caller_per_min,omitempty"`
	PerCallerPerDay int    `json:"per_caller_per_day,omitempty"`
	GlobalPerMin    int    `json:"global_per_min,omitempty"`
	MaxInflight     int    `json:"max_inflight,omitempty"`
	// PerCallerInflight is how many calls of one caller may run at once
	// (CUX-F11): 1 by default, at most MaxInflight, so that one caller
	// cannot hold every slot of the capability.
	PerCallerInflight int `json:"per_caller_inflight,omitempty"`
	MaxArgsBytes      int `json:"max_args_bytes,omitempty"`
	// AllowOnly serves the capability only to the callers on the allow
	// and trust lists (CUX-F12). To anyone else it is not there: the same
	// answer, at the same moment, as a capability this node does not have
	// (CUX-Q2), and it is on no card the hub sees (CUX-F32).
	AllowOnly bool `json:"allow_only,omitempty"`
	// Evidence is what the chain keeps of a call of this capability
	// (trust public_cap, whoever the caller): "cid" (the default) keeps
	// the result CID and the metrics, "full" also keeps the provenance
	// with the observed state, which is the capability's whole answer.
	// See evidence_mode.go.
	Evidence string `json:"evidence,omitempty"`
}

// The peer lists, in the data directory (ux §2.2: "固定为数据目录下的
// peers.*"). One AID per line, '#' starts a comment.
const (
	peersAllowFile = "peers.allow"
	peersDenyFile  = "peers.deny"
	peersTrustFile = "peers.trust"
)

// The approval queue's bounds (ux §2.2, §2.6). They are a safety boundary,
// not a preference: a full queue answers pending_full, and what the
// operator does about it is clear the queue, not raise the bound.
const (
	pendingMaxTotal   = 200
	pendingMaxPerPeer = 3
	pendingTTL        = 72 * time.Hour
	// pendingFollowupsMax bounds the messages kept for one held delegation.
	pendingFollowupsMax = 5
)

// Defaults of a public capability's limits.
const (
	defaultCapPerCallerPerMin   = 60
	defaultCapPerCallerPerDay   = 2000
	defaultCapGlobalPerMin      = 1200
	defaultCapMaxInflight       = 16
	defaultCapPerCallerInflight = 1
	defaultCapMaxArgsBytes      = 4096
)

// defaultInbound is the inbound block of a fresh install: closed, no public
// capability (SI-5).
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
	if c.PublicCapabilities == nil {
		c.PublicCapabilities = []PublicCapability{}
	}
}

// limits returns p with zero fields replaced by the defaults. A
// per_caller_inflight above max_inflight stops the daemon from starting
// (checkPublicCapabilities); one that got past that is held to it here.
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
	if p.PerCallerInflight <= 0 {
		p.PerCallerInflight = defaultCapPerCallerInflight
	}
	p.PerCallerInflight = min(p.PerCallerInflight, p.MaxInflight)
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

// publicCapability returns the public_capabilities entry for capID,
// allow_only or not.
func (c InboundConfig) publicCapability(capID string) (PublicCapability, bool) {
	for _, p := range c.PublicCapabilities {
		if p.ID == capID && capID != "" {
			return p.limits(), true
		}
	}
	return PublicCapability{}, false
}

// publicTo returns the entry for capID when it is public to a caller: any
// caller, or for an allow_only capability one on the allow or trust list
// (listed). To any other caller an allow_only capability is not public,
// exactly as if it were not in the list (CUX-F12).
func (c InboundConfig) publicTo(capID string, listed bool) (PublicCapability, bool) {
	p, ok := c.publicCapability(capID)
	if !ok || (p.AllowOnly && !listed) {
		return PublicCapability{}, false
	}
	return p, true
}

// openToAll reports whether capID is public to every caller: in the list
// and not allow_only. Only those are on a card the hub sees (CUX-F32).
func (c InboundConfig) openToAll(capID string) bool {
	_, ok := c.publicTo(capID, false)
	return ok
}

// allowOnlyIDs are the allow_only capabilities, in the order of the list.
func (c InboundConfig) allowOnlyIDs() []string {
	var out []string
	for _, p := range c.PublicCapabilities {
		if p.AllowOnly && strings.TrimSpace(p.ID) != "" {
			out = append(out, p.ID)
		}
	}
	return out
}

// --- configuration validation (A2A-DESIGN §5.1) ---

// ErrPolicyConflict is returned when a configuration would accept work from
// anyone (policy open) while also forwarding to an A2A backend that takes
// peers this node does not trust. The control plane answers it with 409.
//
// Policy open together with the exec backend in the sandbox for untrusted
// peers is not a conflict (LEC GT-5): the sandbox has no network beyond the
// node's model route, holds no key and runs under a daily budget (SI-55 to
// SI-58), which is what running an agent for anyone needs.
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
	if _, err := c.noResponseAfter(); err != nil {
		return err
	}
	for _, p := range in.PublicCapabilities {
		if strings.TrimSpace(p.ID) == "" {
			return fmt.Errorf("anet: inbound.public_capabilities has an entry without an id")
		}
		if err := validEvidenceMode(p); err != nil {
			return err
		}
	}
	if ref := checkPublicCapabilities(in); ref != nil {
		return ref
	}
	if in.Policy != PolicyOpen {
		return nil
	}
	if untrustedBackend {
		return fmt.Errorf("%w: policy open accepts tasks from anyone, and an A2A backend is configured with "+
			"accept_untrusted; change one of the two", ErrPolicyConflict)
	}
	return nil
}

// checkPublicCapabilities is the check of the public capabilities' own
// settings (CUX §2.10): each capability is listed once, and
// per_caller_inflight is from 1 to max_inflight, 1 when it is left out.
// It stops the daemon from starting (configChecks) and refuses a
// control-plane write (409), with config_invalid naming the entry.
func checkPublicCapabilities(in InboundConfig) *a2ashape.Refusal {
	seen := map[string]bool{}
	for i, p := range in.PublicCapabilities {
		// One entry a capability: two could say allow_only and not, and the
		// cards and the admission would each believe another one.
		id := strings.TrimSpace(p.ID)
		if id != "" && seen[id] {
			return configInvalid(fmt.Sprintf("inbound.public_capabilities[%d].id", i),
				ruleText(fmt.Sprintf("能力 %s 已在前面列过，每个能力只写一项", id),
					fmt.Sprintf("the capability %s is listed already; list each capability once", id)))
		}
		seen[id] = true
		if p.PerCallerInflight == 0 {
			continue
		}
		maxIn := p.limits().MaxInflight
		if p.PerCallerInflight < 1 || p.PerCallerInflight > maxIn {
			return configInvalid(fmt.Sprintf("inbound.public_capabilities[%d].per_caller_inflight", i),
				ruleText(fmt.Sprintf("须是 1 到 %d 之间的整数（不超过 max_inflight）", maxIn),
					fmt.Sprintf("it is a whole number from 1 to %d (no more than max_inflight)", maxIn)))
		}
	}
	return nil
}

func init() {
	configChecks = append(configChecks, func(c Config, _ map[string]any) *a2ashape.Refusal {
		return checkPublicCapabilities(c.inbound())
	})
}

// validate runs validatePolicy against this daemon's module declarations.
// It also runs the auto-reply checks that need the data dir (LEC §3.2):
// a daemon whose config.json a person edited by hand does not start with a
// value it would refuse to write.
func (d *Daemon) validate(c Config) error {
	if err := validatePolicy(c, d.untrustedBackend.Load()); err != nil {
		return err
	}
	if ref := checkAutoReplyConfig(c.AutoReply, d.layout.Root); ref != nil {
		return ref
	}
	return nil
}

// --- peer lists ---

// peerLists serializes writes to the three peer files. Reads are not
// locked: a file is replaced atomically, so a reader sees the old or the
// new list.
type peerLists struct {
	mu sync.Mutex
	// skippedPaid holds the paid interactions a deny already reported as
	// left running (an internal note item), so the minute sweep reports each once.
	skippedPaid sync.Map // interaction id -> struct{}
}

// peerFile is a peer list's file in the data directory.
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
	var ps peerSets
	var err error
	if ps.deny, err = readPeerFile(d.peerFile(peersDenyFile)); err != nil {
		d.logOnce("peers-deny-read", "anet: cannot read %s (%v); refusing every peer until it can be read", peersDenyFile, err)
		ps.deny = nil
	}
	if ps.allow, err = readPeerFile(d.peerFile(peersAllowFile)); err != nil {
		d.logOnce("peers-allow-read", "anet: cannot read %s (%v); treating it as empty", peersAllowFile, err)
		ps.allow = map[string]bool{}
	}
	if ps.trust, err = readPeerFile(d.peerFile(peersTrustFile)); err != nil {
		d.logOnce("peers-trust-read", "anet: cannot read %s (%v); treating it as empty", peersTrustFile, err)
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

// --- the delegation decision (§5.2, CUX-Q1a) ---

// Actions of inboundDecision.
const (
	actAccept = "accept"
	actHold   = "hold"
	actRefuse = "refuse"
)

// Refusal reasons carried as anet.reason. The policy's and the admission's
// are the note codes a refusal is answered with (core §2.15.2, CUX-Q1).
const (
	reasonNotAccepting      = "not_accepting"
	reasonDenied            = "denied"
	reasonCapabilityNotPub  = "capability_not_public"
	reasonPendingFull       = "pending_full"
	reasonBadRequest        = "bad_request"
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

func refuseWith(reason string, retryAfterMS int64) inboundDecision {
	return inboundDecision{action: actRefuse, reason: reason, retryAfterMS: retryAfterMS}
}

// delegateForm is what step 9 reads of a delegation's TaskDoc.
type delegateForm struct {
	// capID is the capability called; empty for a natural-language task.
	capID string
	// argsLen is the size of the call's args as JSON.
	argsLen int
	// bad is a request whose form is refused, bad_request (CORE-K7,
	// CUX-Q1a step 2): a capability call without its args, or with args
	// that are not one JSON object, or with a top-level key that starts
	// with "$" and is not one anet defines.
	bad bool
}

// argsFilesKey is the one "$" key of a capability call's args that anet
// defines (core §2.4.3): the files the call carries. Its admission is the
// files' (CUX-F10, step 4).
const argsFilesKey = "$files"

// readDelegateForm reads td as step 9 does. The capability is the first
// requirement of type capability, as capabilityCall finds it; its args
// are the one args context, which every capability call carries.
func readDelegateForm(td *tsir.TaskDoc) delegateForm {
	var f delegateForm
	if td == nil || len(td.Tasks) == 0 {
		return f
	}
	t := td.Tasks[0]
	for _, r := range t.Requires {
		if r.Type == RequireTypeCapability && r.ID != "" {
			f.capID = r.ID
			break
		}
	}
	if f.capID == "" {
		return f
	}
	var raw *string
	for _, c := range t.Contexts {
		if c.Key != "args" {
			continue
		}
		if raw != nil {
			f.bad = true // two args: which one would run is a guess
			return f
		}
		v := c.Value
		raw = &v
	}
	var args map[string]any
	if raw == nil || json.Unmarshal([]byte(*raw), &args) != nil || args == nil {
		f.bad = true
		return f
	}
	for k := range args {
		if strings.HasPrefix(k, "$") && k != argsFilesKey {
			f.bad = true
			return f
		}
	}
	if b, err := json.Marshal(args); err == nil {
		f.argsLen = len(b)
	}
	return f
}

// decideDelegate applies §5.2 to a delegation from `from`, in the order of
// CUX-Q1a; the first step that refuses decides, and a refusal takes no
// quota.
func (d *Daemon) decideDelegate(from string, f delegateForm) inboundDecision {
	return d.decideDelegateAt(int64(d.nowMS()), from, f)
}

// decideDelegateAt is decideDelegate at a given time (unix ms):
//
//  1. the policy (decidePolicy);
//  2. the request's form (bad_request);
//  3. and 5–9 for a public capability call, its admission (admitAt).
//
// Step 4, the files a call carries, is the files' own (CUX-F10). A
// natural-language task stops after step 2.
func (d *Daemon) decideDelegateAt(now int64, from string, f delegateForm) inboundDecision {
	in := d.config().inbound()
	dec, pc, public := decidePolicy(in, d.readPeers(), from, f.capID)
	if dec.action == actRefuse {
		return dec
	}
	if f.bad {
		return refuseWith(reasonBadRequest, 0)
	}
	if !public {
		return dec
	}
	release, refusal, retry := d.admitAt(now, from, pc, f.argsLen)
	if refusal != "" {
		return refuseWith(refusal, retry)
	}
	dec.release = release
	return dec
}

// decidePolicy is step 1 (§5.2):
//
//	1 from ∈ deny                              refuse (closed: the same answer as row 6)
//	2 capability call, public to from          admission, then trust=public_cap
//	3 from ∈ allow ∪ trust                     trust=peer
//	4 policy approve                           hold in the pending queue
//	5 policy open                              capability call: refuse; otherwise trust=public
//	6 otherwise                                refuse, not_accepting
//
// An allow_only capability is public only to a caller on the allow or
// trust list; to any other it is a capability that is not in the list, and
// what it is told is what a call of one this node does not have is told:
// not_accepting under closed, capability_not_public under open, the queue
// under approve (CUX-F12, CUX-Q2). public reports row 2, with the entry.
func decidePolicy(in InboundConfig, ps peerSets, from, capID string) (dec inboundDecision, pc PublicCapability, public bool) {
	// Row 1. Under closed a denied peer is told exactly what a stranger is
	// told (the same note, at the same moment), so the deny list is not
	// observable there. Under approve and open it is (§2 X2).
	if ps.denied(from) {
		if in.Policy == PolicyClosed {
			return refuseWith(reasonNotAccepting, 0), pc, false
		}
		return refuseWith(reasonDenied, 0), pc, false
	}
	listed := ps.allowed(from)
	// Row 2.
	if capID != "" {
		if pc, public = in.publicTo(capID, listed); public {
			return inboundDecision{action: actAccept, trust: interactions.TrustPublicCap}, pc, true
		}
	}
	// Row 3.
	if listed {
		return inboundDecision{action: actAccept, trust: interactions.TrustPeer}, pc, false
	}
	switch in.Policy {
	case PolicyApprove: // row 4
		return inboundDecision{action: actHold}, pc, false
	case PolicyOpen: // row 5
		if capID != "" {
			return refuseWith(reasonCapabilityNotPub, 0), pc, false
		}
		return inboundDecision{action: actAccept, trust: interactions.TrustPublic}, pc, false
	}
	return refuseWith(reasonNotAccepting, 0), pc, false // row 6
}

// --- kernel admission (§5.4, CUX-Q1a steps 3 and 5–9) ---

// Admission refusal codes of a public capability call: the note codes
// they are answered with (CUX-Q1). busy is the capability's in-flight
// bound and its global per-minute quota alike, so a caller is not told
// that someone else is using it (review V9).
const (
	admitArgsTooLarge = "args_too_large"
	admitBusy         = "busy"
	admitCallerBusy   = "caller_busy"
	admitCallerMinute = "quota_caller_per_min"
	admitCallerDay    = "quota_caller_per_day"
)

// admitBusyWaitMS is how long a call refused for a slot (busy for the
// in-flight bound, caller_busy) is told to wait: 5 seconds (CUX-Q1).
const admitBusyWaitMS = 5_000

// admission holds the counters behind admitAt. Windows are fixed (the
// current minute, the current day), which is coarse and cheap: a caller can
// make at most twice the per-minute quota across a window boundary.
type admission struct {
	mu       sync.Mutex
	callers  map[string]*callerCount // key: capID + "\x00" + caller
	order    []string
	global   map[string]*window // key: capID
	inflight map[string]int     // key: capID
	// callerInflight is the calls of one caller running on a capability
	// (CUX-F11), key capID + "\x00" + caller. An entry goes when its count
	// is back to 0, so the map holds no more than the calls running.
	callerInflight map[string]int
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

// admitAt is the kernel's admission check of a call of pc, a capability
// public to caller (A2A-DESIGN §5.4), at now (unix ms), in the order of
// CUX-Q1a: the argument size (3), the capability's in-flight bound (5),
// the caller's (6), the caller's per-minute (7) and per-day (8) quotas and
// the capability's global per-minute quota (9). Every limit is checked
// before any is taken from, so a refused call takes nothing. On success it
// returns a release function that must be called once when the invocation
// ends, freeing both in-flight slots; on refusal, the code and how long
// until the call may be admitted.
func (d *Daemon) admitAt(now int64, caller string, pc PublicCapability, argsLen int) (release func(), refusal string, retryAfterMS int64) {
	pc = pc.limits()
	capID := pc.ID
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
	if a.callerInflight == nil {
		a.callerInflight = map[string]int{}
	}
	key := capID + "\x00" + caller
	if a.inflight[capID] >= pc.MaxInflight {
		return nil, admitBusy, admitBusyWaitMS
	}
	if a.callerInflight[key] >= pc.PerCallerInflight {
		return nil, admitCallerBusy, admitBusyWaitMS
	}
	cc := a.callers[key]
	var fresh callerCount
	if cc == nil {
		cc = &fresh
	}
	g := a.global[capID]
	var freshG window
	if g == nil {
		g = &freshG
	}
	probe := func(w window, size int64, limit int) bool { return w.take(now, size, limit) }
	switch {
	case !probe(cc.minute, minute, pc.PerCallerPerMin):
		return nil, admitCallerMinute, cc.minute.retryAfter(now, minute)
	case !probe(cc.day, day, pc.PerCallerPerDay):
		return nil, admitCallerDay, cc.day.retryAfter(now, day)
	case !probe(*g, minute, pc.GlobalPerMin):
		return nil, admitBusy, g.retryAfter(now, minute)
	}
	// Admitted: only now is anything recorded.
	if a.callers[key] == nil {
		a.callers[key] = cc
		a.order = append(a.order, key)
		if len(a.order) > admissionCallerLimit {
			delete(a.callers, a.order[0])
			a.order = a.order[1:]
		}
	}
	if a.global[capID] == nil {
		a.global[capID] = g
	}
	cc.minute.take(now, minute, pc.PerCallerPerMin)
	cc.day.take(now, day, pc.PerCallerPerDay)
	g.take(now, minute, pc.GlobalPerMin)
	a.inflight[capID]++
	a.callerInflight[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.inflight[capID]--; a.inflight[capID] <= 0 {
				delete(a.inflight, capID)
			}
			if a.callerInflight[key]--; a.callerInflight[key] <= 0 {
				delete(a.callerInflight, key)
			}
		})
	}, "", 0
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

// --- policy writes ---

// InboundState is the inbound configuration and lists as the control plane
// reports them.
type InboundState struct {
	Policy             string             `json:"policy"`
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
	st := InboundState{Policy: in.Policy,
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
	d.cfgWrite.Lock()
	defer d.cfgWrite.Unlock()
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
	d.cfgWrite.Lock()
	defer d.cfgWrite.Unlock()
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
	var name string
	switch list {
	case ListAllow:
		name = peersAllowFile
	case ListDeny:
		name = peersDenyFile
	case ListTrust:
		name = peersTrustFile
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
	// payment was submitted or settled on them (an internal note item).
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
