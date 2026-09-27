//go:build !no_a2a

package a2a

// card.go makes the proxy cards (A2A-DESIGN §11.3): for each remote agent,
// the card an A2A client reads to reach it through this interface.
//
// A proxy card is this node's statement, not the remote agent's. Its
// interfaces are this listener's URLs and its security scheme is this
// interface's bearer token, so it is signed by this node's key (kid
// did:anet:<this AID>#<seq>, no jku: there is no JWKS to point at, and a
// client that wants to check it resolves this node's KEL). What the remote
// agent said about itself is carried inside, whole: anet-origin/v1 holds its
// network card's bytes and whether this node verified them, and a client
// that wants the agent's own word checks that signature.
//
// The card is written in the publish form (A2A-DESIGN §10.1, A2A §8.4.1):
// every REQUIRED member present and non-empty, every member at its default
// value left out, no empty value inside extension params. On that form the
// payload every A2A verifier reconstructs is the same, and a signature made
// here verifies in a2a-go, in a2a-python and by the specification's rule.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// Extension URIs of the proxy card. anet-pricing is the network card's
// (A2A-DESIGN §10.1); anet-origin is the proxy card's own.
const (
	// ExtOriginURI says which remote agent a proxy card stands for:
	// params {aid, originVerification, originCard?, anet.official?},
	// originCard being the remote network card's bytes, base64url without
	// padding, and anet.official true when this node's official manifest
	// lists the agent by that AID (A2A-DESIGN §15).
	ExtOriginURI  = "https://agentnetwork.org.cn/a2a/ext/anet-origin/v1"
	extPricingURI = "https://agentnetwork.org.cn/a2a/ext/anet-pricing/v1"
)

// Verification values of a remote card (module.RemoteAgent.Verification).
const (
	verified   = "VERIFIED"
	unverified = "UNVERIFIED"
)

// securityScheme names this interface's bearer token in the cards.
const securityScheme = "anetLocal"

// originOfficial is the anet-origin param that marks an official agent:
// true, or absent (§10.1: no false, no empty values inside params).
const originOfficial = "anet.official"

// maxOriginCard bounds the remote card carried in anet-origin: the limit a
// network card is admitted under (ANetCore a2acard.MaxCardBytes).
const maxOriginCard = 64 << 10

// maxNameBytes is the longest name a card may have (ANetCore
// a2acard.MaxNameBytes).
const maxNameBytes = 128

// cardBuilder builds proxy cards for one listener.
type cardBuilder struct {
	host, port string
}

