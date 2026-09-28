//go:build !no_service

// Package service exposes an ordinary HTTP service as an ANet capability.
//
// This is the answer to the first question anyone asks: I have a service,
// how do I put it on the network? Until now there was none. The daemon
// could offer devices through ANetLink, or the three built-in modules, and
// nothing else — so the network could carry work between agents that
// already spoke ANet, and could not carry anything a person had written.
//
// A capability declared here is a URL. The daemon POSTs the call's
// arguments as JSON and reads the reply back; the service needs to know
// nothing about ANet, AIDs, receipts or CBOR. That is deliberate — asking
// people to adopt a protocol before they can try it is how a network stays
// empty.
//
// Trust is not the operator's to declare. A service that answers is a
// service that acknowledged (V1), and no configuration here can claim
// otherwise, because the daemon has no way to check whether the answer is
// correct — a caption is not verifiable by the machine that requested it.
// A service that CAN say more returns an `evidence` object and that is
// used instead, the same shape ANetLink puts on C1. Letting a config file
// assert V4 would make the trust axis a preference.
//
// What the service is told about the call travels in headers, never in the
// body, so a service written against the plain JSON contract keeps working:
//
//	Authorization: Bearer <token>   when token_file is set: the daemon proves
//	                                itself to the service
//	X-ANet-Caller: <AID>            the caller, only when the daemon
//	                                authenticated it (a relayed delegation)
//	X-ANet-Call: <id>               the interaction id (the voucher id at the
//	                                voucher door)
//	X-ANet-Via: relay|voucher       which door the call came through
//	X-ANet-Capability: <id>         the capability being invoked
//
// The token matters because a service on 127.0.0.1 is reachable by every
// process on the host, including a local agent in the auto-reply sandbox
// (A2A-DESIGN §6). Without it the service cannot tell the daemon from
// anything else that can open a loopback socket, and X-ANet-Caller would be
// a header anyone could write.
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetCore/tsir"

	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

const name = "service"

func init() {
	module.Register(name, func(raw []byte) (module.Module, error) {
		if len(raw) == 0 {
			return nil, nil // compiled in, not configured
		}
		var cfg Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		if len(cfg.Capabilities) == 0 {
			return nil, fmt.Errorf("service: no capabilities declared")
		}
		if err := cfg.check(); err != nil {
			return nil, err
		}
		return &Module{cfg: cfg}, nil
	})
}

// check refuses a configuration before the node advertises anything from
// it. Every limit here is one the card or the transport would otherwise
// enforce later, where the failure is harder to trace back to this file.
func (cfg *Config) check() error {
	if cfg.TimeoutMS < 0 {
		return fmt.Errorf("service: timeout_ms must not be negative")
	}
	if cfg.TokenFile != "" {
		if _, err := tokenPath(cfg.TokenFile); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for i, c := range cfg.Capabilities {
		if c.ID == "" || c.URL == "" {
			return fmt.Errorf("service: capability %d needs both id and url", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("service: capability %q declared twice", c.ID)
		}
		seen[c.ID] = true
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("service: capability %q: url %q is not an http(s) URL", c.ID, c.URL)
		}
		if c.TimeoutMS < 0 {
			return fmt.Errorf("service: capability %q: timeout_ms must not be negative", c.ID)
		}
		if c.TokenFile != "" {
			if _, err := tokenPath(c.TokenFile); err != nil {
				return fmt.Errorf("service: capability %q: %w", c.ID, err)
			}
		}
		// A bearer token sent in cleartext to another host is a token given
		// to every hop on the way. Loopback never leaves the machine; any
		// other host is reached over https or not with a token.
		if cfg.tokenFileFor(&cfg.Capabilities[i]) != "" && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("service: capability %q: a token is only sent over https or to a loopback address, not to %s", c.ID, u.Host)
		}
		if len(c.Name) > provider.MaxSkillNameBytes {
			return fmt.Errorf("service: capability %q: name is %d bytes, limit %d", c.ID, len(c.Name), provider.MaxSkillNameBytes)
		}
		if len(c.Description) > provider.MaxSkillDescriptionBytes {
			return fmt.Errorf("service: capability %q: description is %d bytes, limit %d", c.ID, len(c.Description), provider.MaxSkillDescriptionBytes)
		}
		if len(c.Tags) > provider.MaxSkillTags {
			return fmt.Errorf("service: capability %q: %d tags, limit %d", c.ID, len(c.Tags), provider.MaxSkillTags)
		}
		for _, list := range [][]string{c.Tags, c.Examples, c.InputModes, c.OutputModes} {
			for _, v := range list {
				if strings.TrimSpace(v) == "" {
					return fmt.Errorf("service: capability %q: empty entry in tags, examples or modes", c.ID)
				}
			}
		}
	}
	return nil
}

