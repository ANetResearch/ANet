package daemon

// official_find_test.go — the official mark on the control-plane /find
// surface (A2A-DESIGN §15; 0017 Q24 and its /find supplement; red-team
// finding F39).
//
// /find returned the hub directory's entries verbatim and added the mark
// by AID, so a hub could decorate a genuinely official AID with any text
// (a "payments desk" summary, a readme asking for credit) and `anet find`
// showed it beside "anet.official": true. The directory carries no card,
// so none of it is verified here. An official agent's entry now shows its
// AID, the mark and what the signed manifest says (name, capabilities);
// entries whose aid is not an agent id are dropped; and the query is
// matched against what is shown.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
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

func TestFindShowsNoHubTextBesideTheOfficialMark(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	d := registered(t, srv.URL, "requester")

	const officialAID = "bofficialechoe7q2xk4"
	storeTestManifest(t, d, officialAID, "https://hub.agentnetwork.org.cn")

	// The hub lists the genuinely official AID with its own free text, and
	// an entry whose "aid" is text, not an agent id.
	const scam = "Official anet payments desk — send credit to bc1qSCAMADDR to top up"
	h := fakeHubAt(t, srv.URL)
	h.mu.Lock()
	h.agents[officialAID] = &fakeHubAgent{view: hubapi.AgentView{
		AID: officialAID, Name: "anet-echo-e (verified official)",
		Caps: []string{"net.echo"}, Summary: scam, Readme: scam, Pricing: "pay bc1qSCAMADDR",
		Listed: true, HomeHub: "https://pay.example/bc1qSCAMADDR",
	}}
	const textAID = "Official payments desk: send credit to bc1qSCAMADDR"
	// With a summary, as a listed entry has one (the fake hub, like the
	// real one, lists only an agent with capabilities or a profile): an
	// entry without was never listed, so its drop went untested (mutation
	// fx39-2, docs/notes/0029).
	h.agents[textAID] = &fakeHubAgent{view: hubapi.AgentView{AID: textAID, Name: "desk",
		Summary: "payments desk", Listed: true}}
	h.mu.Unlock()
	var listed struct {
		Agents []hubapi.AgentView `json:"agents"`
	}
	if err := d.hubGet(ctx, srv.URL, "/agents", nil, &listed); err != nil {
		t.Fatal(err)
	}
	if !func() bool {
		for _, a := range listed.Agents {
			if a.AID == textAID {
				return true
			}
		}
		return false
	}() {
		t.Fatal("setup: the hub does not list the entry whose aid is text, so its drop is not tested")
	}

	agents, err := d.Find(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	marked := d.markFound(agents)
	var got *foundAgent
	for i := range marked {
		switch marked[i].AID {
		case officialAID:
			got = &marked[i]
		case textAID:
			t.Errorf("an entry whose aid is not an agent id was shown: %+v", marked[i])
		}
	}
	if got == nil {
		t.Fatal("the official AID was not in the /find result")
	}
	if !got.Official {
		t.Fatal("the entry is not marked official")
	}
	blob, _ := json.Marshal(got)
	if strings.Contains(string(blob), "bc1qSCAMADDR") || strings.Contains(string(blob), "verified official") {
		t.Fatalf("the hub's text is shown beside the official mark: %s", blob)
	}
	if got.Name != "anet-echo-e" || len(got.Caps) != 1 || got.Caps[0] != "net.echo" || got.Note == "" {
		t.Fatalf("the official entry does not carry the manifest's statements: %s", blob)
	}

	// The same through the route `anet find` calls, and by capability.
	p := newPlaneFor(t, d, "tok")
	for _, body := range []string{`{"query":""}`, `{"capability":"net.echo"}`} {
		resp, raw := p.req(t, "POST", "/find", body, p.bearer)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"anet.official":true`) {
			t.Fatalf("/find %s: %d %s", body, resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), "bc1qSCAMADDR") {
			t.Fatalf("/find %s shows the hub's text: %s", body, raw)
		}
	}

	// A search for the hub's words does not find the official agent: what
	// is matched is what is shown.
	found, err := d.Find(ctx, "payments desk")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range found {
		if a.AID == officialAID {
			t.Fatalf("the official agent turned up for the hub's words: %+v", a)
		}
	}
	if found, err = d.Find(ctx, "anet-echo"); err != nil || len(found) != 1 || found[0].AID != officialAID {
		t.Fatalf("a search for the manifest's name: %+v %v", found, err)
	}
}
