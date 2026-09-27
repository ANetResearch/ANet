package daemon

// taskseam_agents.go is discovery for the A2A surface (A2A-DESIGN §10.5,
// §11.1 Agents/Card): network cards from the hub registry, verified here.
// Each agent is marked official, or not, by its AID alone (official.go).
//
// The hub's statement that a card verified is recorded (hubVerification)
// but not relied on: every card is checked again with ANetCore a2acard
// against the signer's KEL, and the card must be signed by the AID it is
// listed under. A card must also not be older than one this process has
// already admitted for that agent (the params.seq high-water rule, §10.3):
// a hub serving an earlier, validly signed card is refused. Free-text
// search never leaves this node: the hub is asked by skill and tag only,
// and the text is matched locally against the cards it returned.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/ANetResearch/ANetCore/a2acard"
	"github.com/ANetResearch/ANetCore/identity"
	"github.com/ANetResearch/ANetCore/seal"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// Verification values of a RemoteAgent.
const (
	cardVerified   = "VERIFIED"
	cardUnverified = "UNVERIFIED"
)

// Discovery page sizes.
const (
	agentsPageDefault = 20
	agentsPageMax     = 100
)

// registryEntry is one entry of GET /a2a/v1/agents (A2A-DESIGN §10.5). The
// wrapper fields are the hub's statements.
type registryEntry struct {
	AID              string          `json:"aid"`
	Card             json.RawMessage `json:"card"`
	CardVerification string          `json:"cardVerification"`
	VerifiedAt       string          `json:"verifiedAt"`
	HomeHub          string          `json:"homeHub"`
	LastSeen         string          `json:"lastSeen"`
	Quiet            bool            `json:"quiet"`
	ReviewCount      int             `json:"reviewCount"`
	AvgRating        any             `json:"avgRating"`
}

type registryPage struct {
	Agents     []registryEntry `json:"agents"`
	NextCursor string          `json:"nextCursor"`
}

// listAgents asks the hub registry for agents by skill and tag, verifies
// each card here, and applies the free-text query locally.
func (d *Daemon) listAgents(ctx context.Context, q module.AgentQuery) ([]module.RemoteAgent, string, error) {
	hub := d.config().HubURL
	if hub == "" {
		return nil, "", a2ashape.Errorf(a2ashape.ErrUnavailable, "this node has no hub (run `anet hub-register` first)")
	}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = agentsPageDefault
	case limit < 0 || limit > agentsPageMax:
		return nil, "", a2ashape.Errorf(a2ashape.ErrInvalidParams, "limit must be between 1 and %d", agentsPageMax)
	}
	v := url.Values{}
	if q.Skill != "" {
		v.Set("skill", q.Skill)
	}
	if q.Tag != "" {
		v.Set("tag", q.Tag)
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	v.Set("limit", strconv.Itoa(limit))
	hctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	var page registryPage
	err := d.hubGet(hctx, hub, "/a2a/v1/agents", v, &page)
	if hubStatus(err) == http.StatusNotFound {
		// A hub without the A2A registry: fall back to its agent directory,
		// which lists agents without network cards.
		return d.listAgentsLegacy(hctx, hub, q, limit)
	}
	if err != nil {
		return nil, "", a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
	}
	resolve := d.cardKELResolver(hctx)
	out := make([]module.RemoteAgent, 0, len(page.Agents))
	for _, e := range page.Agents {
		ra := module.RemoteAgent{AID: e.AID, Card: e.Card, HubVerification: e.CardVerification,
			HomeHub: e.HomeHub, LastSeen: e.LastSeen, Quiet: e.Quiet, ReviewCount: e.ReviewCount, AvgRating: e.AvgRating}
		d.verifyCardInto(&ra, resolve)
		if q.Query != "" && !cardMatches(e.Card, e.AID, q.Query) {
			continue
		}
		d.markOfficial(&ra)
		out = append(out, ra)
	}
	return out, page.NextCursor, nil
}

// listAgentsLegacy lists agents from a hub's /agents directory, which has
// no network cards: each entry is UNVERIFIED. The free-text query is still
// matched here, not sent.
func (d *Daemon) listAgentsLegacy(ctx context.Context, hub string, q module.AgentQuery, limit int) ([]module.RemoteAgent, string, error) {
	var resp struct {
		Agents []struct {
			AID         string   `json:"aid"`
			Name        string   `json:"name"`
			Caps        []string `json:"caps"`
			Summary     string   `json:"summary"`
			AvgRating   float64  `json:"avg_rating"`
			ReviewCount int      `json:"review_count"`
			HomeHub     string   `json:"home_hub"`
		} `json:"agents"`
	}
	v := url.Values{}
	if q.Skill != "" {
		v.Set("cap", q.Skill)
	}
	if err := d.hubGet(ctx, hub, "/agents", v, &resp); err != nil {
		return nil, "", a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
	}
	out := []module.RemoteAgent{}
	for _, a := range resp.Agents {
		if q.Tag != "" && !containsFold(strings.Join(a.Caps, " "), q.Tag) {
			continue
		}
		if q.Query != "" && !containsFold(strings.Join(append([]string{a.AID, a.Name, a.Summary}, a.Caps...), "\n"), q.Query) {
			continue
		}
		out = append(out, module.RemoteAgent{AID: a.AID, Name: a.Name, HomeHub: a.HomeHub,
			ReviewCount: a.ReviewCount, AvgRating: a.AvgRating, Official: d.IsOfficial(a.AID),
			Verification: cardUnverified, VerificationError: "the hub publishes no network card for this agent"})
		if len(out) == limit {
			break
		}
	}
	return out, "", nil
}