// Config lists what this node offers and where each one lives.
type Config struct {
	Capabilities []Capability `json:"capabilities"`
	// TimeoutMS bounds one call. A capability that hangs holds a
	// delegation open, and the requester is waiting on the other side of a
	// hub. A capability's own timeout_ms overrides it.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// TokenFile names a file whose first line is the bearer token sent to
	// every service of this module (Authorization: Bearer). A capability's
	// own token_file overrides it, so services run by different people
	// need not share one secret.
	//
	// The path must be absolute; $VAR and ${VAR} are expanded, so a systemd
	// credential can be named as "${CREDENTIALS_DIRECTORY}/token". The file
	// is read once at start and must not be readable by other users.
	TokenFile string `json:"token_file,omitempty"`
}

// Capability is one offering: an id the network calls, and a URL behind it.
type Capability struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	// Price, when set, is what this capability costs in the hub's credit
	// units. Zero is free, and free means no 402 at all rather than a
	// price of nothing — a caller should not have to parse a payment
	// requirement to learn there is none.
	Price uint64 `json:"price,omitempty"`
	// Description is what a peer sees when deciding whether to call this.
	Description string `json:"description,omitempty"`
	// Protocol names what is on the far side, for the evidence record —
	// "http" unless the service is fronting something more specific.
	Protocol string `json:"protocol,omitempty"`

	// Name, Tags, Examples, InputModes and OutputModes describe the
	// capability as an A2A skill, together with Description
	// (provider.Described, A2A-DESIGN §10.2). They are what an A2A client
	// reads on this node's card; left empty, the card says only what the
	// id says. Publishing a skill does not make it callable: only
	// capabilities in inbound.public_capabilities reach the card.
	Name        string   `json:"name,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
	InputModes  []string `json:"input_modes,omitempty"`
	OutputModes []string `json:"output_modes,omitempty"`

	// TimeoutMS overrides the module's timeout_ms for this capability. A
	// digest that takes more than two seconds is broken; a transcription
	// that takes two minutes is working. One bound cannot say both.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// TokenFile overrides the module's token_file for this capability.
	TokenFile string `json:"token_file,omitempty"`
}

// tokenFileFor is the token file that applies to c, or "".
func (cfg *Config) tokenFileFor(c *Capability) string {
	if c.TokenFile != "" {
		return c.TokenFile
	}
	return cfg.TokenFile
}

// defaultTimeout bounds a call when neither the capability nor the module
// sets timeout_ms.
const defaultTimeout = 2 * time.Minute

// timeoutFor is how long one call of c may take, and whether the operator
// set it (rather than it being the default).
func (cfg *Config) timeoutFor(c *Capability) (time.Duration, bool) {
	if c.TimeoutMS > 0 {
		return time.Duration(c.TimeoutMS) * time.Millisecond, true
	}
	if cfg.TimeoutMS > 0 {
		return time.Duration(cfg.TimeoutMS) * time.Millisecond, true
	}
	return defaultTimeout, false
}

// Module registers the declared capabilities.
type Module struct {
	cfg Config
	cli *http.Client
	// tokens maps a capability id to the bearer token sent with its calls;
	// a capability without a token_file is absent.
	tokens map[string]string
}

func (m *Module) Name() string { return name }

func (m *Module) Start(ctx context.Context, h module.Host) error {
	// Each call carries its own deadline (timeoutFor), so the client has
	// none: one client-wide bound would be the shortest or the longest
	// capability's, and wrong for the others.
	m.cli = &http.Client{
		// A redirect would resend the call, headers and all, to a URL the
		// operator did not configure. The service answers where it was
		// told to live, or the call fails and says so.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	m.tokens = map[string]string{}
	read := map[string]string{}
	for i := range m.cfg.Capabilities {
		c := &m.cfg.Capabilities[i]
		f := m.cfg.tokenFileFor(c)
		if f == "" {
			if isLoopbackURL(c.URL) {
				log.Printf("service: %s has no token_file; any process on this host can call %s directly", c.ID, c.URL)
			}
			continue
		}
		tok, ok := read[f]
		if !ok {
			var err error
			if tok, err = readToken(f); err != nil {
				return fmt.Errorf("service: capability %q: %w", c.ID, err)
			}
			read[f] = tok
		}
		m.tokens[c.ID] = tok
	}
	return h.Providers().Register(ctx, &svcProvider{m: m})
}

// minTokenBytes is the shortest token accepted. A token is a password the
// daemon presents on every call; a short one is guessable by anything on
// the host that can open a socket and try.
const minTokenBytes = 16

// tokenPath expands and checks a token_file value. A variable that is not
// set, or set to "", is an error rather than an empty string: outside
// systemd "${CREDENTIALS_DIRECTORY}/token" would otherwise name /token.
func tokenPath(raw string) (string, error) {
	var unset []string
	p := os.Expand(raw, func(k string) string {
		v, ok := os.LookupEnv(k)
		if !ok || v == "" {
			unset = append(unset, k)
		}
		return v
	})
	if len(unset) > 0 {
		return "", fmt.Errorf("service: token_file %q: %s is not set", raw, strings.Join(unset, ", "))
	}
	if p == "" || !filepath.IsAbs(p) {
		return "", fmt.Errorf("service: token_file %q must be an absolute path (after expanding variables)", raw)
	}
	return p, nil
}

// maxTokenFileBytes bounds what is read of a token file; a token is one
// short line.
const maxTokenFileBytes = 4096

// readToken reads a token file: its first line, trimmed. The mode is taken
// from the file that was opened, not from the path, so what was checked is
// what is read.
func readToken(raw string) (string, error) {
	p, err := tokenPath(raw)
	if err != nil {
		return "", err
	}
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("token_file %s is not a regular file", p)
	}
	if fi.Mode().Perm()&0o007 != 0 {
		return "", fmt.Errorf("token_file %s is accessible to other users (mode %04o); chmod o-rwx", p, fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes))
	if err != nil {
		return "", fmt.Errorf("token_file: %w", err)
	}
	tok := string(b)
	if i := strings.IndexAny(tok, "\r\n"); i >= 0 {
		tok = tok[:i]
	}
	tok = strings.TrimSpace(tok)
	if len(tok) < minTokenBytes {
		return "", fmt.Errorf("token_file %s: token is %d bytes, at least %d required", p, len(tok), minTokenBytes)
	}
	for _, r := range tok {
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("token_file %s: token contains whitespace or control characters", p)
		}
	}
	return tok, nil
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && isLoopbackHost(u.Hostname())
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (m *Module) Stop(context.Context) error { return nil }

type svcProvider struct{ m *Module }

func (p *svcProvider) ID() string { return name }

func (p *svcProvider) Capabilities(context.Context) ([]string, error) {
	out := make([]string, 0, len(p.m.cfg.Capabilities))
	for _, c := range p.m.cfg.Capabilities {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out, nil
}

func (p *svcProvider) Describe(context.Context) (string, error) { return "", nil }

// Health reports the module up. It deliberately does not probe the
// services: a capability whose backend is down should fail when called,
// with the reason, rather than removing every capability on this node
// because one of them is restarting.
func (p *svcProvider) Health(context.Context) error { return nil }

func (p *svcProvider) Invoke(ctx context.Context, call provider.Call) (effect.Effect, error) {
	var target *Capability
	for i := range p.m.cfg.Capabilities {
		if p.m.cfg.Capabilities[i].ID == call.Capability {
			target = &p.m.cfg.Capabilities[i]
			break
		}
	}
	if target == nil {
		return effect.Effect{}, fmt.Errorf("service: no capability %q on this node", call.Capability)
	}

	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return effect.Effect{}, err
	}
	to, _ := p.m.cfg.timeoutFor(target)
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return effect.Effect{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := p.m.tokens[target.ID]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// Only a caller the daemon authenticated is named. At the voucher door
	// the AID is the payer the hub attested, and a service that authorized
	// on it would let whoever holds a voucher act as its payer
	// (provider.Call.Via).
	if caller := call.VerifiedCaller(); caller != "" {
		req.Header.Set(HeaderCaller, caller)
	}
	if call.CallID != "" {
		req.Header.Set(HeaderCall, call.CallID)
	}
	if call.Via != "" {
		req.Header.Set(HeaderVia, call.Via)
	}
	req.Header.Set(HeaderCapability, target.ID)

	// Whether the request went out decides what a transport error means:
	// before its header block is written the service cannot have begun the
	// call (provider.TrackSent).
	req, sent := provider.TrackSent(req)

	started := time.Now()
	resp, err := p.m.cli.Do(req)
	if err != nil {
		ev := &effect.Evidence{Protocol: protoOf(target), Requested: call.Capability,
			LatencyMS: time.Since(started).Milliseconds()}
		if sent() {
			// The call went out and its answer did not come back: the
			// deadline passed while the service worked, or the connection
			// dropped before the reply. The effect may have happened, so
			// neither "nothing was attempted" nor "it failed" is true, and
			// a requester told either could run it twice (A2A-DESIGN §4.3).
			return effect.Effect{Status: effect.Unverified, Evidence: ev},
				fmt.Errorf("service %s: %w", call.Capability, provider.AnswerLost(err))
		}
		// The service could not be reached: no connection, or it broke
		// before the request was written. UNAVAILABLE, not FAILED: nothing
		// was attempted at the far end, and a requester deciding whether
		// to retry elsewhere needs that distinction.
		return effect.Effect{
			Status:   effect.Unavailable,
			Message:  fmt.Sprintf("service %s: %v", call.Capability, err),
			Evidence: ev,
		}, nil
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	latency := time.Since(started).Milliseconds()
	if rerr != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// The service took the call — it said so with a 2xx — and its
		// answer broke off: the deadline passed while the body was still
		// coming, or the connection dropped mid-reply. The same unknown
		// outcome as an answer that never started, not FAILED, which says
		// the effect did not happen (redteam F10, a reply lost after its
		// headers). A refusal's status is its answer whatever its body.
		ev := &effect.Evidence{Protocol: protoOf(target), Requested: call.Capability, LatencyMS: latency}
		return effect.Effect{Status: effect.Unverified, Evidence: ev},
			fmt.Errorf("service %s: HTTP %d, then %w", call.Capability, resp.StatusCode, provider.AnswerLost(rerr))
	}

	if resp.StatusCode == http.StatusGatewayTimeout || resp.StatusCode == http.StatusBadGateway {
		// A gateway in front of the service (a reverse proxy) answering for
		// it: 504, the service did not answer the proxy in time; 502, its
		// answer broke. Either way the call reached the service, which may
		// have acted — the same unknown outcome as an answer lost on this
		// side, not FAILED (redteam F10: a proxy_read_timeout shorter than
		// the service's work turns every slow call into "did not happen").
		reason := provider.ReasonConnectionLost
		if resp.StatusCode == http.StatusGatewayTimeout {
			reason = provider.ReasonTimeout
		}
		ev := &effect.Evidence{Protocol: protoOf(target), Requested: call.Capability, LatencyMS: latency}
		return effect.Effect{Status: effect.Unverified, Evidence: ev},
			fmt.Errorf("service %s: %w", call.Capability, &provider.OutcomeUnknownError{Reason: reason,
				Err: fmt.Errorf("HTTP %d from a gateway: %s", resp.StatusCode, snippet(raw))})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 503 and 429 are the service saying "not now": busy, or out of
		// time under load. UNAVAILABLE, like an unreachable service, so the
		// requester knows a retry may succeed; every other refusal is an
		// answer about this call, and FAILED.
		st := effect.Failed
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
			st = effect.Unavailable
		}
		return effect.Effect{
			Status:  st,
			Message: fmt.Sprintf("service %s: HTTP %d: %s", call.Capability, resp.StatusCode, snippet(raw)),
			Evidence: &effect.Evidence{
				Protocol: protoOf(target), Requested: call.Capability, LatencyMS: latency,
			},
		}, nil
	}

	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		return effect.Effect{
			Status:  effect.Failed,
			Message: fmt.Sprintf("service %s: reply is not a JSON object: %s", call.Capability, snippet(raw)),
			Evidence: &effect.Evidence{
				Protocol: protoOf(target), Requested: call.Capability, LatencyMS: latency,
			},
		}, nil
	}

	ev := &effect.Evidence{
		Protocol: protoOf(target), Requested: call.Capability, NativeAck: true,
		LatencyMS: latency,
		// The answer is what the caller came for, so it travels as the
		// observed state — the only channel that can carry a string.
		ObservedState: string(raw),
		// V1 and no higher. The service answered; whether the answer is
		// right is not something this daemon can establish.
		VerifyTrust: 1,
	}
	// A service that can say more about how it knows is believed about
	// itself — the same latitude ANetLink's adapters have — except for
	// trust, which is capped at what was actually established.
	if declared, ok := reply["evidence"].(map[string]any); ok {
		applyDeclared(ev, declared)
	}

	return effect.Effect{
		Status:   effect.OK,
		Record:   &tsir.EffectRecord{Metrics: numbersIn(reply)},
		Evidence: ev,
	}, nil
}

const maxReplyBytes = 1 << 20

// applyDeclared folds a service's own evidence in. Trust is taken as the
// lower of what it claims and what a plain HTTP answer establishes: a
// service asserting V4 over an unauthenticated POST is asserting something
// the transport cannot support, and believing it would make the trust axis
// decorative.
func applyDeclared(ev *effect.Evidence, declared map[string]any) {
	if s, ok := declared["observed_state"].(string); ok && s != "" {
		ev.ObservedState = s
	}
	if s, ok := declared["protocol"].(string); ok && s != "" {
		ev.Protocol = s
	}
	if s, ok := declared["quirk"].(string); ok && s != "" {
		ev.Quirk = s
	}
	if b, ok := declared["native_ack"].(bool); ok {
		ev.NativeAck = b
	}
}

// numbersIn lifts the reply's top-level numbers into metrics, which is
// what a TSIR acceptance predicate can evaluate. Nested objects are left
// in the observed state: flattening them would invent field names.
func numbersIn(reply map[string]any) map[string]float64 {
	out := map[string]float64{}
	for k, v := range reply {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func protoOf(c *Capability) string {
	if c.Protocol != "" {
		return c.Protocol
	}
	return "http"
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

var _ provider.CapabilityProvider = (*svcProvider)(nil)

// Price reports what a declared capability costs. Part of provider.Priced.
func (p *svcProvider) Price(capability string) (uint64, bool) {
	for i := range p.m.cfg.Capabilities {
		c := &p.m.cfg.Capabilities[i]
		if c.ID == capability && c.Price > 0 {
			return c.Price, true
		}
	}
	return 0, false
}

var _ provider.Priced = (*svcProvider)(nil)

// Headers the module sets on every call. They are part of the contract
// with the services behind it, so they are fixed names, not configuration.
const (
	HeaderCaller     = "X-ANet-Caller"
	HeaderCall       = "X-ANet-Call"
	HeaderVia        = "X-ANet-Via"
	HeaderCapability = "X-ANet-Capability"
)

func (p *svcProvider) capability(id string) *Capability {
	for i := range p.m.cfg.Capabilities {
		if p.m.cfg.Capabilities[i].ID == id {
			return &p.m.cfg.Capabilities[i]
		}
	}
	return nil
}

// SkillInfo describes a declared capability as an A2A skill, from its
// configuration. Part of provider.Described.
func (p *svcProvider) SkillInfo(capability string) (provider.SkillInfo, bool) {
	c := p.capability(capability)
	if c == nil {
		return provider.SkillInfo{}, false
	}
	si := provider.SkillInfo{
		Name: c.Name, Description: c.Description,
		Tags: c.Tags, Examples: c.Examples,
		InputModes: c.InputModes, OutputModes: c.OutputModes,
	}
	if si.Name == "" && si.Description == "" && len(si.Tags) == 0 && len(si.Examples) == 0 &&
		len(si.InputModes) == 0 && len(si.OutputModes) == 0 {
		return provider.SkillInfo{}, false
	}
	return si, true
}

var _ provider.Described = (*svcProvider)(nil)

// InvokeTimeout reports the bound the operator set for a capability. Part
// of provider.LongRunning.
//
// Without it the daemon's own bound applied on top of timeout_ms, and a
// capability configured for three minutes was stopped at one, with only
// the shorter of two limits ever visible. A capability whose bound is the
// default says nothing and gets the daemon's.
func (p *svcProvider) InvokeTimeout(capability string) (time.Duration, bool) {
	c := p.capability(capability)
	if c == nil {
		return 0, false
	}
	to, set := p.m.cfg.timeoutFor(c)
	if !set {
		return 0, false
	}
	return to, true
}

var _ provider.LongRunning = (*svcProvider)(nil)