// build makes the proxy card for a remote agent, and lists the extension
// URIs it declares (what the interface echoes back to a client that
// activates them).
func (b cardBuilder) build(ra module.RemoteAgent) (map[string]any, []string) {
	base := "http://" + listenerURLHost(b.host, b.port) + agentsPath + "/" + ra.AID
	card := map[string]any{
		"supportedInterfaces": []any{
			map[string]any{"url": base + "/jsonrpc", "protocolBinding": string(a2a.TransportProtocolJSONRPC), "protocolVersion": string(a2a.Version)},
			map[string]any{"url": base + "/rest", "protocolBinding": string(a2a.TransportProtocolHTTPJSON), "protocolVersion": string(a2a.Version)},
		},
		"securitySchemes": map[string]any{securityScheme: map[string]any{"httpAuthSecurityScheme": map[string]any{
			"scheme":      "Bearer",
			"description": "The local A2A token of this anet node (a2a_token.txt in the a2a module's state directory).",
		}}},
		// A non-empty scope list: {} is the default value, which a2a-python
		// drops before signing — and the whole requirement with it — so a
		// card saying "Bearer required" with {} is a card a2a-python
		// verifies as saying nothing. HTTP Bearer ignores scopes.
		"securityRequirements": []any{map[string]any{"schemes": map[string]any{securityScheme: map[string]any{"list": []any{"a2a"}}}}},
	}

	// VERIFIED is said only of a card that is here to be read: without its
	// bytes, or with bytes that do not parse, the card is a placeholder, and
	// a placeholder calling its origin verified would be vouching for
	// nothing.
	verification := unverified
	var remote *a2a.AgentCard
	if ra.Verification == verified && len(ra.Card) > 0 {
		var c a2a.AgentCard
		if err := json.Unmarshal(ra.Card, &c); err == nil {
			remote, verification = &c, verified
		}
	}

	origin := map[string]any{"aid": ra.AID, "originVerification": verification}
	if n := len(ra.Card); n > 0 && n <= maxOriginCard {
		origin["originCard"] = base64.RawURLEncoding.EncodeToString(ra.Card)
	}
	// This node's statement, like the rest of the proxy card: the AID is on
	// the official manifest it carries. It does not depend on the remote
	// card, which the agent writes about itself.
	if ra.Official {
		origin[originOfficial] = true
	}
	exts := []any{map[string]any{
		"uri":         ExtOriginURI,
		"description": "The remote anet agent this card stands for, and whether this node verified the agent's own card.",
		"params":      origin,
	}}
	uris := []string{ExtOriginURI}

	if remote != nil {
		copyVerified(card, remote)
		var x402, pricing bool
		for _, e := range remote.Capabilities.Extensions {
			switch e.URI {
			case a2ashape.X402ExtensionURI:
				x402 = true
			case extPricingURI:
				pricing = true
				ext := map[string]any{"uri": extPricingURI}
				if p := clean(e.Params); p != nil {
					ext["params"] = p
				}
				if d := strings.TrimSpace(e.Description); d != "" {
					ext["description"] = d
				}
				exts = append(exts, ext)
				uris = append(uris, extPricingURI)
			}
		}
		if x402 || pricing {
			// This node is the signing service (a2a-x402 §5.1, A2A-DESIGN
			// §8.7): the client says payment-submitted and the daemon
			// signs. Not required — "required" is omitted rather than
			// written false (§10.1) — because a client that does not
			// activate the extension is still paid for within the
			// automatic tier.
			exts = append(exts, map[string]any{
				"uri":         a2ashape.X402ExtensionURI,
				"description": "Payments are signed by the local anet daemon: answer payment-required with payment-submitted and no payload.",
				"params":      map[string]any{"signer": "anet-daemon", "clientPayload": false},
			})
			uris = append(uris, a2ashape.X402ExtensionURI)
		}
	} else {
		placeholder(card, ra)
	}
	card["capabilities"] = map[string]any{
		// This interface streams whatever the remote agent's card says:
		// the stream is between the client and this node.
		"streaming":         true,
		"pushNotifications": false,
		"extensions":        exts,
	}
	return card, uris
}

// copyVerified copies what a client needs of the remote agent's own card:
// who it is and what it does. Its interfaces and security are its own
// relay's and not copied; nor are its URLs (documentation, icon), which a
// client might fetch.
func copyVerified(card map[string]any, c *a2a.AgentCard) {
	card["name"] = orDefault(strings.TrimSpace(c.Name), "anet agent")
	card["description"] = orDefault(strings.TrimSpace(c.Description), "An anet agent; its card gives no description.")
	card["version"] = orDefault(strings.TrimSpace(c.Version), "unknown")
	card["defaultInputModes"] = modes(c.DefaultInputModes)
	card["defaultOutputModes"] = modes(c.DefaultOutputModes)
	var skills []any
	for _, s := range c.Skills {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			continue
		}
		sk := map[string]any{
			"id":          id,
			"name":        orDefault(strings.TrimSpace(s.Name), id),
			"description": orDefault(strings.TrimSpace(s.Description), "The agent gives no description of "+id+"."),
			"tags":        orList(nonEmpty(s.Tags), []any{id}),
		}
		if l := nonEmpty(s.Examples); l != nil {
			sk["examples"] = l
		}
		if l := nonEmpty(s.InputModes); l != nil {
			sk["inputModes"] = l
		}
		if l := nonEmpty(s.OutputModes); l != nil {
			sk["outputModes"] = l
		}
		skills = append(skills, sk)
	}
	if len(skills) == 0 {
		skills = []any{chatSkill()}
	}
	card["skills"] = skills
}

