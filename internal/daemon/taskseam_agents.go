package daemon

// taskseam_agents.go is discovery for the A2A surface (A2A-DESIGN §10.5,
// §11.1 Agents/Card): network cards from the hub registry, verified here.
// Each agent is marked official, or not, by its AID alone (official.go).
//
// The hub's statement that a card verified is recorded (hubVerification)
// but not relied on: every card is checked again with ANetCore a2acard
// against the signer's KEL — the one the entry carries, verified here, or
// this node's record of it — and the card must be signed by the AID it is
// listed under. A card must also not be older than one this process has
// already admitted for that agent (the params.seq high-water rule, §10.3):
// a hub serving an earlier, validly signed card is refused. The mark is
// kept in peer_identity for a peer this node has a row for, so it survives
// a restart, and in memory for anyone else (card_highwater.go). Nothing
// looked for leaves this node: the registry is read a page at a time, with
// no query, and skill, tag and free text are matched here (sealed-sender
// SSR-D16). One agent's card comes from the directory without naming the
// agent (peer_resolve.go).
//
// A card that does not verify here is not passed on (an internal note item): its entry
// carries the AID, UNVERIFIED with the a2acard code of the refusal, and the
// official mark, which is decided by the AID and not by anything the hub
// served — and nothing else, neither the card's words nor the hub's. A hub
// that forged a card must not have its text shown, least of all beside the
// official mark. An agent with no card at all is listed only on request
// (AgentQuery.IncludeUncarded, an internal note item): after the registry's last page,
// the hub directory's entries the registry does not list, as NONE and with
// the hub's statements about them, which is all there is.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
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
// cursor or asked with a tag: agentsPageMax each, so a few thousand agents
// with cards.
const registryWalkPages = 50

// CardUnknownAgent is the verificationError of an agent no hub's directory
// holds (agentCard): NONE, and not because it publishes no card.
const CardUnknownAgent = "no hub knows this agent"

// uncardedNote is what a NONE entry says of itself.
const uncardedNote = "the agent publishes no network card; all here but the aid is the hub's statement, not signed by the agent"

// registryCursor begins the cursor of a listing that stopped inside a page
// of the registry: the offset into that page, a colon, and the registry's
// own cursor of the page ("" for the first).
const registryCursor = "registry:"

