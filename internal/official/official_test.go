package official

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/release"
)

// The AIDs of the tests: the official agent, and a stranger who registered
// under the same name.
const (
	officialAID = "bofficialtools7q2xk4"
	strangerAID = "bstrangerxxxxxxxxxxx"
)

var (
	testIssued  = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	testExpires = time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC)
	testNow     = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
)

// testKey is a key made for one test; tests never use the release key.
type testKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{pub, priv}
}

func (k testKey) trust() release.Trust { return release.Trust{Keys: []ed25519.PublicKey{k.pub}} }
func (k testKey) fp() string           { return release.Fingerprint(k.pub) }

// sign signs raw the way build-release.sh --official does.
func (k testKey) sign(raw []byte) []byte { return release.Sign(k.priv, raw, release.OfficialNamespace) }

// manifestJSON is a manifest in the layout build-release.sh writes, with
// the given agents lines.
func manifestJSON(fp string, agents ...string) []byte {
	list := "[]"
	if len(agents) > 0 {
		list = "[\n    " + strings.Join(agents, ",\n    ") + "\n  ]"
	}
	return []byte(fmt.Sprintf(`{
  "schema": "anet-official/1",
  "seq": 3,
  "issued_at": "%s",
  "expires_at": "%s",
  "key_fingerprint": "%s",
  "agents": %s
}
`, testIssued.Format(timeLayout), testExpires.Format(timeLayout), fp, list))
}

func toolsEntry(aid string) string {
	return `{"id": "anet-tools", "name": "anet-tools", "aid": "` + aid +
		`", "hub": "https://hub.agentnetwork.org.cn", "caps": ["text.stats", "text.digest"]}`
}

// A manifest signed in the official namespace by a trusted key is read, and
// lists its agent by AID.
func TestASignedManifestListsItsAgents(t *testing.T) {
	k := newTestKey(t)
	raw := manifestJSON(k.fp(), toolsEntry(officialAID),
		`{"id": "anet-echo-f", "name": "anet echo (fmax)", "aid": "bechof", "hub": "https://hub2.agentnetwork.org.cn", "caps": ["net.echo"]}`)
	m, err := Verify(raw, k.sign(raw), k.trust())
	if err != nil {
		t.Fatal(err)
	}
	if m.Seq != 3 || !m.Expires().Equal(testExpires) || len(m.Agents) != 2 {
		t.Fatalf("manifest %+v", m)
	}
	e, ok := m.Lookup(officialAID, testNow)
	if !ok || e.ID != "anet-tools" || e.Hub != "https://hub.agentnetwork.org.cn" ||
		strings.Join(e.Caps, ",") != "text.stats,text.digest" {
		t.Fatalf("Lookup(official) = %+v, %v", e, ok)
	}
	// The caller's copy is its own.
	e.Caps[0] = "changed"
	if again, _ := m.Lookup(officialAID, testNow); again.Caps[0] != "text.stats" {
		t.Fatal("Lookup handed out the manifest's own caps slice")
	}
	if _, ok := m.Lookup("bechof", testNow); !ok {
		t.Fatal("the second agent is not listed")
	}
	if err := m.CheckFresh(testNow); err != nil {
		t.Fatal(err)
	}
}

// Only the AID decides. A stranger who registers under the official
// agent's name or id is not official, and neither is the name or id itself
// asked about as if it were an AID. [mut] Lookup by name → this is red.
func TestOnlyTheAIDMakesAnAgentOfficial(t *testing.T) {
	k := newTestKey(t)
	raw := manifestJSON(k.fp(), toolsEntry(officialAID))
	m, err := Verify(raw, k.sign(raw), k.trust())
	if err != nil {
		t.Fatal(err)
	}
	for _, aid := range []string{strangerAID, "anet-tools", "anettools", "", strings.ToUpper(officialAID),
		officialAID + " ", officialAID[:len(officialAID)-1]} {
		if e, ok := m.Lookup(aid, testNow); ok {
			t.Errorf("Lookup(%q) = %+v: only the listed AID is official", aid, e)
		}
	}
	var none *Manifest
	if _, ok := none.Lookup(officialAID, testNow); ok {
		t.Error("no manifest listed an agent")
	}
}

