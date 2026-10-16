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
// agent said about itself is carried inside, whole, when this node verified
// it: anet-origin/v1 holds its network card's bytes, and a client that
// wants the agent's own word checks that signature. A card this node could
// not verify is not carried in any form (an internal note item): the proxy card is then
// the one for an agent without a card, made from the AID.
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

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// Extension URIs of the proxy card: anet-origin is the proxy card's own.
const (
	// ExtOriginURI says which remote agent a proxy card stands for:
	// params {aid, originVerification, keyState, originCard?,
	// originExtendedCard?, anet.official?}, originCard being the remote
	// network card's bytes, base64url without padding, present only when
	// originVerification is VERIFIED; originExtendedCard the remote extended
	// card's bytes, the same way, only on a proxy extended card (HPP §2.6);
	// keyState "fresh" when the agent's hub confirmed its current key and
	// "stale" otherwise (KEL A-2, D-28); and anet.official true when this
	// node's official manifest lists the agent by that AID
	// (A2A-DESIGN §15).
	ExtOriginURI = "https://agentnetwork.org.cn/a2a/ext/anet-origin/v1"
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

// originKeyState is anet-origin's keyState for ra (KEL A-2): fresh only
// when the kernel said so; anything else — an agent the directory does not
// hold, a placeholder — is stale.
func originKeyState(ra module.RemoteAgent) string {
	if ra.KeyState == module.KeyStateFresh {
		return module.KeyStateFresh
	}
	return module.KeyStateStale
}

// cardBuilder builds proxy cards for one listener.
type cardBuilder struct {
	host, port string
	// canPay says whether this node can pay now (TaskSeam.CanPay). The
	// proxy card declares a2a-x402 only when it does; nil is no (MOD-P7).
	canPay func() bool
}

// build makes the proxy card for a remote agent, and lists the extension
// URIs it declares (what the interface echoes back to a client that
// activates them).
func (b cardBuilder) build(ra module.RemoteAgent) (map[string]any, []string) {
	card, uris, _ := b.buildFrom(ra, nil)
	return card, uris
}

// buildExtended makes the proxy extended card (HPP §2.6): the proxy card
// rule applied to the remote agent's verified extended card rather than its
// public card — its skills, the a2a-x402 declaration with this node as the
// signing service, and its price list as the agent signed it — with the
// extended card's own bytes in anet-origin (originExtendedCard).
func (b cardBuilder) buildExtended(ea module.ExtendedAgent) (map[string]any, []string, error) {
	if len(ea.Card) == 0 {
		return nil, nil, fmt.Errorf("no extended card")
	}
	return b.buildFrom(ea.Agent, ea.Card)
}

// buildFrom is build, and buildExtended when ext is the remote agent's
// verified extended card.
func (b cardBuilder) buildFrom(ra module.RemoteAgent, ext []byte) (map[string]any, []string, error) {
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
	var prices map[string]any
	if ext != nil {
		// The extended card stands in for the public one: what the remote
		// agent says of itself is read from it.
		if remote == nil {
			return nil, nil, fmt.Errorf("an extended card for an agent whose public card is not verified")
		}
		var c a2a.AgentCard
		if err := json.Unmarshal(ext, &c); err != nil {
			return nil, nil, fmt.Errorf("the extended card: %w", err)
		}
		remote = &c
		var err error
		if prices, err = priceParams(ext); err != nil {
			return nil, nil, err
		}
	}

	origin := map[string]any{"aid": ra.AID, "originVerification": verification, "keyState": originKeyState(ra)}
	// The remote card's bytes only when they verified: a card that did not
	// is not passed on in any form (an internal note item), and the proxy card is then
	// the one for an agent without a card.
	if n := len(ra.Card); remote != nil && n <= maxOriginCard {
		origin["originCard"] = base64.RawURLEncoding.EncodeToString(ra.Card)
	}
	// The extended card's bytes beside them, which this node verified: a
	// client checks the agent's own signature on its prices there.
	if n := len(ext); n > 0 && n <= maxOriginCard {
		origin["originExtendedCard"] = base64.RawURLEncoding.EncodeToString(ext)
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
		// A public card carries no price (0.3.0): nothing of a remote
		// card's extensions is passed on but whether it takes payment.
		x402, preview := false, false
		for _, e := range remote.Capabilities.Extensions {
			switch e.URI {
			case a2ashape.X402ExtensionURI:
				x402 = true
			case module.ExtPreviewURI:
				preview = true
			}
		}
		if preview {
			// The verified remote card says the agent sends previews
			// (CUX-P30): the same declaration, not required. A card that is
			// not verified, or does not say so, leaves it out; a client may
			// activate the extension all the same.
			exts = append(exts, map[string]any{
				"uri":         module.ExtPreviewURI,
				"description": "This agent sends a preview of its reply while it is written; activate the extension to receive it as updates of the anet.reply artifact.",
			})
			uris = append(uris, module.ExtPreviewURI)
		}
		if x402 && b.canPay != nil && b.canPay() {
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
		if prices != nil {
			// The prices as the agent signed them, carried over unchanged;
			// originExtendedCard holds its signature.
			exts = append(exts, map[string]any{
				"uri":         module.ExtPriceURI,
				"description": "The price of each skill, as the remote agent signed it on its extended card; a skill not listed is free.",
				"params":      prices,
			})
			uris = append(uris, module.ExtPriceURI)
		}
	} else {
		placeholder(card, ra)
	}
	caps := map[string]any{
		// This interface streams whatever the remote agent's card says:
		// the stream is between the client and this node.
		"streaming":         true,
		"pushNotifications": false,
		"extensions":        exts,
	}
	// An extended card is declared when this node would ask the agent for
	// one (core CORE-K23 rule 3): what a client sees promised is what
	// GetExtendedAgentCard does. false is the default, left out (§10.1).
	if remote != nil && (ext != nil || ra.ExtendedOffered) {
		caps["extendedAgentCard"] = true
	}
	card["capabilities"] = caps
	return card, uris, nil
}

// priceParams are the params of the price list an extended card carries,
// exactly as written; nil when it has none.
func priceParams(card []byte) (map[string]any, error) {
	var c struct {
		Capabilities struct {
			Extensions []struct {
				URI    string         `json:"uri"`
				Params map[string]any `json:"params"`
			} `json:"extensions"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(card, &c); err != nil {
		return nil, fmt.Errorf("the extended card: %w", err)
	}
	for _, e := range c.Capabilities.Extensions {
		if e.URI == module.ExtPriceURI {
			return e.Params, nil
		}
	}
	return nil, nil
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

// placeholder fills a card for an agent with no verified card of its own:
// no card at all, or one this node could not verify, which are shown the
// same way (an internal note item). It is made from the AID alone and says so: nothing
// the hub states about the agent — a name, a description, a forged card's
// words — is put next to this node's signature, or next to the official
// mark in anet-origin, which is decided by the AID. The one skill is plain
// conversation.
func placeholder(card map[string]any, ra module.RemoteAgent) {
	card["name"] = "anet agent " + short(ra.AID)
	card["description"] = "The anet agent " + ra.AID + ", reached through this node. It has no card this node could " +
		"verify, so nothing here comes from the agent itself: the name is made from its AID, and its skills are unknown."
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
	// canPay is what CanPay answered when the card was made: a card is
	// made again as soon as the answer differs, so a node that registers
	// at its hub after start declares a2a-x402 without waiting for the
	// cache or a restart (K-11).
	canPay bool
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
	canPay := c.seam.CanPay()
	c.mu.Lock()
	e, ok := c.entries[aid]
	c.mu.Unlock()
	if ok && now.Before(e.until) && e.canPay == canPay {
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
	b := c.builder
	b.canPay = func() bool { return canPay }
	card, exts := b.build(ra)
	body, err := json.Marshal(card)
	if err != nil {
		return cardEntry{}, err
	}
	if c.signer != nil {
		if body, err = c.signer.SignProxyCard(body); err != nil {
			return cardEntry{}, fmt.Errorf("sign: %w", err)
		}
	}
	e = cardEntry{body: body, exts: exts, until: now.Add(ttl), canPay: canPay}
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
