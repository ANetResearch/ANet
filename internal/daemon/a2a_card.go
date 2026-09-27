package daemon

// a2a_card.go signs, keeps and publishes this node's A2A network card
// (A2A-DESIGN §10.1–§10.4): the AgentCard a hub verifies at /register and
// lists in its directory. internal/netcard assembles it; this file decides
// what goes in, signs it and tracks what the hub said.
//
// The card is signed with the key this node's KEL currently designates
// (ANetCore a2acard). It is published only by a node with at least one
// public skill: a fresh install serves nobody, and A2A requires skills to
// be non-empty. The kernel writes the relay interface at the node's hub
// and the anet-card and anet-evidence extensions; modules add their own
// interfaces (p2p) and extensions (x402, pricing) through
// module.CardContributor. The kernel never imports an A2A library.
//
// The card is in publish form (A2A specification §8.4.1): REQUIRED members
// present, default values left out, numbers as strings. A verifier that
// rebuilds the payload from the protobuf schema drops default values
// before canonicalising, so a card carrying "required": false or an empty
// tenant would verify under a2a-go and fail under a2a-python.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/relayauth"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/netcard"
	"github.com/ANetResearch/ANet/module"
)

// cardNotBeforeSkew backdates notBefore, so a verifier whose clock is a
// little behind this node's still admits a card issued just now.
const cardNotBeforeSkew = time.Minute

// networkCardFile is where the last issued card is kept, so a restart
// that changes nothing republishes the same bytes (the hub answers
// "unchanged") instead of minting a new seq.
const networkCardFile = "a2a_card.json"

// errNoPublicSkill is why no card is built: A2A requires at least one
// skill, and this node lists no public capability it serves.
var errNoPublicSkill = errors.New("anet: no public skill, so no network card is published")

// netCardState is this node's issued card and what the hub last said
// about it.
type netCardState struct {
	mu      sync.Mutex
	loaded  bool
	issued  *issuedCard
	lastPub CardPublication
}

// issuedCard is the last card this node signed. Digest covers the card
// with its anet-card seq and times zeroed, plus the kid and jku, so two
// builds that say the same thing under the same key compare equal.
type issuedCard struct {
	Digest string          `json:"digest"`
	Seq    uint64          `json:"seq"`
	KID    string          `json:"kid"`
	Card   json.RawMessage `json:"card"`
}

// CardPublication is what the hub last answered for this node's network
// card at /register (card_status and card_error), for status output.
type CardPublication struct {
	Hub string `json:"hub,omitempty"`
	// Sent is whether the registration carried a card. False means this
	// node has no public skill, or the card could not be built (Error).
	Sent bool   `json:"sent"`
	Seq  uint64 `json:"seq,omitempty"`
	// Status is the hub's card_status: ok, unchanged, unverified, invalid,
	// conflict or absent. Empty until a registration was answered.
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
	At     string `json:"at,omitempty"`
}

