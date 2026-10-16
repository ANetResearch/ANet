//go:build !no_a2a

package a2a

import (
	"encoding/json"
	"testing"

	"github.com/ANetResearch/ANet/module"
)

// KEL A-2, D-28: the proxy card's anet-origin params say keyState, fresh
// only when the kernel said the agent's hub confirmed its current key, and
// stale otherwise — a card the kernel could not get included, which is a
// placeholder. The local agent list says the same of each agent.
func TestProxyCardAndListSayTheKeyState(t *testing.T) {
	e := newEnv(t)
	fresh := verifiedRemote(agentA)
	fresh.KeyState = module.KeyStateFresh
	e.seam.cards[agentA] = fresh
	stale := verifiedRemote(agentB)
	stale.KeyState = module.KeyStateStale
	e.seam.cards[agentB] = stale
	const unknownAID = "bafyunknownagent"
	get := func(aid string) []byte {
		t.Helper()
		resp, raw := e.raw("GET", agentsPath+"/"+aid+"/.well-known/agent-card.json",
			map[string]string{"Authorization": "Bearer " + testToken}, "")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", aid, resp.StatusCode, raw)
		}
		return raw
	}
	for aid, want := range map[string]string{agentA: "fresh", agentB: "stale", unknownAID: "stale"} {
		if p := originParams(t, get(aid)); p["keyState"] != want {
			t.Errorf("%s: anet-origin keyState %v, want %s", aid, p["keyState"], want)
		}
	}

	e.seam.agents = []module.RemoteAgent{fresh, stale, {AID: "bafynokeystate", Verification: unverified}}
	resp, raw := e.raw("GET", agentsPath, map[string]string{"Authorization": "Bearer " + testToken}, "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var out struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Agents) != 3 {
		t.Fatalf("agents: %v %s", err, raw)
	}
	want := map[string]string{agentA: "fresh", agentB: "stale", "bafynokeystate": "stale"}
	for _, a := range out.Agents {
		if a["keyState"] != want[a["aid"].(string)] {
			t.Errorf("%v: keyState %v", a["aid"], a["keyState"])
		}
	}
}

// HPP PAY-C15: the local agent list says each verified agent's charge as
// its public card declares it (the kernel's Agents, which never fetches an
// extended card), the same column anet find and MCP list_agents show; an
// agent whose card did not verify has none, whatever the kernel handed over.
func TestTheAgentListSaysTheCharge(t *testing.T) {
	e := newEnv(t)
	paid := verifiedRemote(agentA)
	paid.Charge = module.ChargePartial
	free := verifiedRemote(agentB)
	free.Charge = module.ChargeFree
	e.seam.agents = []module.RemoteAgent{paid, free,
		{AID: "bafyunverifiedcharge", Verification: unverified, Charge: module.ChargePaid}}
	resp, raw := e.raw("GET", agentsPath, map[string]string{"Authorization": "Bearer " + testToken}, "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var out struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Agents) != 3 {
		t.Fatalf("agents: %v %s", err, raw)
	}
	want := map[string]any{agentA: "partial", agentB: "free", "bafyunverifiedcharge": nil}
	for _, a := range out.Agents {
		if a["charge"] != want[a["aid"].(string)] {
			t.Errorf("%v: charge %v, want %v", a["aid"], a["charge"], want[a["aid"].(string)])
		}
	}
	if e.seam.extCalls != 0 {
		t.Fatalf("the list asked for %d extended cards", e.seam.extCalls)
	}
}
