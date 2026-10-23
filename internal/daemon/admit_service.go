package daemon

// admit_service.go is admission delegated to a local service (design v0.3.9
// §A, inbound.admit) and the service calls a module makes as a peer
// (§B, module.ServiceCaller).
//
// inbound.admit names a capability of this node's service module that the
// service configuration marks "internal": true. For a text task from a peer
// on no list (not allow, trust or deny), the kernel asks it: a POST with
// X-ANet-Caller set to the peer's AID and the body {}. The answer
// {"admit": true} admits the peer; any other answer, an error, no answer
// within admitCallTimeout, or no such capability does not (fail closed).
//
// An admitted peer's task is accepted as an allow-listed peer's would be
// (trust peer), and its turns carry Turn.Admitted, its inbound tasks
// anet.admitted. Nothing else follows from it: payments, the trust list and
// what `anet peers` shows are unchanged. The deny list is read first and
// always wins.
//
// The decision is kept per peer for cache_seconds (30 by default, at most
// 300), admitted or not; one that failed is kept admitErrorTTL, so a
// service that is down is not asked again for every message while a member
// is not shut out for long once it is back. It is decided again at every
// owed turn, every message on the task and every delivery to a backend, so
// a member the service removes stops at the next turn after the cache
// lapses.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// InboundAdmit is inbound.admit: the internal capability of the service
// module that decides whether a peer on no list is admitted.
type InboundAdmit struct {
	Capability string `json:"capability"`
	// CacheSeconds is how long a decision is kept per peer: 1 to
	// admitCacheMaxSeconds, admitCacheDefaultSeconds when left out.
	CacheSeconds int `json:"cache_seconds,omitempty"`
}

const (
	admitCacheDefaultSeconds = 30
	admitCacheMaxSeconds     = 300
	// admitCacheEntries bounds the cache; a full cache drops what has
	// expired, then everything, which costs only questions asked again.
	admitCacheEntries = 4096
)

// admitCallTimeout bounds one question to the service: it is asked on the
// receive path. admitErrorTTL is how long a question that failed counts as
// "not admitted" before the service is asked again. Variables so tests can
// shorten them.
var (
	admitCallTimeout = 5 * time.Second
	admitErrorTTL    = 5 * time.Second
)

// cacheTTL is how long a decision of the service is kept.
func (a InboundAdmit) cacheTTL() time.Duration {
	if a.CacheSeconds <= 0 {
		return admitCacheDefaultSeconds * time.Second
	}
	return time.Duration(min(a.CacheSeconds, admitCacheMaxSeconds)) * time.Second
}

// admitCapRe is the form of a capability id inbound.admit may name: printable
// characters without spaces, at most 128.
var admitCapRe = regexp.MustCompile(`^[!-~]{1,128}$`)

// checkInboundAdmit is the check of inbound.admit's own settings: a
// capability id, a cache of 1 to 300 seconds, and a capability that is not
// also public (an internal capability is never called from the network).
// It stops the daemon from starting (configChecks) and refuses a write.
func checkInboundAdmit(in InboundConfig) *a2ashape.Refusal {
	ad := in.Admit
	if ad == nil {
		return nil
	}
	if !admitCapRe.MatchString(ad.Capability) {
		return configInvalid("inbound.admit.capability", ruleText(
			"须是服务模块中标记为 \"internal\": true 的能力 id",
			`it must be the id of a capability of the service module marked "internal": true`))
	}
	if ad.CacheSeconds < 0 || ad.CacheSeconds > admitCacheMaxSeconds {
		return configInvalid("inbound.admit.cache_seconds", ruleText(
			fmt.Sprintf("须是 1 到 %d 之间的整数", admitCacheMaxSeconds),
			fmt.Sprintf("it is a whole number from 1 to %d", admitCacheMaxSeconds)))
	}
	if _, ok := in.publicCapability(ad.Capability); ok {
		return configInvalid("inbound.admit.capability", ruleText(
			"准入能力只供本节点调用，不能同时列在 inbound.public_capabilities 中",
			"the admission capability is called by this node only and cannot also be in inbound.public_capabilities"))
	}
	return nil
}

func init() {
	configChecks = append(configChecks, func(c Config, _ map[string]any) *a2ashape.Refusal {
		return checkInboundAdmit(c.inbound())
	})
}

// checkAdmitServed is the part of the check that needs the running modules:
// inbound.admit names an internal capability of the service module. It runs
// at start, once the modules have started (validate), and on every write of
// inbound.admit, never on a configuration the doctor reads without a daemon.
func (d *Daemon) checkAdmitServed(c Config) *a2ashape.Refusal {
	ad := c.inbound().Admit
	if ad == nil {
		return nil
	}
	if sb := d.serviceBackend(); sb != nil && sb.InternalCapability(ad.Capability) {
		return nil
	}
	return configInvalid("inbound.admit.capability", ruleText(
		fmt.Sprintf("服务模块（modules.service）没有标记为 \"internal\": true 的能力 %s", ad.Capability),
		fmt.Sprintf(`the service module (modules.service) has no capability %s marked "internal": true`, ad.Capability)))
}