// publicSkillIDs is what the card publishes as skills: the entries of
// inbound.public_capabilities that a provider on this node serves, sorted
// and de-duplicated. The ids are the exact strings the inbound check
// admits. A public capability nobody serves is left out: a directory entry
// for it would be an invitation to call something that cannot answer.
func (d *Daemon) publicSkillIDs() []string {
	if d.providers == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range d.config().inbound().PublicCapabilities {
		id := p.ID
		if strings.TrimSpace(id) == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := d.providers.Resolve(id); ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	if len(out) > a2acard.MaxSkills {
		log.Printf("anet: %d public capabilities; the network card lists the first %d", len(out), a2acard.MaxSkills)
		out = out[:a2acard.MaxSkills]
	}
	return out
}

// networkCard returns this node's signed network card for hubURL, reusing
// the last issued card when nothing it says has changed. It returns
// errNoPublicSkill when there is nothing to publish.
func (d *Daemon) networkCard(hubURL string) (json.RawMessage, uint64, error) {
	d.netCard.mu.Lock()
	defer d.netCard.mu.Unlock()
	return d.networkCardLocked(hubURL, false)
}

// networkCardLocked is networkCard with netCard.mu held. fresh forces a
// new seq even when the content is unchanged, which is how a hub's
// conflict answer is resolved.
func (d *Daemon) networkCardLocked(hubURL string, fresh bool) (json.RawMessage, uint64, error) {
	hubURL = strings.TrimRight(strings.TrimSpace(hubURL), "/")
	if hubURL == "" {
		return nil, 0, fmt.Errorf("anet: no hub, so no relay interface for a network card")
	}
	in, err := d.cardInput(hubURL)
	if err != nil {
		return nil, 0, err
	}
	kid := a2acard.KID(d.AID(), d.self.CurrentSeq())
	jku := netcard.JKU(hubURL, d.AID())

	template, err := netcard.Build(in) // seq and times zero
	if err != nil {
		return nil, 0, err
	}
	payload, err := a2acard.SigningPayload(template)
	if err != nil {
		return nil, 0, fmt.Errorf("anet: network card: %w", err)
	}
	h := sha256.New()
	h.Write(payload)
	h.Write([]byte{0})
	h.Write([]byte(kid))
	h.Write([]byte{0})
	h.Write([]byte(jku))
	digest := hex.EncodeToString(h.Sum(nil))

	d.loadIssuedCardLocked()
	if prev := d.netCard.issued; !fresh && prev != nil && prev.Digest == digest && prev.KID == kid {
		// Same statement under the same key: the same bytes. A hub
		// answers "unchanged", and the seq does not creep upward on
		// every restart.
		if _, err := a2acard.Verify(prev.Card, d.selfKELResolver, uint64(time.Now().UnixMilli())); err == nil {
			return prev.Card, prev.Seq, nil
		}
	}

	now := time.Now()
	in.Seq = d.cardSeq()
	in.IssuedAtMs = uint64(now.UnixMilli())
	in.NotBeforeMs = uint64(now.Add(-cardNotBeforeSkew).UnixMilli())
	unsigned, err := netcard.Build(in)
	if err != nil {
		return nil, 0, err
	}
	signed, err := a2acard.SignWithController(unsigned, d.self, jku)
	if err != nil {
		return nil, 0, fmt.Errorf("anet: sign network card: %w", err)
	}
	// The hub runs the same check. Running it here turns a card this node
	// built wrongly into an error at the source, with the reason, rather
	// than a card_status of "invalid" read back from a hub.
	if _, err := a2acard.Verify(signed, d.selfKELResolver, uint64(now.UnixMilli())); err != nil {
		return nil, 0, fmt.Errorf("anet: the network card this node built does not verify: %w", err)
	}
	d.netCard.issued = &issuedCard{Digest: digest, Seq: in.Seq, KID: kid, Card: signed}
	d.persistIssuedCardLocked()
	return signed, in.Seq, nil
}

// selfKELResolver answers a2acard.Verify for this node's own AID.
func (d *Daemon) selfKELResolver(aid string) ([]identity.SignedEvent, error) {
	if aid != d.AID() {
		return nil, fmt.Errorf("not this node's AID")
	}
	return d.self.KEL(), nil
}

// cardInput gathers what the card says, with seq and times left zero:
// name and description from the config (the profile summary), one skill
// per public capability, and the modules' contributions, each put in
// publish form or dropped with a log line.
func (d *Daemon) cardInput(hubURL string) (netcard.Input, error) {
	skills := d.publicSkillIDs()
	if len(skills) == 0 {
		return netcard.Input{}, errNoPublicSkill
	}
	cfg := d.config()
	in := netcard.Input{AID: d.AID(), Name: cfg.Name, Description: cfg.Summary, Version: Version, HubURL: hubURL}
	for _, id := range skills {
		p, _ := d.providers.Resolve(id)
		in.Skills = append(in.Skills, netcard.SkillFor(p, id))
	}
	cc := module.CardContext{AID: d.AID(), HubURL: hubURL, Skills: append([]string(nil), skills...)}
	for _, m := range d.cardContributors() {
		for _, e := range m.CardExtensions(cc) {
			ext, err := netcard.Extension(e)
			if err != nil {
				log.Printf("anet: module %s: card extension dropped: %v", moduleName(m), err)
				continue
			}
			in.Extensions = append(in.Extensions, ext)
		}
		for _, it := range m.CardInterfaces(cc) {
			iface, err := netcard.Interface(it, d.AID())
			if err != nil {
				log.Printf("anet: module %s: card interface dropped: %v", moduleName(m), err)
				continue
			}
			in.Interfaces = append(in.Interfaces, iface)
		}
	}
	return in, nil
}

// cardContributors are the running modules that add to the card.
func (d *Daemon) cardContributors() []module.CardContributor {
	var out []module.CardContributor
	for _, m := range d.modules {
		if c, ok := m.(module.CardContributor); ok {
			out = append(out, c)
		}
	}
	return out
}

func moduleName(c module.CardContributor) string {
	if m, ok := c.(module.Module); ok {
		return m.Name()
	}
	return fmt.Sprintf("%T", c)
}

func (d *Daemon) networkCardPath() string { return filepath.Join(d.layout.Root, networkCardFile) }

// loadIssuedCardLocked reads the last issued card once per process.
func (d *Daemon) loadIssuedCardLocked() {
	if d.netCard.loaded {
		return
	}
	d.netCard.loaded = true
	b, err := os.ReadFile(d.networkCardPath())
	if err != nil {
		return
	}
	var ic issuedCard
	if json.Unmarshal(b, &ic) == nil && len(ic.Card) > 0 {
		d.netCard.issued = &ic
	}
}

func (d *Daemon) persistIssuedCardLocked() {
	b, err := json.Marshal(d.netCard.issued)
	if err != nil {
		return
	}
	if err := writeFileAtomic(d.networkCardPath(), b, 0o600); err != nil {
		// Not fatal: without the file the next start mints a new seq for
		// the same content, which the hub admits as an advance.
		log.Printf("anet: could not keep the issued network card: %v", err)
	}
}

// cardForRegistration is the a2a_card of a registration: the card, or nil
// when this node publishes none. A card that cannot be built is logged
// and left out; it does not stop the registration.
func (d *Daemon) cardForRegistration(hubURL string, fresh bool) (json.RawMessage, uint64) {
	d.netCard.mu.Lock()
	defer d.netCard.mu.Unlock()
	card, seq, err := d.networkCardLocked(hubURL, fresh)
	switch {
	case errors.Is(err, errNoPublicSkill):
		d.netCard.lastPub = CardPublication{Hub: hubURL, At: time.Now().UTC().Format(time.RFC3339)}
		return nil, 0
	case err != nil:
		log.Printf("anet: registering without an A2A network card: %v", err)
		d.netCard.lastPub = CardPublication{Hub: hubURL, Error: err.Error(), At: time.Now().UTC().Format(time.RFC3339)}
		return nil, 0
	}
	return card, seq
}

// noteCardAnswer records the hub's card_status for a registration that
// carried a card, and says whether the card must be re-issued under a new
// seq and sent again.
//
// ok and unchanged mean the hub holds this card. unverified is a hub that
// stores cards before its admission step exists. conflict means the hub
// holds a higher seq, or this seq with other content (a data directory
// restored from a backup, for example): a fresh seq resolves it. invalid
// is this node's card failing the hub's checks; the same card would fail
// again, so it is logged, not retried. absent after sending a card is a
// hub that ignored the field.
func (d *Daemon) noteCardAnswer(hubURL string, sentSeq uint64, out hubapi.RegisterResponse) (reissue bool) {
	d.netCard.mu.Lock()
	d.netCard.lastPub = CardPublication{Hub: hubURL, Sent: true, Seq: sentSeq, Status: out.CardStatus,
		Error: out.CardError, At: time.Now().UTC().Format(time.RFC3339)}
	d.netCard.mu.Unlock()
	switch out.CardStatus {
	case hubapi.CardStatusOK, hubapi.CardStatusUnchanged, hubapi.CardStatusUnverified:
		return false
	case hubapi.CardStatusConflict:
		log.Printf("anet: %s holds another network card for this node at or above seq %d (%s); re-issuing under a new seq",
			hubURL, sentSeq, out.CardError)
		return true
	case hubapi.CardStatusAbsent, "":
		log.Printf("anet: %s did not take this node's network card (card_status %q); it does not admit A2A cards",
			hubURL, out.CardStatus)
	default:
		log.Printf("anet: %s refused this node's network card seq %d (card_status %q: %s)",
			hubURL, sentSeq, out.CardStatus, out.CardError)
	}
	return false
}

// CardPublicationStatus reports what the hub last said about this node's
// network card.
func (d *Daemon) CardPublicationStatus() CardPublication {
	d.netCard.mu.Lock()
	defer d.netCard.mu.Unlock()
	return d.netCard.lastPub
}

// NetworkCard returns the card this node would publish to its hub now, or
// errNoPublicSkill.
func (d *Daemon) NetworkCard() (json.RawMessage, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, fmt.Errorf("anet: no hub configured, so no network card")
	}
	card, _, err := d.networkCard(hub)
	return card, err
}

