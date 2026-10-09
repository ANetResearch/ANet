package netcard

// extended.go builds the extended cards (A2A §3.1.11, §13.3): the AgentCard
// a node signs and gives a caller that asks for it end to end with
// anet.card.get/1, never through a hub or a directory. Every public card
// declares capabilities.extendedAgentCard (Build), so the flag says nothing
// about what an extended card adds.
//
// There are two at a time (core X-CUX-6, CORE-K23 rule 1):
//
//	general  the public card and the prices of its skills, for every caller;
//	list     the public card, the capabilities served to the allow and trust
//	         lists only (Input.ListOnly), and the prices of all of them.
//
// The general variant never lists a ListOnly skill. Prices travel in one
// extension, anet-price/v1, which only an extended card carries, and which
// is checked here against the skills of the card it is on. Each card states
// a2a-x402 for itself: declared when one of its skills is priced, required
// when all are, so a caller handed the list variant sees the declaration
// that card needs even when the public card has none (a2a-x402 §3).
//
// A variant that would say nothing the public card does not — no price and
// no skill beyond it — is not an extended card at all: BuildExtended reports
// it as the same, and the provider answers not_configured (PAY-C8).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/payment"

	"github.com/ANetResearch/ANet/module"
)

// retiredPricingURI is the public price list of 0.2 (anet-pricing/v1). No
// card carries it any more; a contribution of it is dropped everywhere.
const retiredPricingURI = "https://agentnetwork.org.cn/a2a/ext/anet-pricing/v1"

// isPriceURI reports whether uri is a price list.
func isPriceURI(uri string) bool { return uri == module.ExtPriceURI || uri == retiredPricingURI }

// ErrBadPrices is wrapped by every refusal of a price list (PricesOf,
// BuildExtended): the card would state prices a caller cannot rely on.
var ErrBadPrices = errors.New("netcard: the price list is not valid")

// maxAmount is the largest price a card states: 2^63-1, the largest amount a
// hub's ledger holds.
const maxAmount = math.MaxInt64

// VariantSkills returns the skills variant v lists, sorted by id: the
// public skills, and on the list variant the ListOnly skills beside them.
// These are the skills a module is asked to contribute for
// (module.CardContext.Skills).
func VariantSkills(in Input, v module.CardVariant) ([]Skill, error) {
	switch v {
	case module.VariantGeneral:
		return sortedSkills(in.Skills), nil
	case module.VariantList:
		all := append(append([]Skill(nil), in.Skills...), in.ListOnly...)
		return sortedSkills(all), nil
	default:
		return nil, fmt.Errorf("netcard: %q is not an extended card variant", v)
	}
}

// SkillIDs returns the ids of skills, in order.
func SkillIDs(skills []Skill) []string {
	out := make([]string, 0, len(skills))
	for _, s := range skills {
		out = append(out, s.ID)
	}
	return out
}