// listAgents lists the hub registry's agents a page at a time, verifies
// each card here, and matches skill, tag and free text here: the hub is
// asked for its pages with no query of any kind (sealed-sender SSR-D16;
// what an operator looks for says what they are about to do). An agent
// whose card claims the skill or tag is listed even when its card does not
// verify here — UNVERIFIED, its AID alone (an internal note item) — and the free text is
// matched against what is shown. Every agent whose card verifies is kept as
// a discovery (SSR-D15 step 2). With q.IncludeUncarded the agents without a
// card follow the registry's last page (uncardedAgents).
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
	cursor, skip, err := parseRegistryCursor(q.Cursor)
	if err != nil {
		return nil, "", err
	}
	// Unfiltered, a page of the registry is a page of the answer; filtered,
	// the registry is read in its largest pages and matched here.
	size := agentsPageMax
	if q.Skill == "" && q.Tag == "" && q.Query == "" {
		size = limit
	}
	out := make([]module.RemoteAgent, 0, limit)
	now := d.nowMS()
	for pages := 0; ; pages++ {
		if pages == registryWalkPages {
			// Read far enough for one answer; the listing goes on from here.
			return out, registryCursor + "0:" + cursor, nil
		}
		var page hubapi.A2AAgentList
		err := d.hubGet(hctx, hub, hubapi.RegistryAgentsPath, registryPage(cursor, size), &page)
		if pages == 0 && hubStatus(err) == http.StatusNotFound {
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
		for i := min(skip, len(page.Agents)); i < len(page.Agents); i++ {
			ra, ok := d.listedAgent(page.Agents[i], q, now)
			if !ok {
				continue
			}
			out = append(out, ra)
			if len(out) < limit {
				continue
			}
			switch {
			case i+1 < len(page.Agents):
				return out, registryCursor + strconv.Itoa(i+1) + ":" + cursor, nil
			case page.NextCursor != "":
				return out, registryCursor + "0:" + page.NextCursor, nil
			}
		}
		skip = 0
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if !q.IncludeUncarded {
		return out, "", nil
	}
	// The registry's last page: the agents without a card follow it.
	carded, err := d.cardedAIDs(hctx, hub, q)
	if err != nil {
		return nil, "", err
	}
	tail, next, err := d.uncardedAgents(hctx, hub, q, carded, 0, limit-len(out))
	if err != nil {
		return nil, "", err
	}
	return append(out, tail...), next, nil
}

// parseRegistryCursor reads a listing's cursor: the registry's cursor of the
// page to read and how many entries of it were listed already. A cursor of
// another form is the registry's own, as an earlier listing gave it.
func parseRegistryCursor(c string) (string, int, error) {
	rest, ok := strings.CutPrefix(c, registryCursor)
	if !ok {
		return c, 0, nil
	}
	n, hubCursor, ok := strings.Cut(rest, ":")
	skip, err := strconv.Atoi(n)
	if !ok || err != nil || skip < 0 || skip > agentsPageMax {
		return "", 0, a2ashape.Errorf(a2ashape.ErrInvalidParams, "cursor %q is not one this node gave; list again from the start", c)
	}
	return hubCursor, skip, nil
}

// registryPage is the registry request of one page: the cursor and the
// page size, nothing about what is looked for.
func registryPage(cursor string, size int) url.Values {
	v := url.Values{}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	v.Set("limit", strconv.Itoa(size))
	return v
}

// listedAgent is one registry entry as listAgents shows it, and whether q
// keeps it: its card's skills and tags must hold q's (as the card claims
// them, verified or not), and what is shown must hold the free text. Its
// key history, key set and notice come with the entry and are verified here
// (registryMaterial); an agent whose card verifies is kept as a discovery.
func (d *Daemon) listedAgent(e hubapi.A2AAgentEntry, q module.AgentQuery, now uint64) (module.RemoteAgent, bool) {
	if !validAgentID(e.AID) {
		// Not an agent id. Its card cannot verify for it, and the AID is
		// all an unverified entry shows (an internal note item): a hub's text in its
		// place is not passed on either.
		return module.RemoteAgent{}, false
	}
	if !cardClaims(e.Card, q.Skill, q.Tag) {
		return module.RemoteAgent{}, false
	}
	m, kerr := d.registryMaterial(e, now)
	stored, haveStored := d.peerKEL(e.AID)
	ra := module.RemoteAgent{AID: e.AID, Card: e.Card, KeyState: keyStateOf(kerr == nil && m.NoticeOK)}
	d.verifyCardInto(&ra, func(aid string) ([]identity.SignedEvent, error) {
		switch {
		case aid != e.AID:
			return nil, fmt.Errorf("no key history for %s here", aid)
		case kerr == nil:
			return m.KEL, nil
		case haveStored:
			return stored, nil
		}
		return nil, kerr
	})
	if ra.Verification == cardVerified {
		// The list's charge is the public card's, always (PAY-C15): a
		// list never asks anyone for an extended card (PAY-C10).
		ra.Charge = chargeOf(ra.Card)
		ra.HubVerification, ra.HomeHub, ra.LastSeen, ra.Quiet = e.CardVerification, e.HomeHub, e.LastSeen, e.Quiet
		ra.ReviewCount, ra.AvgRating = e.ReviewCount, e.AvgRating
		if kerr == nil {
			m.Card = append(json.RawMessage(nil), ra.Card...)
			d.discover(m)
		}
	}
	// The text is matched against what is shown: an unverified card's
	// words are neither shown nor searched, only its AID.
	if q.Query != "" && !cardMatches(ra.Card, e.AID, q.Query) {
		return module.RemoteAgent{}, false
	}
	d.markOfficial(&ra)
	return ra, true
}

// registryMaterial is what a registry entry says of its agent besides the
// card, verified here (SSR-D15 step 2): the key history must replay to the
// entry's AID and not fork from this node's record (the longer of the two
// is taken); the key set, when it verifies against that history and does
// not roll back this node's, is kept; the notice is checked by C-51 at
// arrival = now.
func (d *Daemon) registryMaterial(e hubapi.A2AAgentEntry, now uint64) (peerMaterial, error) {
	m := peerMaterial{AID: e.AID}
	raw, err := hubapi.DecodeB64(e.KEL, -1)
	if err != nil || len(raw) == 0 {
		return m, errors.New("the registry entry carries no readable key history")
	}
	kel, err := seal.ParseKEL(raw)
	if err != nil {
		return m, err
	}
	if got, err := replayedAID(kel); err != nil || got != e.AID {
		return m, fmt.Errorf("the registry's key history for %s does not replay to it", e.AID)
	}
	m.KEL = kel
	if rawKeys, err := hubapi.DecodeB64(e.Keys, -1); err == nil && len(rawKeys) > 0 {
		if signed, err := seal.UnmarshalSignedEncKeySet(rawKeys); err == nil {
			if set, err := seal.VerifyEncKeySet(signed, e.AID, kel, now); err == nil {
				m.Keys, m.Epoch = &peerKeySet{signed: rawKeys, set: set}, set.Mbox.Epoch
			}
		}
	}
	if notice, err := hubapi.DecodeB64(e.KeyStateNotice, -1); err == nil && len(notice) > 0 && len(notice) <= seal.MaxNoticeBytes {
		m.Notice = notice
		m.NoticeOK = d.dirNoticeOK(d.ctx, notice, kel, now)
	}
	if d.againstRecord(&m, now) != verdictFound {
		return m, m.Err
	}
	return m, nil
}

// cardClaims reports whether the card claims a skill of id skill and a
// skill tagged tag ("" matches anything), as the hub's registry indexes
// them: read from the card as it is, before it is verified. The skill is
// matched as the directory's capabilities are (capListed): exactly, or a
// family "x.*" that a listed id falls in.
func cardClaims(card json.RawMessage, skill, tag string) bool {
	if skill == "" && tag == "" {
		return true
	}
	var c struct {
		Skills []struct {
			ID   string   `json:"id"`
			Tags []string `json:"tags"`
		} `json:"skills"`
	}
	if json.Unmarshal(card, &c) != nil {
		return false
	}
	hasSkill, hasTag := skill == "", tag == ""
	for _, s := range c.Skills {
		hasSkill = hasSkill || capListed([]string{s.ID}, skill)
		hasTag = hasTag || slices.Contains(s.Tags, tag)
	}
	return hasSkill && hasTag
}

// cardedAIDs is every agent the registry lists whose card claims q's skill:
// the agents with a card, which the uncarded list leaves out. Not by q's
// tag: the directory the uncarded list comes from has no tags, so an agent
// whose card lacks the tag is still in it, and must not be taken there for
// one without a card. Empty for a hub without the registry.
func (d *Daemon) cardedAIDs(ctx context.Context, hub string, q module.AgentQuery) (map[string]bool, error) {
	out := map[string]bool{}
	cursor := ""
	for range registryWalkPages {
		var page hubapi.A2AAgentList
		err := d.hubGet(ctx, hub, hubapi.RegistryAgentsPath, registryPage(cursor, agentsPageMax), &page)
		if hubStatus(err) == http.StatusNotFound {
			return out, nil
		}
		if err != nil {
			return nil, a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
		}
		for _, e := range page.Agents {
			if cardClaims(e.Card, q.Skill, "") {
				out[e.AID] = true
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
	return nil, a2ashape.Errorf(a2ashape.ErrUnavailable,
		"more than %d agents with a card are listed; narrow the search with skill to list the ones without",
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
// publish no card (an internal note item): the /agents directory, asked whole, less the
// agents the registry lists (carded), with the skill, the tag and the free
// text matched here against what the hub says (SSR-D16). Each is NONE; up
// to limit from off, and the cursor of the rest.
func (d *Daemon) uncardedAgents(ctx context.Context, hub string, q module.AgentQuery, carded map[string]bool, off, limit int) ([]module.RemoteAgent, string, error) {
	var resp struct {
		Agents []hubDirEntry `json:"agents"`
	}
	if err := d.hubGet(ctx, hub, "/agents", nil, &resp); err != nil {
		return nil, "", a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", err)
	}
	var matched []module.RemoteAgent
	seen := map[string]bool{}
	for _, a := range resp.Agents {
		if !validAgentID(a.AID) || carded[a.AID] || seen[a.AID] {
			continue
		}
		seen[a.AID] = true
		// An official AID is matched against what the signed manifest says
		// of it, never against what the hub wrote: the hub's words neither
		// show beside the mark (uncardedAgent) nor make the official agent
		// turn up for a search [redteam:F39]. Any other entry is matched
		// against the hub's statement, as it is shown.
		name, summary, caps := a.Name, a.Summary, a.Caps
		if mName, mCaps, official := d.officialSays(a.AID); official {
			name, summary, caps = mName, "", mCaps
		}
		if q.Skill != "" && !capListed(caps, q.Skill) {
			continue
		}
		if q.Tag != "" && !containsFold(strings.Join(caps, " "), q.Tag) {
			continue
		}
		if q.Query != "" && !containsFold(strings.Join(append([]string{a.AID, name, summary}, caps...), "\n"), q.Query) {
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
// what a hub wrote, which is not shown beside the mark (an internal note item).
func (d *Daemon) uncardedAgent(a hubDirEntry) module.RemoteAgent {
	// The directory listing carries no notice: its key state is stale
	// (KEL D-28), which does not stop a send.
	if official := d.IsOfficial(a.AID); official {
		return module.RemoteAgent{AID: a.AID, Verification: cardNone, Official: official,
			VerificationError: "the agent publishes no network card", KeyState: module.KeyStateStale}
	}
	ra := module.RemoteAgent{AID: a.AID, Verification: cardNone, VerificationError: uncardedNote, KeyState: module.KeyStateStale}
	ra.Name, ra.Caps, ra.Summary = a.Name, a.Caps, a.Summary
	ra.HomeHub, ra.LastSeen, ra.Quiet = a.HomeHub, a.LastSeen, a.Quiet
	ra.ReviewCount, ra.AvgRating = a.ReviewCount, a.AvgRating
	return ra
}

// capListed reports whether the directory lists capability id among caps:
// that id, or a family "x.*" that a listed id falls in.
func capListed(caps []string, id string) bool {
	if family, ok := strings.CutSuffix(id, "*"); ok && strings.HasSuffix(family, ".") {
		return slices.ContainsFunc(caps, func(c string) bool { return strings.HasPrefix(c, family) })
	}
	return slices.Contains(caps, id)
}

// agentCard is one agent's network card, verified here. It comes from the
// directory (resolvePeer: the discovery cache, else a bucket), never from a
// request that names the agent (SSR-D16). An agent without a card is not an
// error: it is returned NONE with an empty card, and the caller describes it
// by its AID (§11.3); a card that does not verify is returned UNVERIFIED,
// without its bytes; an agent no hub knows is NONE and says so.
//
// It never contacts the agent: the charge is the public card's, and
// ExtendedOffered says whether a display would ask the agent for its
// extended card (CORE-K23 rule 2; agentCardWithPrices asks).
func (d *Daemon) agentCard(ctx context.Context, aid string) (module.RemoteAgent, error) {
	ra, _, err := d.resolveAgentCard(ctx, aid)
	return ra, err
}

// publicCardFacts are what a verified public card and the key history it
// verified under tell the extended card's fetch and check (PAY-C13, C14):
// the card's seq and the agent's KEL.
type publicCardFacts struct {
	Seq uint64
	KEL []identity.SignedEvent
	// Skills are the public card's skill ids.
	Skills []string
}

// resolveAgentCard is agentCard, with the facts of a VERIFIED card (nil
// for any other).
func (d *Daemon) resolveAgentCard(ctx context.Context, aid string) (module.RemoteAgent, *publicCardFacts, error) {
	if !validAgentID(aid) {
		return module.RemoteAgent{}, nil, a2ashape.Errorf(a2ashape.ErrInvalidParams, "%q is not an agent id", aid)
	}
	if d.config().HubURL == "" {
		return module.RemoteAgent{}, nil, a2ashape.Errorf(a2ashape.ErrUnavailable, "this node has no hub (run `anet hub-register` first)")
	}
	official := d.IsOfficial(aid)
	m, v := d.resolvePeer(ctx, aid, resolveOpts{need: needCard})
	switch v {
	case verdictNotFound:
		return module.RemoteAgent{AID: aid, Verification: cardNone, Official: official,
			VerificationError: CardUnknownAgent, KeyState: module.KeyStateStale}, nil, nil
	case verdictUnverifiable:
		return module.RemoteAgent{AID: aid, Verification: cardUnverified, Official: official,
			VerificationError: "the directory's entries for this agent do not verify", KeyState: module.KeyStateStale}, nil, nil
	case verdictFound:
	default:
		return module.RemoteAgent{}, nil, a2ashape.Errorf(a2ashape.ErrUnavailable, "%v", &peerResolveError{AID: aid, Verdict: v, Cause: m.Err})
	}
	// Whether the agent's hub confirmed its current key, checked now: the
	// entry may come from the discovery cache (KEL D-28).
	keyState := keyStateOf(d.noticeFresh(ctx, &m, d.nowMS()))
	if len(m.Card) == 0 {
		ra := module.RemoteAgent{AID: aid, Verification: cardNone, Official: official,
			VerificationError: "the agent publishes no network card", KeyState: keyState}
		if m.CardErr != "" {
			ra.Verification, ra.VerificationError = cardUnverified, m.CardErr
		}
		return ra, nil, nil
	}
	ra := module.RemoteAgent{AID: aid, Card: m.Card, KeyState: keyState}
	pv := d.verifyCardInto(&ra, singleKEL(aid, m.KEL))
	d.markOfficial(&ra)
	if pv == nil {
		return ra, nil, nil
	}
	facts := &publicCardFacts{Seq: pv.Seq, KEL: m.KEL}
	for _, sk := range pv.Skills {
		facts.Skills = append(facts.Skills, sk.ID)
	}
	ra.Charge = chargeOf(ra.Card)
	ra.ExtendedOffered = d.extendedOffered(aid, ra.Card, d.nowMS())
	return ra, facts, nil
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
// from ra (an internal note item): what is kept is the AID, UNVERIFIED and the reason,
// in words of this node's or the a2acard code, never an error's detail,
// which can quote the card or the hub. It returns what a2acard.Verify
// read of a card that verified, nil otherwise.
func (d *Daemon) verifyCardInto(ra *module.RemoteAgent, resolve a2acard.Resolver) *a2acard.Verified {
	ra.Verification = cardUnverified
	if len(ra.Card) == 0 {
		ra.VerificationError = "no card"
		return nil
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
		return v
	}
	ra.Card, ra.Name = nil, ""
	return nil
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
