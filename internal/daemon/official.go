package daemon

// official.go marks the agents the anet project runs (A2A-DESIGN §15).
//
// The one source is the official manifest built into this binary
// (internal/official): signed with the release key, verified once at start,
// and asked by AID each time an agent is shown. The mark appears under the
// key "anet.official" in /find (anet find), /agents/list and /agents/card
// (MCP list_agents, get_agent_card) and, through module.RemoteAgent, in the
// local A2A interface's agent list and proxy cards.
//
// It is a label. Nothing on the admission, trust, payment or delivery path
// reads it: an official agent is reached, admitted and paid exactly like
// any other, and a peer's being official opens nothing on this node. Nor
// does the manifest reach the hub, or give the hub admin plane any channel
// to this node or to the official agents.

import (
	"log"
	"time"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/official"
	"github.com/ANetResearch/ANet/module"
)

// loadOfficials verifies the embedded manifest. One that does not verify
// marks no one, and says so once; one that has expired is kept (it may
// only look expired because this clock is wrong) and marks no one until
// the clock or the binary changes.
func (d *Daemon) loadOfficials() {
	m, err := official.Embedded()
	if err != nil {
		log.Printf("anet: the official-agent manifest built into this binary does not verify (%v); no agent will be marked official", err)
		return
	}
	if err := m.CheckFresh(d.now()); err != nil {
		log.Printf("anet: %v; no agent is marked official until then", err)
	}
	d.officials.Store(m)
}

// IsOfficial reports whether aid is an agent the anet project runs: listed,
// by that exact AID, in the verified official manifest, which has not
// expired. A name, a card or a hub's word never makes an agent official.
func (d *Daemon) IsOfficial(aid string) bool {
	_, ok := d.officials.Load().Lookup(aid, d.now())
	return ok
}

// now is nowMS as a time.
func (d *Daemon) now() time.Time { return time.UnixMilli(int64(d.nowMS())) }

// markOfficial sets ra.Official from ra.AID.
func (d *Daemon) markOfficial(ra *module.RemoteAgent) { ra.Official = d.IsOfficial(ra.AID) }

// foundAgent is one /find answer: the hub directory's entry, and this
// node's mark beside it. hubapi.AgentView is the hub's wire type; the mark
// is this node's statement, not the hub's.
type foundAgent struct {
	hubapi.AgentView
	Official bool `json:"anet.official,omitempty"`
	// Note says, for an official agent, where the description shown comes
	// from.
	Note string `json:"anet.note,omitempty"`
}

// officialFoundNote is what an official /find entry says of itself.
const officialFoundNote = "an official anet agent: the name and capabilities shown are the signed official manifest's; " +
	"what the hub directory says of it is not shown"

// markFound is what /find shows of the hub directory's entries (0017 Q24,
// with its /find supplement; redteam F39). An entry whose aid is not an
// agent id is dropped: a hub can put text in that field too. An agent the
// official manifest lists is shown by its AID, the mark, and the name and
// capabilities the signed manifest gives it — nothing the hub wrote. The
// directory carries no card, so nothing a hub says of an agent is ever
// verified here, and a hub's words beside the official mark (a "payments
// desk" summary, a readme asking for credit) would borrow the mark's
// authority for them. Any other entry is the hub's statement, as it came.
func (d *Daemon) markFound(agents []hubapi.AgentView) []foundAgent {
	out := make([]foundAgent, 0, len(agents))
	for _, a := range agents {
		if f, ok := d.foundView(a); ok {
			out = append(out, f)
		}
	}
	return out
}

// foundView is one /find entry as shown (markFound), and whether it is
// shown at all.
func (d *Daemon) foundView(a hubapi.AgentView) (foundAgent, bool) {
	if !validAgentID(a.AID) {
		return foundAgent{}, false
	}
	e, official := d.officials.Load().Lookup(a.AID, d.now())
	if !official {
		return foundAgent{AgentView: a}, true
	}
	return foundAgent{
		AgentView: hubapi.AgentView{AID: a.AID, Name: e.Name, Caps: e.Caps, Listed: a.Listed},
		Official:  true,
		Note:      officialFoundNote,
	}, true
}

// shownAgents is the directory as /find shows it, as the hub's type:
// entries markFound drops are gone, and an official agent's entry carries
// only what markFound shows. /find matches its query against this, so a
// hub's words are not what makes an official agent turn up for a search.
func (d *Daemon) shownAgents(agents []hubapi.AgentView) []hubapi.AgentView {
	out := make([]hubapi.AgentView, 0, len(agents))
	for _, a := range agents {
		if f, ok := d.foundView(a); ok {
			out = append(out, f.AgentView)
		}
	}
	return out
}