// Any change to the signed bytes, a signature in another namespace, and a
// signature by another key are refused. [mut] skip the signature check →
// the tampered manifest (a stranger's AID swapped in) is read, and this is
// red.
func TestATamperedOrMisSignedManifestIsRefused(t *testing.T) {
	k := newTestKey(t)
	raw := manifestJSON(k.fp(), toolsEntry(officialAID))
	sig := k.sign(raw)
	if _, err := Verify(raw, sig, k.trust()); err != nil {
		t.Fatal(err)
	}

	swapped := bytes.Replace(raw, []byte(officialAID), []byte(strangerAID), 1)
	if m, err := Verify(swapped, sig, k.trust()); err == nil {
		_, listed := m.Lookup(strangerAID, testNow)
		t.Fatalf("a manifest with a stranger's AID swapped in was read (stranger listed: %v)", listed)
	}
	if _, err := Verify(append(append([]byte{}, raw...), ' '), sig, k.trust()); err == nil {
		t.Error("a manifest with one byte added was read")
	}

	// The release namespace: the same key, another purpose.
	if _, err := Verify(raw, release.Sign(k.priv, raw, release.Namespace), k.trust()); err == nil ||
		!strings.Contains(err.Error(), "namespace") {
		t.Errorf("a release-namespace signature was read as an official one: %v", err)
	}
	// Another key.
	other := newTestKey(t)
	if _, err := Verify(raw, other.sign(raw), k.trust()); err == nil {
		t.Error("a manifest signed by an untrusted key was read")
	}
	// No signature, or something else in its place.
	for name, s := range map[string][]byte{"empty": nil, "html": []byte("<html>not found</html>"),
		"oversized": bytes.Repeat([]byte("A"), release.MaxSigBytes+1)} {
		if _, err := Verify(raw, s, k.trust()); err == nil {
			t.Errorf("%s signature: read", name)
		}
	}
	// The pre-committed next key signs after a rotation.
	rotated := manifestJSON(other.fp(), toolsEntry(officialAID))
	tr := k.trust()
	tr.NextFingerprint = other.fp()
	if _, err := Verify(rotated, other.sign(rotated), tr); err != nil {
		t.Errorf("the committed next key was refused: %v", err)
	}
}

// A correctly signed manifest marks no one once it has expired.
func TestAnExpiredManifestMarksNoOne(t *testing.T) {
	k := newTestKey(t)
	raw := manifestJSON(k.fp(), toolsEntry(officialAID))
	m, err := Verify(raw, k.sign(raw), k.trust())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Lookup(officialAID, testExpires.Add(-time.Second)); !ok {
		t.Fatal("not listed a second before expiry")
	}
	for _, at := range []time.Time{testExpires, testExpires.Add(time.Hour)} {
		if _, ok := m.Lookup(officialAID, at); ok {
			t.Errorf("listed at %s, after expires_at", at)
		}
		if err := m.CheckFresh(at); !errors.Is(err, ErrExpired) {
			t.Errorf("CheckFresh(%s) = %v, want ErrExpired", at, err)
		}
	}
}