func sortedSkills(in []Skill) []Skill {
	out := append([]Skill(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// BuildExtended returns the unsigned extended card of variant v and whether
// it is the same as the public card, Build(in): built from the same in, they
// are then the same bytes, and there is no extended card to give (the
// provider answers not_configured). in is the public card's input, with its
// seq and times; contributions are the modules' extension contributions for
// this variant (module.CardContext with Extended set and this variant's
// skills, VariantSkills), each already accepted by Extension.
//
// Interfaces are in.Interfaces. The retired public price list is dropped; an
// anet-price/v1 contribution is checked (PricesOf): a price list that does
// not hold is an error, never a card with prices a caller cannot rely on.
// The card's a2a-x402 declaration must match its prices: declared exactly
// when a skill is priced, required exactly when all are.
func BuildExtended(in Input, v module.CardVariant, contributions []map[string]any) ([]byte, bool, error) {
	skills, err := VariantSkills(in, v)
	if err != nil {
		return nil, false, err
	}
	exts := make([]map[string]any, 0, len(contributions))
	lists := 0
	for _, e := range contributions {
		switch u, _ := e["uri"].(string); u {
		case retiredPricingURI:
			continue
		case module.ExtPriceURI:
			// Two lists are two answers to what a skill costs; the card
			// keeps one copy of a repeated extension, and which one would be
			// an accident of order.
			if lists++; lists > 1 {
				return nil, false, fmt.Errorf("%w: two modules offer a price list for the %s card", ErrBadPrices, v)
			}
		}
		exts = append(exts, e)
	}
	card, err := build(in, skills, exts)
	if err != nil {
		return nil, false, err
	}
	prices, err := PricesOf(card)
	if err != nil {
		return nil, false, err
	}
	declared, required, err := X402Declaration(card)
	if err != nil {
		return nil, false, err
	}
	switch {
	case declared != (len(prices) > 0):
		return nil, false, fmt.Errorf("%w: the %s card prices %d skills but its a2a-x402 declaration says %v",
			ErrBadPrices, v, len(prices), declared)
	case declared && required != (len(prices) == len(skills)):
		return nil, false, fmt.Errorf("%w: the %s card prices %d of %d skills but a2a-x402 required is %v",
			ErrBadPrices, v, len(prices), len(skills), required)
	}
	public, err := Build(in)
	if err != nil {
		return nil, false, err
	}
	return card, bytes.Equal(card, public), nil
}

// PricesOf reads the price list of a card (anet-price/v1): skill id to
// amount, empty when the card has none. It refuses, wrapping ErrBadPrices,
// a list that is not exactly {network, asset, prices} with network a hub
// ledger ("hub:" and the hub's AID), asset credit, and prices a non-empty
// array of {skillId, amount} whose skill ids are distinct skills of this
// card and whose amounts are canonical decimals of 1..2^63-1; and a card
// that carries it twice.
func PricesOf(cardJSON []byte) (map[string]uint64, error) {
	c, err := parseCard(cardJSON)
	if err != nil {
		return nil, err
	}
	skills := map[string]bool{}
	for _, s := range c.Skills {
		skills[s.ID] = true
	}
	out := map[string]uint64{}
	seen := false
	for _, e := range c.Capabilities.Extensions {
		if e.URI != module.ExtPriceURI {
			continue
		}
		if seen {
			return nil, fmt.Errorf("%w: the card carries two price lists", ErrBadPrices)
		}
		seen = true
		if e.Required {
			return nil, fmt.Errorf("%w: a price list is never required", ErrBadPrices)
		}
		if err := readPrices(e.Params, skills, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readPrices(params map[string]any, skills map[string]bool, out map[string]uint64) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrBadPrices}, a...)...)
	}
	for k := range params {
		if k != "network" && k != "asset" && k != "prices" {
			return bad("params has an unknown member %q", k)
		}
	}
	network, _ := params["network"].(string)
	if hub, ok := cutPrefix(network, "hub:"); !ok || !identity.ValidAID(hub) {
		return bad("network %q is not a hub ledger", network)
	}
	if asset, _ := params["asset"].(string); asset != payment.AssetCredit {
		return bad("asset %q is not %s", asset, payment.AssetCredit)
	}
	list, ok := params["prices"].([]any)
	if !ok || len(list) == 0 {
		return bad("prices is not a non-empty array")
	}
	for i, raw := range list {
		p, ok := raw.(map[string]any)
		if !ok || len(p) != 2 {
			return bad("prices[%d] is not {skillId, amount}", i)
		}
		id, _ := p["skillId"].(string)
		amount, _ := p["amount"].(string)
		if !skills[id] {
			return bad("prices[%d] names %q, which is not a skill of this card", i, id)
		}
		if _, dup := out[id]; dup {
			return bad("prices names %q twice", id)
		}
		n, ok := parseAmount(amount)
		if !ok {
			return bad("the price of %q is %q, not a whole number of credits from 1 to 2^63-1", id, amount)
		}
		out[id] = n
	}
	return nil
}

// parseAmount reads a canonical decimal of 1..2^63-1: no sign, no leading
// zero, nothing but digits.
func parseAmount(s string) (uint64, bool) {
	if s == "" || s[0] == '0' || len(s) > 19 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n > maxAmount {
		return 0, false
	}
	return n, true
}

// ValidPrice reports whether n is a price a card can state: 1..2^63-1.
func ValidPrice(n uint64) bool { return n >= 1 && n <= maxAmount }

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}