// cardInputsChanged republishes the registration, and with it the network
// card, after a change to something the card says: the public
// capabilities or the summary. In the background, like the startup
// refresh; a node without a hub has nothing to publish.
func (d *Daemon) cardInputsChanged() {
	if d.config().HubURL == "" {
		return
	}
	d.refreshRegistration()
}

// afterCardAnswer handles the card half of a /register answer. On
// conflict it re-issues the card under a new seq and registers once more
// with it; the rest of the registration is idempotent at the hub.
func (d *Daemon) afterCardAnswer(ctx context.Context, hubURL string, body hubapi.RegisterRequest, seq uint64, out hubapi.RegisterResponse) {
	if len(body.A2ACard) == 0 {
		return // cardForRegistration recorded why
	}
	if !d.noteCardAnswer(hubURL, seq, out) {
		return
	}
	card, seq := d.cardForRegistration(hubURL, true)
	if len(card) == 0 {
		return
	}
	body.A2ACard = card
	// The ADP card of the first attempt was admitted at its seq, so the
	// second attempt carries a newly minted one (card.go).
	if body.Card != nil {
		if adpCard, err := d.signedCard(body.Name, body.Caps); err == nil {
			body.Card = adpCard
		}
	}
	if err := d.screenPublication("this node's registration", body); err != nil {
		log.Printf("anet: re-issued network card: %v", err)
		return
	}
	var again hubapi.RegisterResponse
	if err := d.hubSigned(ctx, hubURL, http.MethodPost, "/register", relayauth.ActionRegister, body, &again); err != nil {
		log.Printf("anet: registering the re-issued network card with %s: %v", hubURL, err)
		return
	}
	if d.noteCardAnswer(hubURL, seq, again) {
		log.Printf("anet: %s still reports a conflict for the network card at seq %d; leaving it", hubURL, seq)
	}
}

// hCard answers POST /card: the network card this node publishes to its
// hub (null when it has no public skill, with the reason) and what the hub
// last said about it.
func (d *Daemon) hCard(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"publication": d.CardPublicationStatus(), "card": nil}
	card, err := d.NetworkCard()
	switch {
	case err == nil:
		out["card"] = card
	case errors.Is(err, errNoPublicSkill):
		out["reason"] = "no public skill: list a served capability under inbound.public_capabilities to publish a card"
	default:
		out["reason"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}
