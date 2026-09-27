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
// a hub serving an earlier, validly signed card is refused. The mark is
// kept in peer_identity for a peer this node has a row for, so it survives
// a restart, and in memory for anyone else (card_highwater.go). Free-text
// search never leaves this node: the hub is asked by skill and tag only,
// and the text is matched locally against the cards it returned.
//
// A card that does not verify here is not passed on (0017 Q24): its entry
// carries the AID, UNVERIFIED with the a2acard code of the refusal, and the
// official mark, which is decided by the AID and not by anything the hub
// served — and nothing else, neither the card's words nor the hub's. A hub
// that forged a card must not have its text shown, least of all beside the
// official mark. An agent with no card at all is listed only on request
// (AgentQuery.IncludeUncarded, 0017 Q27): after the registry's last page,
// the hub directory's entries the registry does not list, as NONE and with
// the hub's statements about them, which is all there is.

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
	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/module"
)

// Verification values of a RemoteAgent.
const (
	cardVerified   = module.CardVerified
	cardUnverified = module.CardUnverified
	cardNone       = module.CardNone
)

// Discovery page sizes.
const (
	agentsPageDefault = 20
	agentsPageMax     = 100
)

// uncardedCursor begins the cursor of a page of agents without a card
// (AgentQuery.IncludeUncarded): the registry's pages come first, then the
// uncarded entries from the offset after the prefix.
const uncardedCursor = "uncarded:"

// registryWalkPages bounds the registry pages read to learn which agents
// have a card, for the uncarded entries that follow a page reached by
// cursor: agentsPageMax each, so a few thousand agents with cards.
const registryWalkPages = 50

// uncardedNote is what a NONE entry says of itself.
const uncardedNote = "the agent publishes no network card; its name, caps and summary are the hub's statement"

// listAgents asks the hub registry for agents by skill and tag, verifies
// each card here, and applies the free-text query locally. With
// q.IncludeUncarded the agents without a card follow the registry's last
// page (uncardedAgents).
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
	hctx, cancel := context.WithTimeout(ctx, hubCallTimeout)
	defer cancel()
	if rest, ok := strings.CutPrefix(q.Cursor, uncardedCursor); ok {
		// Past the registry: a page of the agents without a card.
		off, err := strconv.Atoi(rest)
		if err != nil || off < 0 || !q.IncludeUncarded {
			return nil, "", a2ashape.Errorf(a2ashape.ErrInvalidParams,
				"cursor %q continues a list asked with include_uncarded; ask again with it", q.Cursor)
		}
		carded, err := d.cardedAIDs(hctx, hub, q)
		if err != nil {
			return nil, "", err
		}
		return d.uncardedAgents(hctx, hub, q, carded, off, limit)
	}
	var page hubapi.A2AAgentList
	err := d.hubGet(hctx, hub, hubapi.RegistryAgentsPath, registryQuery(q, q.Cursor, limit), &page)
	if hubStatus(err) == http.StatusNotFound {
		// A hub without the A2A registry: none of its agents has a card
		// there, so its directory is the uncarded list, given on request.
		if !q.IncludeUncarded {
			return []module.RemoteAgent{}, "", nil
		}
		return d.uncardedAgents(hctx, hub, q, nil, 0, limit)
	}
	if err != nil {
		return nil, "", a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
	}
	resolve := d.cardKELResolver(hctx)
	out := make([]module.RemoteAgent, 0, len(page.Agents))
	for _, e := range page.Agents {
		ra := module.RemoteAgent{AID: e.AID, Card: e.Card}
		d.verifyCardInto(&ra, resolve)
		if ra.Verification == cardVerified {
			ra.HubVerification, ra.HomeHub, ra.LastSeen, ra.Quiet = e.CardVerification, e.HomeHub, e.LastSeen, e.Quiet
			ra.ReviewCount, ra.AvgRating = e.ReviewCount, e.AvgRating
		}
		// The text is matched against what is shown: an unverified card's
		// words are neither shown nor searched, only its AID.
		if q.Query != "" && !cardMatches(ra.Card, e.AID, q.Query) {
			continue
		}
		d.markOfficial(&ra)
		out = append(out, ra)
	}
	if page.NextCursor != "" || !q.IncludeUncarded {
		return out, page.NextCursor, nil
	}
	// The registry's last page: the agents without a card follow it.
	carded := map[string]bool{}
	for _, e := range page.Agents {
		carded[e.AID] = true
	}
	if q.Cursor != "" {
		// Earlier pages listed agents with cards too; the directory lists
		// them all, and none of them is uncarded.
		if carded, err = d.cardedAIDs(hctx, hub, q); err != nil {
			return nil, "", err
		}
	}
	tail, next, err := d.uncardedAgents(hctx, hub, q, carded, 0, limit-len(out))
	if err != nil {
		return nil, "", err
	}
	return append(out, tail...), next, nil
}