// serviceBackend is the service module's module.ServiceBackend, nil when
// the module is not configured, not in this build or not started.
func (d *Daemon) serviceBackend() module.ServiceBackend {
	if d.providers == nil {
		return nil
	}
	p, ok := d.providers.Provider("service")
	if !ok {
		return nil
	}
	sb, _ := p.(module.ServiceBackend)
	return sb
}

// internalCapability reports whether id is an internal capability of the
// service module: one that is never published, whatever the lists say.
func (d *Daemon) internalCapability(id string) bool {
	sb := d.serviceBackend()
	return sb != nil && sb.InternalCapability(id)
}

// --- the decision and its cache ---

// admitCache holds the service's decisions by capability and peer, and the
// questions being asked, so that a burst of messages from one peer asks once.
type admitCache struct {
	mu      sync.Mutex
	entries map[string]admitEntry
	flights map[string]*admitFlight
}

type admitEntry struct {
	admitted bool
	until    time.Time
}

type admitFlight struct {
	done     chan struct{}
	admitted bool
}

// clear forgets every decision: inbound.admit changed.
func (c *admitCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

// admittedFor is Turn.Admitted and anet.admitted for peer: true only for a
// text task (not a capability call) from a peer that is on no list and that
// the service named by inbound.admit admits now. A peer on the allow or
// trust list is not asked (the lists decide), nor is a denied one.
func (d *Daemon) admittedFor(peer string, isCapability bool, ps peerSets) bool {
	if isCapability || peer == "" || ps.denied(peer) || ps.allowed(peer) {
		return false
	}
	return d.admittedPeer(peer)
}

// admittedPeer is the service's decision for peer, from the cache or asked
// now. False when inbound.admit is not set.
func (d *Daemon) admittedPeer(peer string) bool {
	ad := d.config().inbound().Admit
	if ad == nil || peer == "" {
		return false
	}
	key := ad.Capability + "\x00" + peer
	c := &d.admitted
	c.mu.Lock()
	now := time.Now()
	if e, ok := c.entries[key]; ok && now.Before(e.until) {
		c.mu.Unlock()
		return e.admitted
	}
	if f, ok := c.flights[key]; ok {
		c.mu.Unlock()
		<-f.done
		return f.admitted
	}
	f := &admitFlight{done: make(chan struct{})}
	if c.flights == nil {
		c.flights = map[string]*admitFlight{}
	}
	c.flights[key] = f
	c.mu.Unlock()

	admitted, err := d.askAdmission(ad.Capability, peer)
	ttl := ad.cacheTTL()
	if err != nil {
		d.logOnce("admit-failed:"+ad.Capability, "anet: inbound.admit: asking %s failed (%v); "+
			"peers on no list are not admitted while it fails", ad.Capability, err)
		ttl = min(ttl, admitErrorTTL)
	}
	c.mu.Lock()
	if c.entries == nil || len(c.entries) >= admitCacheEntries {
		c.prune(time.Now())
	}
	c.entries[key] = admitEntry{admitted: admitted, until: time.Now().Add(ttl)}
	delete(c.flights, key)
	f.admitted = admitted
	close(f.done)
	c.mu.Unlock()
	return admitted
}

// prune drops the expired entries, and all of them when that is not
// enough. Called with mu held.
func (c *admitCache) prune(now time.Time) {
	if c.entries == nil {
		c.entries = map[string]admitEntry{}
		return
	}
	for k, e := range c.entries {
		if !now.Before(e.until) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= admitCacheEntries {
		c.entries = map[string]admitEntry{}
	}
}

// errAdmitUnserved is a question that could not be asked: the service
// module does not serve the capability as internal (any more).
var errAdmitUnserved = errors.New("the service module has no such internal capability")

// askAdmission asks the service whether peer is admitted: true only for a
// reply whose admit member is the JSON value true.
func (d *Daemon) askAdmission(capability, peer string) (bool, error) {
	sb := d.serviceBackend()
	if sb == nil || !sb.InternalCapability(capability) {
		return false, errAdmitUnserved
	}
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, admitCallTimeout)
	defer cancel()
	raw, err := sb.CallInternal(ctx, peer, capability, json.RawMessage(`{}`))
	if err != nil {
		return false, err
	}
	var reply map[string]json.RawMessage
	if json.Unmarshal(raw, &reply) != nil {
		return false, errors.New("the reply is not a JSON object")
	}
	return string(bytes.TrimSpace(reply["admit"])) == "true", nil
}

// stillAccepted reports whether the peer of ix, an inbound interaction
// accepted with trust peer, may still have it acted on: it is on the allow
// or trust list, or, for a text task, inbound.admit admits it now. Its
// deny-list check is the caller's.
func (d *Daemon) stillAccepted(ix *interactions.Interaction, ps peerSets) bool {
	if ps.allowed(ix.PeerAID) {
		return true
	}
	return d.admittedFor(ix.PeerAID, ix.IsCapability, ps)
}

// --- writes and reports ---

// InboundAdmitState is inbound.admit as /inbound/policy reports it.
type InboundAdmitState struct {
	Capability   string `json:"capability"`
	CacheSeconds int    `json:"cache_seconds"`
}

// admitState is inbound.admit as reported, nil when it is not set.
func (in InboundConfig) admitState() *InboundAdmitState {
	if in.Admit == nil {
		return nil
	}
	return &InboundAdmitState{Capability: in.Admit.Capability, CacheSeconds: int(in.Admit.cacheTTL() / time.Second)}
}

// SetInboundAdmit sets inbound.admit, or removes it when admit is nil. The
// capability must be an internal one of the running service module. Saved
// first and in force after, as SetInboundPolicy; the decisions cached under
// the old setting are forgotten.
func (d *Daemon) SetInboundAdmit(admit *InboundAdmit) error {
	if admit != nil {
		a := *admit
		a.Capability = strings.TrimSpace(a.Capability)
		admit = &a
	}
	d.cfgWrite.Lock()
	defer d.cfgWrite.Unlock()
	next := d.config()
	if err := d.carryDiskConfig(&next); err != nil {
		return err
	}
	in := next.inbound()
	from := in.admitState()
	in.Admit = admit
	next.Inbound = &in
	if err := validatePolicy(next, d.untrustedBackend.Load()); err != nil {
		return err // checkInboundAdmit among its checks
	}
	if ref := d.checkAdmitServed(next); ref != nil {
		return ref
	}
	if err := SaveConfig(d.layout, next); err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg.Inbound = next.Inbound
	d.mu.Unlock()
	d.admitted.clear()
	if to := in.admitState(); from != nil || to != nil {
		d.recordPolicyChange("inbound.admit", from, to, nil)
	}
	d.kickTurns() // turns of peers the change admits, or no longer admits
	return nil
}

// admitSaid is the sentence after the inbound line of the status summary
// when inbound.admit is set: which capability admits, and how auto-reply
// runs an admitted peer's turn.
func (d *Daemon) admitSaid(cfg Config) *a2ashape.Said {
	ad := cfg.inbound().Admit
	if ad == nil {
		return nil
	}
	mode := admittedAutoReply(cfg)
	switch mode {
	case "direct", "sandbox":
	default:
		mode = "off"
	}
	return &a2ashape.Said{Key: "inbound.admit." + mode, Args: a2ashape.Args{"capability": ad.Capability}}
}

// admittedAutoReply is auto_reply.admitted as the auto-reply module's
// ConfigSpec reports it: "off" without a block or without the module.
func admittedAutoReply(cfg Config) string {
	if v, ok := moduleSI5(cfg)["auto_reply.admitted"].(string); ok && v != "" {
		return v
	}
	return "off"
}

// --- module.ServiceCaller (design v0.3.9 §B) ---

// serviceCallerPart is the module.ServiceCaller grant.
type serviceCallerPart struct{ d *Daemon }

var _ module.ServiceCaller = serviceCallerPart{}

// viaRe is the form of X-ANet-Via: a short lower-case word.
var viaRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// AgentTools implements module.ServiceCaller.
func (p serviceCallerPart) AgentTools() []module.AgentTool {
	sb := p.d.serviceBackend()
	if sb == nil {
		return nil
	}
	return sb.AgentTools()
}

// CallServiceAs implements module.ServiceCaller. The kernel's part is who:
// the caller is an AID, not on the deny list, and via is a word; the
// service module's is which capability (agent_tool only) and the call.
func (p serviceCallerPart) CallServiceAs(ctx context.Context, callerAID, capability string, args json.RawMessage, via string) (json.RawMessage, error) {
	d := p.d
	sb := d.serviceBackend()
	if sb == nil {
		return nil, module.ErrNotAgentTool
	}
	if !validAIDSyntax(callerAID) {
		return nil, fmt.Errorf("anet: %q is not an AID", callerAID)
	}
	if !viaRe.MatchString(via) {
		return nil, fmt.Errorf("anet: X-ANet-Via %q is not a short lower-case word", via)
	}
	if d.readPeers().denied(callerAID) {
		// The deny list wins here as everywhere: a denied peer's run gets
		// nothing from the service, whatever it still holds.
		return nil, &module.ServiceError{Code: reasonDenied}
	}
	out, err := sb.CallAgentTool(ctx, callerAID, capability, args, via)
	if err != nil && !errors.Is(err, module.ErrNotAgentTool) {
		var se *module.ServiceError
		if !errors.As(err, &se) {
			log.Printf("anet: service call %s as %s: %v", capability, callerAID, err)
		}
	}
	return out, err
}
