package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANet/internal/evtypes"
)

var updateHubAdmissionGolden = flag.Bool("update-hub-admission", false, "rewrite the hub_admission_* files of internal/evtypes/testdata")

// G-EVID: anet.hub.admission as the join loop writes it, byte for byte, and with exactly the members of the
// registered shape (the evtypes tests read the same files). The autoreply_invoked_* files beside them are
// the auto-reply module's (module/autoreply/autoreply_evidence_test.go).
func TestHubAdmissionEvidenceGolden(t *testing.T) {
	d := newBareDaemon(t)
	d.recordJoinChange(joinRecord{Hub: "https://hub.example", State: "pending_admission", Reason: "hub_pending_admission"})
	d.recordJoinChange(joinRecord{Hub: "https://hub.example", State: "registered"})
	_, recs := d.ledger.Evidence(EvidenceQuery{EventType: evtypes.HubAdmission, Limit: 10})
	if len(recs) != 2 {
		t.Fatalf("%d hub admission records", len(recs))
	}
	if recs[0].Payload["state"] != "pending_admission" {
		recs[0], recs[1] = recs[1], recs[0]
	}
	samples := map[string]map[string]any{
		"hub_admission_pending.json":    recs[0].Payload,
		"hub_admission_registered.json": recs[1].Payload,
	}
	dir := filepath.Join("..", "evtypes", "testdata")
	for name, p := range samples {
		got, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join(dir, name)
		if *updateHubAdmissionGolden {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (regenerate with -update-hub-admission)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s:\n%s\nwant\n%s", name, got, want)
		}
		shape, ok := evtypes.PayloadOf(evtypes.HubAdmission)
		if !ok {
			t.Fatal("anet.hub.admission has no registered payload")
		}
		b, _ := json.Marshal(p)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if why := shape.Check(m); why != "" {
			t.Errorf("payload %s: %s", b, why)
		}
	}
}