// X402Declaration reads a card's a2a-x402 declaration: whether it declares
// the extension and whether as required. A card that declares it twice is
// an error.
func X402Declaration(cardJSON []byte) (declared, required bool, err error) {
	c, err := parseCard(cardJSON)
	if err != nil {
		return false, false, err
	}
	for _, e := range c.Capabilities.Extensions {
		if e.URI != module.ExtX402URI {
			continue
		}
		if declared {
			return false, false, errors.New("netcard: the card declares a2a-x402 twice")
		}
		declared, required = true, e.Required
	}
	return declared, required, nil
}

// readCard is what PricesOf, X402Declaration and DeclaresExtended read of
// a card. Members are read by their exact names, as a2acard.Verify and
// a2a-python read them: encoding/json would also take "URI" for "uri".
type readCard struct {
	Capabilities struct {
		ExtendedAgentCard bool
		Extensions        []readExt
	}
	Skills []struct{ ID string }
}

type readExt struct {
	URI      string
	Required bool
	Params   map[string]any
}

func parseCard(cardJSON []byte) (*readCard, error) {
	bad := func(what string, err error) (*readCard, error) {
		if err == nil {
			return nil, fmt.Errorf("netcard: the card is not readable: %s", what)
		}
		return nil, fmt.Errorf("netcard: the card is not readable: %s: %w", what, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(cardJSON, &top); err != nil || top == nil {
		return bad("not a JSON object", err)
	}
	var c readCard
	var skills []map[string]json.RawMessage
	if raw, ok := top["skills"]; ok {
		if err := json.Unmarshal(raw, &skills); err != nil {
			return bad("skills", err)
		}
	}
	for _, sk := range skills {
		var id string
		if raw, ok := sk["id"]; ok {
			if err := json.Unmarshal(raw, &id); err != nil {
				return bad("skill id", err)
			}
		}
		c.Skills = append(c.Skills, struct{ ID string }{ID: id})
	}
	var caps map[string]json.RawMessage
	if raw, ok := top["capabilities"]; ok {
		if err := json.Unmarshal(raw, &caps); err != nil {
			return bad("capabilities", err)
		}
	}
	if raw, ok := caps["extendedAgentCard"]; ok {
		if err := json.Unmarshal(raw, &c.Capabilities.ExtendedAgentCard); err != nil {
			return bad("capabilities.extendedAgentCard", err)
		}
	}
	var exts []map[string]json.RawMessage
	if raw, ok := caps["extensions"]; ok {
		if err := json.Unmarshal(raw, &exts); err != nil {
			return bad("capabilities.extensions", err)
		}
	}
	for _, e := range exts {
		var x readExt
		for name, dst := range map[string]any{"uri": &x.URI, "required": &x.Required, "params": &x.Params} {
			if raw, ok := e[name]; ok {
				if err := json.Unmarshal(raw, dst); err != nil {
					return bad("extension "+name, err)
				}
			}
		}
		c.Capabilities.Extensions = append(c.Capabilities.Extensions, x)
	}
	return &c, nil
}

// DeclaresExtended reports whether a card declares
// capabilities.extendedAgentCard as true.
func DeclaresExtended(cardJSON []byte) (bool, error) {
	c, err := parseCard(cardJSON)
	if err != nil {
		return false, err
	}
	return c.Capabilities.ExtendedAgentCard, nil
}
