package daemon

import (
	"net/http"
	"strings"
	"time"

	"github.com/ANetResearch/ANet/internal/hubapi"
)

// RunningDaemons returns the locally-recorded identities whose daemon is actually reachable right now
// (a fast loopback /ping). The registry can hold stale entries for daemons that crashed without cleaning
// up, so the CLI probes each before showing it — used to tell an operator "a daemon IS running, but at a
// different data dir/port; set ANET_DATA_DIR" when their default data dir has no live daemon.
func RunningDaemons() []IdentityEntry {
	list, err := listRegistry()
	if err != nil || len(list) == 0 {
		return nil
	}
	client := &http.Client{Timeout: 400 * time.Millisecond}
	out := make([]IdentityEntry, 0, len(list))
	for _, e := range list {
		if e.ControlAddr == "" {
			continue
		}
		resp, err := client.Get("http://" + e.ControlAddr + "/ping")
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			out = append(out, e)
		}
	}
	return out
}

// matchAgents keeps the agents whose AID, name, summary, readme or one of
// whose capabilities contains query, ignoring case — the fields the hub's
// own search reads. An empty query keeps every agent.
//
// Done here rather than by the hub's ?q= because what an operator searches
// for says what they are about to do, and the hub has no need to know it
// (A2A-DESIGN §10.5: the daemon matches free text locally; the hub's q is
// for its web UI).
func matchAgents(agents []hubapi.AgentView, query string) []hubapi.AgentView {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return agents
	}
	out := make([]hubapi.AgentView, 0, len(agents))
	for _, a := range agents {
		for _, f := range append([]string{a.AID, a.Name, a.Summary, a.Readme}, a.Caps...) {
			if strings.Contains(strings.ToLower(f), q) {
				out = append(out, a)
				break
			}
		}
	}
	return out
}
