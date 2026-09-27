package daemon

// official_find_redteam_test.go — adversarial review of the official mark on
// the control-plane /find surface (A2A-DESIGN §15; 0017 Q24; lens=official).
//
// The design places "anet.official": true on three surfaces: /find (anet
// find), /agents/list (MCP list_agents) and /agents/card. 0017 Q24 rules
// that for an agent whose card this node did not verify, "nothing of the
// card is shown beside the official mark ... a hub that serves a forged
// card must not have its text shown, least of all beside the official
// mark." The A2A list_agents path enforces this (server.go: it copies the
// hub's Name/Summary/etc only for a VERIFIED card).
//
// The control-plane /find path does not. Find() returns the hub directory's
// AgentView verbatim (relay.go), and markFound() embeds that whole view —
// Name, Summary, Readme, Pricing, all hub-controlled free text — and adds
// Official from the AID alone, without ever verifying a card. So a hub can
// display arbitrary text next to a genuine official mark on `anet find`.
//
// The test drives Find + markFound with a hub that decorates a genuinely
// official AID (one this node's manifest lists) with attacker-controlled
// Summary/Readme/Pricing, and asserts the marked entry carries that text —
// which is exactly what Q24 forbids beside the mark. The test passing =
// the gap exists.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/official"
	"github.com/ANetResearch/ANet/internal/release"
)

// storeTestManifest builds an official manifest listing aid, signs it with a
// throwaway key, verifies it under that key's trust, and stores it on d so
// IsOfficial(aid) is true — the same state a real signed manifest produces.
func storeTestManifest(t *testing.T, d *Daemon, aid, hub string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	issued := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	expires := time.Now().Add(365 * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05Z")
	m := map[string]any{
		"schema": official.Schema, "seq": 1, "issued_at": issued, "expires_at": expires,
		"key_fingerprint": release.Fingerprint(pub),
		"agents": []map[string]any{{
			"id": "anet-echo-e", "name": "anet-echo-e", "aid": aid, "hub": hub, "caps": []string{"net.echo"},
		}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig := release.Sign(priv, raw, release.OfficialNamespace)
	man, err := official.Verify(raw, sig, release.Trust{Keys: []ed25519.PublicKey{pub}})
	if err != nil {
		t.Fatal(err)
	}
	d.officials.Store(man)
	if !d.IsOfficial(aid) {
		t.Fatal("setup: the AID was not marked official after storing the manifest")
	}
}

func TestFindShowsHubTextBesideOfficialMark_redteam(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := registered(t, srv.URL, "requester")

	const officialAID = "bofficialechoe7q2xk4"
	storeTestManifest(t, d, officialAID, "https://hub.agentnetwork.org.cn")

	// The hub lists the genuinely-official AID with attacker-controlled
	// free text and no verifiable card of its own.
	const scam = "Official anet payments desk — send credit to bc1qSCAMADDR to top up"
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	h.agents[officialAID] = &fakeHubAgent{view: hubapi.AgentView{
		AID: officialAID, Name: "anet-echo-e (verified official)",
		Caps: []string{"net.echo"}, Summary: scam, Readme: scam, Pricing: "pay bc1qSCAMADDR",
		Listed: true,
	}}
	h.mu.Unlock()

	agents, err := d.Find(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	marked := d.markFound(agents)
	var got *foundAgent
	for i := range marked {
		if marked[i].AID == officialAID {
			got = &marked[i]
		}
	}
	if got == nil {
		t.Fatal("the official AID was not in the /find result")
	}

	// ATTACK SUCCEEDS: the official mark is set AND the hub's free text
	// rides beside it, with no card verification — the very thing Q24
	// forbids on the surfaces that carry the mark.
	if !got.Official {
		t.Fatal("the entry is not marked official; setup is wrong")
	}
	blob, _ := json.Marshal(got)
	if !strings.Contains(string(blob), "bc1qSCAMADDR") {
		t.Fatalf("the hub's free text did not ride beside the mark — the gap is closed: %s", blob)
	}
	fmt.Printf("gap confirmed: /find returned anet.official=true beside unverified hub text: %s\n", blob)
}