// registryQuery is the registry request for q: skill and tag, never the
// free text.
func registryQuery(q module.AgentQuery, cursor string, limit int) url.Values {
	v := url.Values{}
	if q.Skill != "" {
		v.Set("skill", q.Skill)
	}
	if q.Tag != "" {
		v.Set("tag", q.Tag)
	}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	v.Set("limit", strconv.Itoa(limit))
	return v
}

// cardedAIDs is every agent the registry lists for q's skill and tag: the
// agents with a card, which the uncarded list leaves out. Empty for a hub
// without the registry.
func (d *Daemon) cardedAIDs(ctx context.Context, hub string, q module.AgentQuery) (map[string]bool, error) {
	out := map[string]bool{}
	cursor := ""
	for range registryWalkPages {
		var page hubapi.A2AAgentList
		err := d.hubGet(ctx, hub, hubapi.RegistryAgentsPath, registryQuery(q, cursor, agentsPageMax), &page)
		if hubStatus(err) == http.StatusNotFound {
			return out, nil
		}
		if err != nil {
			return nil, a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
		}
		for _, e := range page.Agents {
			out[e.AID] = true
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
	return nil, a2ashape.Errorf(a2ashape.ErrUnavailable,
		"more than %d agents with a card match; narrow the search with skill or tag to list the ones without",
		registryWalkPages*agentsPageMax)
}

// hubDirEntry is one entry of the hub's /agents directory: the hub's
// statement of what the agent registered.
type hubDirEntry struct {
	AID         string   `json:"aid"`
	Name        string   `json:"name"`
	Caps        []string `json:"caps"`
	Summary     string   `json:"summary"`
	AvgRating   float64  `json:"avg_rating"`
	ReviewCount int      `json:"review_count"`
	HomeHub     string   `json:"home_hub"`
	LastSeen    string   `json:"last_seen"`
	Quiet       bool     `json:"quiet"`
}

// uncardedAgents is a page of the agents registered at the hub that
// publish no card (0017 Q27): the /agents directory, asked by capability
// when q has a skill, less the agents the registry lists (carded), with the
// tag and the free text matched here against what the hub says. Each is
// NONE; up to limit from off, and the cursor of the rest.
func (d *Daemon) uncardedAgents(ctx context.Context, hub string, q module.AgentQuery, carded map[string]bool, off, limit int) ([]module.RemoteAgent, string, error) {
	var resp struct {
		Agents []hubDirEntry `json:"agents"`
	}
	v := url.Values{}
	if q.Skill != "" {
		v.Set("cap", q.Skill)
	}
	if err := d.hubGet(ctx, hub, "/agents", v, &resp); err != nil {
		return nil, "", a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
	}
	var matched []module.RemoteAgent
	seen := map[string]bool{}
	for _, a := range resp.Agents {
		if a.AID == "" || carded[a.AID] || seen[a.AID] {
			continue
		}
		seen[a.AID] = true
		if q.Tag != "" && !containsFold(strings.Join(a.Caps, " "), q.Tag) {
			continue
		}
		if q.Query != "" && !containsFold(strings.Join(append([]string{a.AID, a.Name, a.Summary}, a.Caps...), "\n"), q.Query) {
			continue
		}
		matched = append(matched, d.uncardedAgent(a))
	}
	off = min(off, len(matched))
	end := min(off+max(limit, 0), len(matched))
	next := ""
	if end < len(matched) {
		next = uncardedCursor + strconv.Itoa(end)
	}
	return matched[off:end], next, nil
}

// uncardedAgent is a directory entry as a NONE RemoteAgent: the hub's
// statements, said to be the hub's. An official AID gets none of them: the
// official agent publishes a card, and an entry without one carries only
// what a hub wrote, which is not shown beside the mark (0017 Q24).
func (d *Daemon) uncardedAgent(a hubDirEntry) module.RemoteAgent {
	if official := d.IsOfficial(a.AID); official {
		return module.RemoteAgent{AID: a.AID, Verification: cardNone, Official: official,
			VerificationError: "the agent publishes no network card"}
	}
	ra := module.RemoteAgent{AID: a.AID, Verification: cardNone, VerificationError: uncardedNote}
	ra.Name, ra.Caps, ra.Summary = a.Name, a.Caps, a.Summary
	ra.HomeHub, ra.LastSeen, ra.Quiet = a.HomeHub, a.LastSeen, a.Quiet
	ra.ReviewCount, ra.AvgRating = a.ReviewCount, a.AvgRating
	return ra
}

// agentCard fetches one agent's network card and verifies it here. An
// agent without a card is not an error: it is returned NONE with an empty
// card, and the caller describes it by its AID (§11.3); a card that does
// not verify is returned UNVERIFIED, without its bytes.
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
	err := d.hubGet(hctx, hub, hubapi.RegistryAgentsPath+"/"+url.PathEscape(aid)+"/card", nil, &card)
	switch {
	case hubStatus(err) == http.StatusNotFound:
		return module.RemoteAgent{AID: aid, Verification: cardNone, Official: d.IsOfficial(aid),
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
// served under this AID is refused. A card that does not verify is dropped
// from ra (0017 Q24): what is kept is the AID, UNVERIFIED and the reason,
// in words of this node's or the a2acard code, never an error's detail,
// which can quote the card or the hub.
func (d *Daemon) verifyCardInto(ra *module.RemoteAgent, resolve a2acard.Resolver) {
	ra.Verification = cardUnverified
	if len(ra.Card) == 0 {
		ra.VerificationError = "no card"
		return
	}
	v, err := a2acard.Verify(ra.Card, resolve, d.nowMS())
	switch {
	case err != nil:
		ra.VerificationError = verificationReason(err)
	case v.AID != ra.AID:
		// v.AID is the kid AID, of the character set a2acard admits;
		// ra.AID is the entry's own.
		ra.VerificationError = fmt.Sprintf("the card is signed by %s, not %s", v.AID, ra.AID)
	default:
		if err := d.admitCardMark(v); err != nil {
			ra.VerificationError = verificationReason(err)
			break
		}
		ra.Verification = cardVerified
		ra.Name = v.Name
		return
	}
	ra.Card, ra.Name = nil, ""
}

// verificationReason names why a card did not verify: the a2acard code
// (INVALID_SIGNATURE, SEQ_ROLLBACK, KEL_UNAVAILABLE …), whose detail is
// left out because it may quote the card (a member name, a skill id) or
// the hub (the answer to a key-history fetch).
func verificationReason(err error) string {
	var ce *a2acard.Error
	if errors.As(err, &ce) {
		return string(ce.Code)
	}
	return "the card could not be checked"
}

// cardMarkCap bounds the high-water marks one process keeps.
const cardMarkCap = 4096

// cardMarkCache is the params.seq high water of the cards this process
// has admitted, per (this node, card AID) — tests run several nodes in
// one process. It is what a stranger's card is checked against: a restart
// forgets it, and the first card seen after one is taken as the mark. Past
// cardMarkCap entries an arbitrary one is forgotten, which loses only that
// agent's protection. A peer this node has a peer_identity row for is
// checked against the persisted mark as well (card_highwater.go).
type cardMarkCache struct {
	mu sync.Mutex
	m  map[string]a2acard.Mark
	// forks counts, per node, the cards refused as a second card under an
	// admitted seq (CodeSeqFork): the signer said two things at once.
	forks map[string]uint64
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