// placeholder fills a card for an agent with no verified card of its own.
// It says so rather than guessing: the name is the hub directory's (not
// signed by the agent), and the one skill is plain conversation.
func placeholder(card map[string]any, ra module.RemoteAgent) {
	name := strings.TrimSpace(ra.Name)
	if name == "" {
		name = "anet agent " + short(ra.AID)
	}
	if len(name) > maxNameBytes {
		// The limit is in bytes (ANetCore a2acard.MaxNameBytes), and the
		// cut falls between characters: one split in half is not the text
		// the hub gave, nor valid UTF-8.
		cut := maxNameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	card["name"] = name
	card["description"] = "The anet agent " + ra.AID + ", reached through this node. It has no card this node could " +
		"verify, so nothing here comes from the agent itself: the name is the hub directory's, and its skills are unknown."
	card["version"] = "unknown"
	card["defaultInputModes"] = []any{"text/plain"}
	card["defaultOutputModes"] = []any{"text/plain"}
	card["skills"] = []any{chatSkill()}
}

func chatSkill() map[string]any {
	return map[string]any{
		"id":          "chat",
		"name":        "chat",
		"description": "Send the agent a message in natural language; it answers in the task.",
		"tags":        []any{"chat"},
	}
}

func short(aid string) string {
	if len(aid) > 12 {
		return aid[:12]
	}
	return aid
}

// modes is a REQUIRED list of media types: the non-empty ones, or
// text/plain.
func modes(l []string) []any {
	return orList(nonEmpty(l), []any{"text/plain"})
}

func nonEmpty(l []string) []any {
	var out []any
	for _, s := range l {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func orList(l, def []any) []any {
	if len(l) == 0 {
		return def
	}
	return l
}

// clean removes empty values from extension params, recursively: "", nil,
// [] and {} inside a Struct are values some verifiers drop before signing
// and others keep (A2A-DESIGN §10.1). nil when nothing is left.
func clean(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if c, ok := cleanValue(v); ok {
			out[k] = c
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanValue(v any) (any, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case string:
		return x, x != ""
	case map[string]any:
		c := clean(x)
		return c, c != nil
	case []any:
		var out []any
		for _, e := range x {
			if c, ok := cleanValue(e); ok {
				out = append(out, c)
			}
		}
		return out, len(out) > 0
	}
	return v, true
}

// cardCache holds the signed proxy cards, so a client reading a card and
// every request that echoes extensions does not each ask the hub again.
type cardCache struct {
	seam    module.TaskSeam
	builder cardBuilder
	signer  module.ProxyCardSigner
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]cardEntry
}

type cardEntry struct {
	body  []byte
	exts  []string
	until time.Time
}

// How long a card is kept: a verified one five minutes, and the
// placeholder for an agent whose card could not be had half a minute, so a
// hub that was briefly unreachable does not leave a placeholder for long.
const (
	cardTTL       = 5 * time.Minute
	cardRetryTTL  = 30 * time.Second
	maxCardsCache = 1024
)

func newCardCache(seam module.TaskSeam, b cardBuilder, signer module.ProxyCardSigner) *cardCache {
	return &cardCache{seam: seam, builder: b, signer: signer, now: time.Now, entries: map[string]cardEntry{}}
}

// card is the proxy card's bytes for aid.
func (c *cardCache) card(ctx context.Context, aid string) ([]byte, error) {
	e, err := c.get(ctx, aid)
	return e.body, err
}

// extensions are the extension URIs aid's proxy card declares. An agent
// whose card cannot be made declares only anet-origin.
func (c *cardCache) extensions(ctx context.Context, aid string) []string {
	e, err := c.get(ctx, aid)
	if err != nil {
		return []string{ExtOriginURI}
	}
	return e.exts
}

func (c *cardCache) get(ctx context.Context, aid string) (cardEntry, error) {
	now := c.now()
	c.mu.Lock()
	e, ok := c.entries[aid]
	c.mu.Unlock()
	if ok && now.Before(e.until) {
		return e, nil
	}
	ttl := cardTTL
	ra, err := c.seam.Card(ctx, aid)
	if err != nil || ra.AID != aid {
		// Unknown to the network, or not reachable now: a placeholder,
		// which says it is one.
		ra = module.RemoteAgent{AID: aid, Verification: unverified}
		ttl = cardRetryTTL
	}
	if ra.Verification != verified {
		ttl = cardRetryTTL
	}
	card, exts := c.builder.build(ra)
	body, err := json.Marshal(card)
	if err != nil {
		return cardEntry{}, err
	}
	if c.signer != nil {
		if body, err = c.signer.SignProxyCard(body); err != nil {
			return cardEntry{}, fmt.Errorf("sign: %w", err)
		}
	}
	e = cardEntry{body: body, exts: exts, until: now.Add(ttl)}
	c.mu.Lock()
	if len(c.entries) >= maxCardsCache {
		for k, old := range c.entries {
			if !now.Before(old.until) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxCardsCache {
			c.entries = map[string]cardEntry{}
		}
	}
	c.entries[aid] = e
	c.mu.Unlock()
	return e, nil
}
