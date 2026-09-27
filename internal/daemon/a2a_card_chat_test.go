package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/effect"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/module"
	"github.com/ANetResearch/ANet/provider"
)

// setPolicyQuiet sets inbound.policy without the background republish
// SetInboundPolicy starts.
func setPolicyQuiet(d *Daemon, policy string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	in := d.cfg.inbound()
	in.Policy = policy
	d.cfg.Inbound = &in
}

// cardSkills reads the skills of a card.
func cardSkills(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var c struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("card: %v", err)
	}
	return c.Skills
}

func skillIDs(skills []map[string]any) []string {
	var out []string
	for _, s := range skills {
		id, _ := s["id"].(string)
		out = append(out, id)
	}
	return out
}

// 0017 Q27: a node whose inbound policy is open takes natural-language
// tasks from anyone, and its network card says so with a chat skill — the
// only skill of a node with no public capability, and beside the public
// capabilities of one that has some. The skill describes itself as it is:
// natural language, who may send it, and that the hub only carries it. A
// closed or approving node with nothing public still publishes no card.
func TestAnOpenNodeListsAChatSkill(t *testing.T) {
	h := newFakeHub(t)
	d := newCardDaemon(t, "Talker", "")
	for _, p := range []string{PolicyClosed, PolicyApprove} {
		setPolicyQuiet(d, p)
		if _, _, err := d.networkCard(h.URL); err != errNoPublicSkill {
			t.Fatalf("%s with nothing public: %v, want errNoPublicSkill", p, err)
		}
	}

	setPolicyQuiet(d, PolicyOpen)
	raw, _, err := d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	verifyOwn(t, d, raw)
	assertPublishForm(t, decodeCard(t, raw), "")
	skills := cardSkills(t, raw)
	if !slices.Equal(skillIDs(skills), []string{module.ChatSkillID}) {
		t.Fatalf("skills %v, want only chat", skillIDs(skills))
	}
	chat := skills[0]
	desc, _ := chat["description"].(string)
	for _, want := range []string{"natural language", "allowed", "inbound policy is open", "hub only carries"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the chat skill's description lacks %q: %s", want, desc)
		}
	}
	if in, _ := chat["inputModes"].([]any); len(in) != 1 || in[0] != "text/plain" {
		t.Errorf("chat inputModes %v, want text/plain (the card's default is JSON)", chat["inputModes"])
	}

	// Beside a public capability, both, in id order; the payment module is
	// told of both, so a card whose capabilities are all priced does not
	// call a2a-x402 required while chat is free.
	if err := d.Providers().Register(context.Background(), &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d, lampCap)
	raw, _, err = d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	verifyOwn(t, d, raw)
	want := []string{module.ChatSkillID, lampCap}
	slices.Sort(want)
	if got := skillIDs(cardSkills(t, raw)); !slices.Equal(got, want) {
		t.Fatalf("skills %v, want %v", got, want)
	}
	setPolicyQuiet(d, PolicyClosed)
	raw, _, err = d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := skillIDs(cardSkills(t, raw)); !slices.Equal(got, []string{lampCap}) {
		t.Fatalf("closed: skills %v, want only %s", got, lampCap)
	}
}

// A capability named chat is not shadowed: public, it is listed as itself;
// private, an open node lists no chat skill that would name it.
func TestAChatCapabilityIsNotShadowed(t *testing.T) {
	h := newFakeHub(t)
	d := newCardDaemon(t, "Talker", "")
	if err := d.Providers().Register(context.Background(), &namedCapProvider{id: module.ChatSkillID}); err != nil {
		t.Fatal(err)
	}
	setPolicyQuiet(d, PolicyOpen)
	if _, _, err := d.networkCard(h.URL); err != errNoPublicSkill {
		t.Fatalf("a private chat capability on an open node: %v, want no card", err)
	}
	setPublic(d, module.ChatSkillID)
	raw, _, err := d.networkCard(h.URL)
	if err != nil {
		t.Fatal(err)
	}
	skills := cardSkills(t, raw)
	if len(skills) != 1 || skills[0]["id"] != module.ChatSkillID || strings.Contains(skills[0]["description"].(string), "inbound policy") {
		t.Fatalf("skills %v, want the capability itself", skills)
	}
}

// Changing the policy republishes (0017 Q27, Q6): opening publishes the
// card with the chat skill; closing again, with nothing else on it,
// withdraws the card; closing a node that also has a public capability
// keeps the card, without chat.
func TestThePolicyDecidesWhetherTheCardIsPublished(t *testing.T) {
	h := newFakeHub(t)
	fh := fakeHubAt(t, h.URL)
	ctx := context.Background()
	d := newCardDaemon(t, "Talker", "")
	if err := d.HubRegister(ctx, h.URL, "Talker", nil, ""); err != nil {
		t.Fatal(err)
	}
	held := func() ([]byte, string, int) {
		fh.mu.Lock()
		defer fh.mu.Unlock()
		return fh.a2aCards[d.AID()], fh.registerCards[d.AID()], fh.a2aWithdrawals
	}
	if card, status, _ := held(); card != nil || status != hubapi.CardStatusAbsent {
		t.Fatalf("closed: card %s, card_status %q", card, status)
	}

	if err := d.SetInboundPolicy(PolicyOpen); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the card with the chat skill", func() bool {
		card, status, _ := held()
		return status == hubapi.CardStatusOK && card != nil &&
			slices.Equal(skillIDs(cardSkills(t, card)), []string{module.ChatSkillID})
	})

	if err := d.SetInboundPolicy(PolicyClosed); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the card withdrawn", func() bool {
		card, status, n := held()
		return status == hubapi.CardStatusWithdrawn && card == nil && n == 1
	})

	if err := d.Providers().Register(ctx, &lampProvider{}); err != nil {
		t.Fatal(err)
	}
	setPublic(d, lampCap)
	if err := d.SetInboundPolicy(PolicyOpen); err != nil {
		t.Fatal(err)
	}
	both := []string{module.ChatSkillID, lampCap}
	slices.Sort(both)
	waitUntil(t, "the card with chat and the capability", func() bool {
		card, status, _ := held()
		return status == hubapi.CardStatusOK && card != nil && slices.Equal(skillIDs(cardSkills(t, card)), both)
	})
	if err := d.SetInboundPolicy(PolicyApprove); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the card without chat", func() bool {
		card, status, _ := held()
		return status == hubapi.CardStatusOK && card != nil && slices.Equal(skillIDs(cardSkills(t, card)), []string{lampCap})
	})
	if _, _, n := held(); n != 1 {
		t.Fatalf("%d withdrawals, want the one", n)
	}
}

// namedCapProvider serves one capability, id.
type namedCapProvider struct{ id string }

func (p *namedCapProvider) ID() string { return "named-" + p.id }
func (p *namedCapProvider) Capabilities(context.Context) ([]string, error) {
	return []string{p.id}, nil
}
func (p *namedCapProvider) Describe(context.Context) (string, error) { return "", nil }
func (p *namedCapProvider) Health(context.Context) error             { return nil }
func (p *namedCapProvider) Invoke(context.Context, provider.Call) (effect.Effect, error) {
	return effect.Effect{Status: effect.OK}, nil
}