// Every malformed manifest is refused even when correctly signed: the
// reader does not guess at what the signer meant.
func TestMalformedManifestsAreRefused(t *testing.T) {
	k := newTestKey(t)
	good := string(manifestJSON(k.fp(), toolsEntry(officialAID)))
	entry := func(id, name, aid, hub, caps string) string {
		return string(manifestJSON(k.fp(), fmt.Sprintf(`{"id": %q, "name": %q, "aid": %q, "hub": %q, "caps": %s}`,
			id, name, aid, hub, caps)))
	}
	cases := map[string]string{
		"unknown member":        strings.Replace(good, `"seq": 3,`, `"seq": 3, "trust": "all",`, 1),
		"unknown entry member":  strings.Replace(good, `"caps": [`, `"channel": "ssh://x", "caps": [`, 1),
		"other schema":          strings.Replace(good, `anet-official/1`, `anet-release/1`, 1),
		"seq 0":                 strings.Replace(good, `"seq": 3`, `"seq": 0`, 1),
		"negative seq":          strings.Replace(good, `"seq": 3`, `"seq": -1`, 1),
		"expires before issued": strings.Replace(good, testExpires.Format(timeLayout), "2026-08-01T00:00:00Z", 1),
		"bad time":              strings.Replace(good, testIssued.Format(timeLayout), "2026-09-01 00:00:00", 1),
		"no agents":             strings.Replace(string(manifestJSON(k.fp())), `"agents": []`, `"agents": null`, 1),
		"trailing data":         good + "{}",
		"other key named":       strings.Replace(good, k.fp(), newTestKey(t).fp(), 1),
		"duplicate aid": string(manifestJSON(k.fp(), toolsEntry(officialAID),
			strings.Replace(toolsEntry(officialAID), "anet-tools", "anet-tools-2", 1))),
		"duplicate id":   string(manifestJSON(k.fp(), toolsEntry(officialAID), toolsEntry(strangerAID))),
		"aid upper":      entry("anet-tools", "t", "BADAID", "https://h", `[]`),
		"aid empty":      entry("anet-tools", "t", "", "https://h", `[]`),
		"id":             entry("Anet Tools", "t", officialAID, "https://h", `[]`),
		"name empty":     entry("anet-tools", "", officialAID, "https://h", `[]`),
		"name long":      entry("anet-tools", strings.Repeat("n", 129), officialAID, "https://h", `[]`),
		"name control":   entry("anet-tools", "anet\ttools", officialAID, "https://h", `[]`),
		"hub scheme":     entry("anet-tools", "t", officialAID, "ftp://h", `[]`),
		"hub userinfo":   entry("anet-tools", "t", officialAID, "https://u:p@h", `[]`),
		"hub query":      entry("anet-tools", "t", officialAID, "https://h/?x=1", `[]`),
		"hub relative":   entry("anet-tools", "t", officialAID, "/hub", `[]`),
		"caps null":      entry("anet-tools", "t", officialAID, "https://h", `null`),
		"cap malformed":  entry("anet-tools", "t", officialAID, "https://h", `["Text Stats"]`),
		"cap not string": entry("anet-tools", "t", officialAID, "https://h", `[1]`),
	}
	for name, raw := range cases {
		if raw == good {
			t.Fatalf("%s: the mutation did not apply", name)
		}
		if _, err := Verify([]byte(raw), k.sign([]byte(raw)), k.trust()); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	// And the good one, with an empty agent list, is read.
	empty := manifestJSON(k.fp())
	if m, err := Verify(empty, k.sign(empty), k.trust()); err != nil || len(m.Agents) != 0 {
		t.Fatalf("an empty manifest: %v", err)
	}
	big := bytes.Repeat([]byte(" "), MaxManifestBytes+1)
	if _, err := Verify(big, k.sign(big), k.trust()); err == nil {
		t.Error("an oversized manifest was read")
	}
}

// The manifest this commit embeds verifies against the release key this
// commit embeds, in the official namespace, and is in the layout
// build-release.sh reads with sed. Expiry is not asked here — a test that
// fails on a date is a test that fails for no change — the release build
// checks it (build-release.sh check_official). build-release.sh --official
// runs this test on the pair it has just written.
func TestTheEmbeddedManifestVerifies(t *testing.T) {
	m, err := Embedded()
	if err != nil {
		t.Fatalf("the embedded official manifest does not verify: %v (re-sign it: deploy/release/build-release.sh --official)", err)
	}
	tr := release.OfficialTrust()
	signer := false
	for _, k := range tr.Keys {
		signer = signer || release.Fingerprint(k) == m.KeyFingerprint
	}
	if !signer && m.KeyFingerprint != tr.NextFingerprint {
		t.Fatalf("signed by %s, which the embedded trust does not list", m.KeyFingerprint)
	}
	for _, re := range []string{`(?m)^  "seq": [0-9]+,$`, `(?m)^  "expires_at": "[0-9TZ:-]+",$`,
		`(?m)^  "key_fingerprint": "SHA256:[A-Za-z0-9+/]{43}",$`} {
		if !regexp.MustCompile(re).Match(embeddedManifest) {
			t.Errorf("manifest.json has no line matching %s: build-release.sh reads it with sed", re)
		}
	}
}

// The official key is the release key: allowed_signers lists the one key
// for both namespaces, and for no other.
func TestTheOfficialKeyIsTheReleaseKey(t *testing.T) {
	rel, off := release.DefaultTrust(), release.OfficialTrust()
	if len(off.Keys) != 1 || len(rel.Keys) != 1 || !off.Keys[0].Equal(rel.Keys[0]) {
		t.Fatalf("official keys %d, release keys %d: want the same single key", len(off.Keys), len(rel.Keys))
	}
	if off.NextFingerprint != rel.NextFingerprint {
		t.Fatal("the next-key commitment differs between the two namespaces")
	}
	if _, err := release.ParseAllowedSignersFor(release.AllowedSigners(), "file"); err == nil {
		t.Fatal("the release key is listed for a namespace it has no business in")
	}
}

// SI-8 and "labels only": this package links neither a2a-go nor anything
// of the daemon or its modules. It is read by them, not the other way.
func TestTheManifestReaderIsALeaf(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	out, err := exec.Command(gobin, "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "a2aproject") || strings.Contains(pkg, "ANet/internal/daemon") ||
			strings.Contains(pkg, "ANet/module") {
			t.Errorf("%s is in the dependency closure of internal/official", pkg)
		}
	}
}

// "Labels only", across the repository: the manifest is read by the daemon
// (which labels agents, internal/daemon/official.go, where a test pins
// which files may ask it) and by anet doctor (which reports it), and by no
// other package — not the payment, admission or delivery code, not a
// module, not the MCP server. A new importer is a new use and has to be
// added here on purpose. Each tag that adds files (shell, no_a2a, no_mcp)
// is asked too: a tagged file can import what the default build does not.
func TestOnlyTheLabellersImportTheManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	const self = "github.com/ANetResearch/ANet/internal/official"
	allowed := map[string]bool{
		"github.com/ANetResearch/ANet/internal/daemon": true,
		"github.com/ANetResearch/ANet/cmd/anet":        true,
	}
	for _, tags := range []string{"", "shell", "no_a2a", "no_mcp"} {
		out, err := exec.Command(gobin, "list", "-tags", tags, "-f",
			"{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", "github.com/ANetResearch/ANet/...").CombinedOutput()
		if err != nil {
			t.Fatalf("go list -tags %q: %v\n%s", tags, err, out)
		}
		importers := 0
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			for _, imp := range f[1:] {
				if imp != self {
					continue
				}
				importers++
				if !allowed[f[0]] {
					t.Errorf("-tags %q: %s imports internal/official; the manifest is for labels only", tags, f[0])
				}
			}
		}
		if importers == 0 {
			t.Errorf("-tags %q: nothing imports internal/official (is the go list output what this test reads?)", tags)
		}
	}
}
