package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/hubapi"
	"github.com/ANetResearch/ANet/internal/official"
	"github.com/ANetResearch/ANet/internal/release"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
	"github.com/ANetResearch/ANet/module"
)

// testOfficials is an official manifest listing aids, signed with a key
// made for this test (never the release key) and verified the way the
// embedded one is, valid from an hour ago to a day from now — or, with
// expired, from two days ago to one day ago.
func testOfficials(t *testing.T, expired bool, aids ...string) *official.Manifest {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	from, until := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	if expired {
		from, until = time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)
	}
	const layout = "2006-01-02T15:04:05Z"
	entries := []string{}
	for i, aid := range aids {
		entries = append(entries, fmt.Sprintf(`{"id": "anet-tools-%d", "name": "anet-tools", "aid": %q, `+
			`"hub": "https://hub.agentnetwork.org.cn", "caps": ["text.stats"]}`, i, aid))
	}
	raw := []byte(fmt.Sprintf(`{"schema": "anet-official/1", "seq": 1, "issued_at": %q, "expires_at": %q, `+
		`"key_fingerprint": %q, "agents": [%s]}`, from.UTC().Format(layout), until.UTC().Format(layout),
		release.Fingerprint(pub), strings.Join(entries, ", ")))
	m, err := official.Verify(raw, release.Sign(priv, raw, release.OfficialNamespace),
		release.Trust{Keys: []ed25519.PublicKey{pub}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The official agent and a stranger who registered under its name. Both
// publish a card and a directory entry calling themselves anet-tools.
const (
	offAID      = "bofficialtools7q2xk4"
	imposterAID = "bimposterzzzzzzzzzzz"
)

// namesakeHub is a hub whose registry and directory list the official
// agent and the imposter under the same name, with the same skill.
func namesakeHub(t *testing.T) *httptest.Server {
	t.Helper()
	card := `{"name": "anet-tools", "description": "the official text tools", "skills": [{"id": "text.stats", "name": "text.stats"}]}`
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(hubapi.WireVersionHeader, strconv.Itoa(hubapi.WireVersion))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/a2a/v1/agents":
			fmt.Fprintf(w, `{"agents":[{"aid":%q,"card":%s,"cardVerification":"VERIFIED"},`+
				`{"aid":%q,"card":%s,"cardVerification":"VERIFIED"}],"nextCursor":""}`, offAID, card, imposterAID, card)
		case "/a2a/v1/agents/" + offAID + "/card", "/a2a/v1/agents/" + imposterAID + "/card":
			fmt.Fprint(w, card)
		case "/agents":
			fmt.Fprintf(w, `{"agents":[{"aid":%q,"name":"anet-tools","caps":["text.stats"],"listed":true},`+
				`{"aid":%q,"name":"anet-tools","caps":["text.stats"],"listed":true}]}`, offAID, imposterAID)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	return hub
}

// The contract of the mark (A2A-DESIGN §15): the key is "anet.official",
// its value is true, and it appears on the agent whose AID the manifest
// lists — in /agents/list and /agents/card (MCP list_agents,
// get_agent_card) and in /find (anet find) — and on no other. The imposter
// has the same name, the same card and the same skill, and is not marked:
// the key is absent, not false. [mut] Lookup by name → the imposter is
// marked and this is red.
func TestOfficialAgentsAreMarkedByAIDOnly(t *testing.T) {
	hub := namesakeHub(t)
	d := newTestDaemon(t, hub.URL, false)
	d.officials.Store(testOfficials(t, false, offAID))
	p := newPlaneFor(t, d, "tok")
	call := func(path, body string) []byte {
		t.Helper()
		resp, raw := p.req(t, "POST", path, body, p.bearer)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d %s", path, body, resp.StatusCode, raw)
		}
		return raw
	}
	// marks reads the "anet.official" member of each agent, by AID; a
	// missing member reads as nil.
	marks := func(path string, raw []byte) map[string]any {
		t.Helper()
		var out struct {
			Agents []map[string]json.RawMessage `json:"agents"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		got := map[string]any{}
		for _, a := range out.Agents {
			var aid string
			_ = json.Unmarshal(a["aid"], &aid)
			var v any
			if m, ok := a["anet.official"]; ok {
				_ = json.Unmarshal(m, &v)
			}
			got[aid] = v
		}
		return got
	}
	want := func(path string, got map[string]any) {
		t.Helper()
		if got[offAID] != true {
			t.Errorf("%s: the official agent carries anet.official=%v, want true", path, got[offAID])
		}
		if v, listed := got[imposterAID]; !listed || v != nil {
			t.Errorf("%s: the namesake imposter carries anet.official=%v (listed %v), want the key absent", path, v, listed)
		}
	}

	raw := call("/agents/list", `{"skill":"text.stats"}`)
	if !strings.Contains(string(raw), `"anet.official":true`) {
		t.Fatalf("/agents/list does not spell the mark \"anet.official\":true: %s", raw)
	}
	want("/agents/list", marks("/agents/list", raw))
	want("/find", marks("/find", call("/find", `{"query":"anet-tools"}`)))
	want("/find --cap", marks("/find", call("/find", `{"capability":"text.stats"}`)))

	one := func(aid string) any {
		var ra map[string]any
		if err := json.Unmarshal(call("/agents/card", `{"aid":"`+aid+`"}`), &ra); err != nil {
			t.Fatal(err)
		}
		return ra["anet.official"]
	}
	if v := one(offAID); v != true {
		t.Errorf("/agents/card: the official agent carries anet.official=%v", v)
	}
	if v := one(imposterAID); v != nil {
		t.Errorf("/agents/card: the imposter carries anet.official=%v", v)
	}
	// The kernel's answer is the same one.
	if !d.IsOfficial(offAID) || d.IsOfficial(imposterAID) || d.IsOfficial("anet-tools") || d.IsOfficial("") {
		t.Error("IsOfficial answers by something other than the AID")
	}
}

// Without a verified manifest, or with an expired one, nobody is marked.
func TestNoOrExpiredManifestMarksNoOne(t *testing.T) {
	hub := namesakeHub(t)
	d := newTestDaemon(t, hub.URL, false)
	ctx := context.Background()
	for name, m := range map[string]*official.Manifest{
		"none":    nil,
		"expired": testOfficials(t, true, offAID),
	} {
		d.officials.Store(m)
		if d.IsOfficial(offAID) {
			t.Errorf("%s: the agent is official", name)
		}
		agents, _, err := d.listAgents(ctx, module.AgentQuery{Skill: "text.stats"})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range agents {
			if a.Official {
				t.Errorf("%s: %s is marked official", name, a.AID)
			}
		}
	}
}

// The mark grants nothing. A requester this node's manifest calls official
// sends a task to a closed node that has not allowed it: it is refused
// exactly as a stranger is, and nothing is stored.
func TestTheOfficialMarkGrantsNoAdmission(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	prov := registered(t, srv.URL, "prov")
	offReq := registered(t, srv.URL, "anet-tools")
	stranger := registered(t, srv.URL, "stranger")
	prov.officials.Store(testOfficials(t, false, offReq.AID()))
	if !prov.IsOfficial(offReq.AID()) {
		t.Fatal("setup: the requester is not official to the provider")
	}
	send := func(from *Daemon) string {
		t.Helper()
		id, err := from.Delegate(ctx, prov.AID(), "please do this", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := prov.pollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oid, sid := send(offReq), send(stranger)
	oState, oMeta := lastStatusMeta(t, offReq, oid)
	sState, sMeta := lastStatusMeta(t, stranger, sid)
	if oState != interactions.StateRejected || oMeta["anet.reason"] != reasonNotAccepting {
		t.Fatalf("an official requester under closed: %s %v, want rejected/%s", oState, oMeta, reasonNotAccepting)
	}
	if oState != sState || string(mustJSON(t, oMeta)) != string(mustJSON(t, sMeta)) {
		t.Fatalf("an official requester sees %s %v, a stranger %s %v", oState, oMeta, sState, sMeta)
	}
	if _, err := prov.ix.Get(oid); !errors.Is(err, interactions.ErrNotFound) {
		t.Fatal("the official requester's refused task was stored")
	}
}

// The manifest is read for labels and nowhere else: in this package only
// official.go asks it, and only discovery and /find call what official.go
// offers. A use on the admission, trust, payment or delivery path would be
// a channel the design (§15) rules out, and has to be added here, on
// purpose, to pass.
func TestTheOfficialManifestIsReadOnlyForLabels(t *testing.T) {
	allowed := map[string]map[string]bool{
		"official.go":        {"officials": true, "IsOfficial": true, "markOfficial": true, "markFound": true, "official": true},
		"daemon.go":          {"officials": true, "loadOfficials": true, "official": true},
		"taskseam_agents.go": {"IsOfficial": true, "markOfficial": true},
		"control_api.go":     {"markFound": true},
	}
	watched := map[string]bool{"officials": true, "IsOfficial": true, "markOfficial": true, "markFound": true,
		"loadOfficials": true, "official": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			x, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := x.Sel.Name
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "official" {
				name = "official" // the package
			}
			if watched[name] && !allowed[f][name] {
				t.Errorf("%s: %s uses %q; the official manifest is for labels only (official.go)",
					f, fset.Position(n.Pos()), name)
			}
			return true
		})
	}
}