// agentCard fetches one agent's network card and verifies it here. An
// agent without a card is not an error: it is returned UNVERIFIED with an
// empty card, and the caller describes it by its AID (§11.3).
func (d *Daemon) agentCard(ctx context.Context, aid string) (module.RemoteAgent, error) {
	if !validAgentID(aid) {
		return module.RemoteAgent{}, a2ashape.Errorf(a2ashape.ErrInvalidParams, "%q is not an agent id", aid)
	}
	hub := d.config().HubURL
	if hub == "" {
		return module.RemoteAgent{}, a2ashape.Errorf(a2ashape.ErrUnavailable, "this node has no hub (run `anet hub-register` first)")
	}
	hctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	var card json.RawMessage
	err := d.hubGet(hctx, hub, "/a2a/v1/agents/"+url.PathEscape(aid)+"/card", nil, &card)
	switch {
	case hubStatus(err) == http.StatusNotFound:
		return module.RemoteAgent{AID: aid, Verification: cardUnverified, Official: d.IsOfficial(aid),
			VerificationError: "the agent publishes no network card"}, nil
	case err != nil:
		return module.RemoteAgent{}, a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
	}
	ra := module.RemoteAgent{AID: aid, Card: card}
	d.verifyCardInto(&ra, d.cardKELResolver(hctx))
	d.markOfficial(&ra)
	return ra, nil
}

// validAgentID reports whether aid has the form of an agent id: the
// character set and length a2acard accepts in a kid, which is what a URL
// path segment may safely carry. Whether the agent exists is the hub's to
// say.
func validAgentID(aid string) bool {
	_, _, err := a2acard.ParseKID(a2acard.KID(aid, 0))
	return err == nil
}

// verifyCardInto checks ra.Card and records the outcome on ra. The card
// must verify and must be signed by ra.AID: a valid card of another agent
// served under this AID is refused.
func (d *Daemon) verifyCardInto(ra *module.RemoteAgent, resolve a2acard.Resolver) {
	ra.Verification = cardUnverified
	if len(ra.Card) == 0 {
		ra.VerificationError = "no card"
		return
	}
	v, err := a2acard.Verify(ra.Card, resolve, d.nowMS())
	switch {
	case err != nil:
		ra.VerificationError = err.Error()
	case v.AID != ra.AID:
		ra.VerificationError = fmt.Sprintf("the card is signed by %s, not %s", v.AID, ra.AID)
	default:
		if err := cardMarks.admit(d.AID(), v); err != nil {
			ra.VerificationError = err.Error()
			return
		}
		ra.Verification = cardVerified
		ra.Name = v.Name
	}
}

// cardMarkCap bounds the high-water marks one process keeps.
const cardMarkCap = 4096

// cardMarkCache is the params.seq high water of the cards this process
// has admitted, per (this node, card AID) — tests run several nodes in
// one process. It lives in memory: a restart forgets it, and the first
// card seen after one is taken as the mark. Past cardMarkCap entries an
// arbitrary one is forgotten, which loses only that agent's protection.
type cardMarkCache struct {
	mu sync.Mutex
	m  map[string]a2acard.Mark
}

var cardMarks cardMarkCache

// admit applies a2acard.CheckHighWater to a verified card and records its
// mark when it advances. A rolled-back or forked card is an error.
func (c *cardMarkCache) admit(self string, v *a2acard.Verified) error {
	key := self + "\x00" + v.AID
	c.mu.Lock()
	defer c.mu.Unlock()
	var stored *a2acard.Mark
	if m, ok := c.m[key]; ok {
		stored = &m
	}
	dec, err := a2acard.CheckHighWater(stored, v.Mark())
	if err != nil {
		return err
	}
	if dec == a2acard.Advance {
		if c.m == nil {
			c.m = map[string]a2acard.Mark{}
		}
		if _, ok := c.m[key]; !ok && len(c.m) >= cardMarkCap {
			for k := range c.m {
				delete(c.m, k)
				break
			}
		}
		c.m[key] = v.Mark()
	}
	return nil
}

// cardKELResolver resolves a card signer's KEL: the one this node holds
// for the peer, else the hub's copy, which must replay to the AID and not
// fork from anything stored. Nothing fetched here is stored: reading a
// directory is not contact with the agent.
func (d *Daemon) cardKELResolver(ctx context.Context) a2acard.Resolver {
	return func(aid string) ([]identity.SignedEvent, error) {
		stored, haveStored := d.peerKEL(aid)
		if haveStored {
			return stored, nil
		}
		hub := d.config().HubURL
		if hub == "" {
			return nil, errors.New("no hub to fetch the key history from")
		}
		var resp struct {
			KEL string `json:"kel"`
		}
		if err := d.hubGet(ctx, hub, "/agents/"+url.PathEscape(aid)+"/kel", nil, &resp); err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(resp.KEL)
		if err != nil {
			return nil, fmt.Errorf("undecodable key history: %w", err)
		}
		kel, err := seal.ParseKEL(raw)
		if err != nil {
			return nil, err
		}
		if got, err := replayedAID(kel); err != nil || got != aid {
			return nil, fmt.Errorf("the hub's key history for %s does not replay to it", aid)
		}
		return kel, nil
	}
}

// cardMatches reports whether the free-text query occurs (case-folded) in
// the card's AID, name, description or skills.
func cardMatches(card json.RawMessage, aid, query string) bool {
	var c struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Skills      []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
			Examples    []string `json:"examples"`
		} `json:"skills"`
	}
	_ = json.Unmarshal(card, &c)
	fields := []string{aid, c.Name, c.Description}
	for _, s := range c.Skills {
		fields = append(fields, s.ID, s.Name, s.Description)
		fields = append(fields, s.Tags...)
		fields = append(fields, s.Examples...)
	}
	return containsFold(strings.Join(fields, "\n"), query)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(strings.TrimSpace(sub)))
}
