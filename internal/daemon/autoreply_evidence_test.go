package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/evtypes"
)

var updateEvidenceGolden = flag.Bool("update-evidence", false, "rewrite internal/evtypes/testdata")

// evidenceGoldenDir holds G-EVID: the payloads of anet.autoreply.invoked and anet.hub.admission (LEC
// §7.1), read by the evtypes tests against the registered shapes.
var evidenceGoldenDir = filepath.Join("..", "evtypes", "testdata")

// sampleEgress is a sandboxed run's egress counts for the samples.
func sampleEgress() *egressSummary {
	return &egressSummary{ModelRequests: 3, BytesUp: 18432, BytesDown: 52100,
		PolicyRefused: map[string]int{"policy_server_tool": 1},
		Connect: egressConnectSummary{Allowed: map[string]int{"pypi.org": 2}, Refused: map[string]int{"egress_host_not_allowed": 1},
			BytesUp: 2048, BytesDown: 1 << 20}}
}

// G-EVID: anet.autoreply.invoked for a direct run, a sandboxed run, an openai turn, a turn the budget
// turned away and a run killed for its disk use, and anet.hub.admission, byte for byte; each has
// exactly the members of LEC §2.5 (the evtypes tests read the same files against the shape).
func TestAutoReplyEvidenceGolden(t *testing.T) {
	const ix, peer = "ix_0f1e2d3c4b5a69788796a5b4c3d2e1f0", "bafyreiexamplepeeraid7hq2abcd"
	samples := map[string]map[string]any{
		"autoreply_invoked_direct.json": autoReplyRun{Agent: "claude", InteractionID: ix, Peer: peer, Trusted: true,
			Mode: modeDirect, Exit: exitOK, Duration: 41250 * time.Millisecond}.payload(),
		"autoreply_invoked_sandbox.json": autoReplyRun{Agent: "claude", InteractionID: ix, Peer: peer, Sandboxed: true,
			Mode: modeSandbox, Kind: "bwrap", Exit: exitOK, Duration: 63800 * time.Millisecond, Egress: sampleEgress()}.payload(),
		"autoreply_invoked_openai.json": autoReplyRun{InteractionID: ix, Peer: peer, Mode: modeOpenAI, Exit: exitError,
			Duration: 2100 * time.Millisecond}.payload(),
		"autoreply_invoked_budget.json": autoReplyRun{Agent: "claude", InteractionID: ix, Peer: peer, Mode: modeSandbox,
			Exit: exitBudget}.payload(),
		"autoreply_invoked_resource.json": autoReplyRun{Agent: "claude", InteractionID: ix, Peer: peer, Sandboxed: true,
			Mode: modeSandbox, Kind: "bwrap", Exit: exitResource, Duration: 95 * time.Second, Egress: sampleEgress()}.payload(),
		"autoreply_invoked_unavailable.json": autoReplyRun{Agent: "cursor", InteractionID: ix, Peer: peer, Mode: modeSandbox,
			Exit: exitSandboxUnavailable, Unavailable: "agent_unsupported"}.payload(),
	}
	// anet.hub.admission as the join loop writes it.
	d := newBareDaemon(t)
	d.recordJoinChange(joinRecord{Hub: "https://hub.example", State: "pending_admission", Reason: "hub_pending_admission"})
	d.recordJoinChange(joinRecord{Hub: "https://hub.example", State: "registered"})
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: evtypes.HubAdmission, Limit: 10})
	if len(recs) != 2 {
		t.Fatalf("%d hub admission records", len(recs))
	}
	samples["hub_admission_pending.json"] = recs[0].Payload
	samples["hub_admission_registered.json"] = recs[1].Payload
	if recs[0].Payload["state"] != "pending_admission" {
		recs[0], recs[1] = recs[1], recs[0]
		samples["hub_admission_pending.json"], samples["hub_admission_registered.json"] = recs[0].Payload, recs[1].Payload
	}

	for name, p := range samples {
		got, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join(evidenceGoldenDir, name)
		if *updateEvidenceGolden {
			if err := os.MkdirAll(evidenceGoldenDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (regenerate with -update-evidence)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s:\n%s\nwant\n%s", name, got, want)
		}
		typ := evtypes.AutoReplyInvoked
		if strings.HasPrefix(name, "hub_admission") {
			typ = evtypes.HubAdmission
		}
		assertPayloadShape(t, typ, p)
	}
}

// assertPayloadShape fails when an evidence payload's members are not exactly its registered shape.
func assertPayloadShape(t *testing.T, typ string, p map[string]any) {
	t.Helper()
	shape, ok := evtypes.PayloadOf(typ)
	if !ok {
		t.Fatalf("%s has no registered payload", typ)
	}
	// Through JSON, the way the samples and anet audit see it.
	b, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if why := shape.Check(m); why != "" {
		t.Errorf("%s payload %s: %s", typ, b, why)
	}
}

// The record never holds the turn's content or the operator's secrets (LEC §2.5): whatever the run
// was given, the payload names only the agent, the ids, the mode and the counts.
func TestAutoReplyRecordHoldsNoContent(t *testing.T) {
	const canary = "CANARY-7f3a-secret"
	run := autoReplyRun{Agent: "claude", InteractionID: "ix_1", Peer: "bafyreipeer", Mode: modeSandbox, Kind: "bwrap",
		Sandboxed: true, Exit: exitError, Duration: time.Second, Egress: sampleEgress()}
	b, _ := json.Marshal(run.payload())
	if strings.Contains(string(b), canary) || strings.Contains(string(b), "/") {
		t.Fatalf("the record carries a path or content: %s", b)
	}
	assertPayloadShape(t, EvAutoReplyInvoked, run.payload())
}
