package daemon

import (
	"net/http"
	"sort"
	"testing"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// withRegistry turns on the fake hub's A2A registry at url.
func withRegistry(t testing.TB, url string) {
	t.Helper()
	h := fakeHubAt(t, url)
	h.mu.Lock()
	h.registry = true
	h.mu.Unlock()
}

// hRegistry is GET /a2a/v1/agents when the fake has the registry on: every
// agent with an admitted card, in AID order, on one page, each with its key
// history, key set and notice, as the real hub lists them.
func (h *fakeHub) hRegistry(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.registry {
		http.NotFound(w, r)
		return
	}
	var aids []string
	for aid := range h.a2aCards {
		aids = append(aids, aid)
	}
	sort.Strings(aids)
	out := hubapi.A2AAgentList{Agents: []hubapi.A2AAgentEntry{}}
	for _, aid := range aids {
		a := h.agents[aid]
		if a == nil {
			continue
		}
		notice, _ := h.noticeForLocked(aid)
		out.Agents = append(out.Agents, hubapi.A2AAgentEntry{AID: aid, Card: h.a2aCards[aid],
			CardVerification: hubapi.CardVerificationOK, KEL: hubapi.B64(a.kel), Keys: hubapi.B64(a.keyset),
			KeyStateNotice: notice})
	}
	fakeHubJSON(w, http.StatusOK, out)
}
