package release

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestVectorManifestParses(t *testing.T) {
	m, s, err := VerifyManifest(readVector(t, "release.json"), readVector(t, "release.json.sig"), vectorTrust(t))
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(s.PublicKey) != m.KeyFingerprint {
		t.Fatal("key_fingerprint not tied to the signer")
	}
	if m.Version != "0.2.0" || m.ExpiresAt != "2026-12-26T09:00:00Z" || m.NextKeyFingerprint != vectorOtherFP {
		t.Fatalf("unexpected fields: %+v", m)
	}
	name, a, err := m.AssetFor("shell", "linux", "amd64")
	if err != nil || name != "anet-shell-linux-amd64" || a.GzSize != 9100000 {
		t.Fatalf("AssetFor: %q %+v %v", name, a, err)
	}
	if _, _, err := m.AssetFor("default", "darwin", "arm64"); err == nil {
		t.Error("a platform the manifest does not list was found")
	}
	if _, _, err := m.AssetFor("nope", "linux", "amd64"); err == nil {
		t.Error("an unknown variant was found")
	}
}

// key_fingerprint must name the key that actually signed.
func TestKeyFingerprintMustMatchTheSigner(t *testing.T) {
	pub, priv := newKey(t)
	raw := readVector(t, "release.json") // names the vector signer, not pub
	_, _, err := VerifyManifest(raw, signSSHSIG(priv, raw, Namespace), Trust{Keys: []ed25519.PublicKey{pub}})
	if err == nil || !strings.Contains(err.Error(), "key_fingerprint") {
		t.Fatalf("a manifest naming another key verified: %v", err)
	}
}

func TestManifestValidation(t *testing.T) {
	base := readVector(t, "release.json")
	if _, err := ParseManifest(base); err != nil {
		t.Fatal(err)
	}
	type obj = map[string]any
	edit := func(f func(m obj)) []byte {
		var m obj
		if err := json.Unmarshal(base, &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		b, _ := json.Marshal(m)
		return b
	}
	asset := func(m obj) obj { return m["assets"].(obj)["anet-linux-amd64"].(obj) }
	cases := map[string][]byte{
		"other schema":         edit(func(m obj) { m["schema"] = "anet-release/2" }),
		"version not x.y.z":    edit(func(m obj) { m["version"] = "v0.2" }),
		"short commit":         edit(func(m obj) { m["commit"] = "6295665" }),
		"local-time timestamp": edit(func(m obj) { m["expires_at"] = "2026-12-26T17:00:00+08:00" }),
		"expires before":       edit(func(m obj) { m["expires_at"] = m["released_at"] }),
		"bad fingerprint":      edit(func(m obj) { m["key_fingerprint"] = "MD5:aa" }),
		"bad next fingerprint": edit(func(m obj) { m["next_key_fingerprint"] = "x" }),
		"uppercase sha":        edit(func(m obj) { asset(m)["sha256"] = strings.Repeat("A", 64) }),
		"short gz sha":         edit(func(m obj) { asset(m)["gz_sha256"] = "11" }),
		"zero size":            edit(func(m obj) { asset(m)["size"] = 0 }),
		"unknown variant":      edit(func(m obj) { asset(m)["variant"] = "gpu" }),
		"name/platform clash":  edit(func(m obj) { asset(m)["arch"] = "arm64" }),
		"unsorted modules": edit(func(m obj) {
			m["variants"].(obj)["default"].(obj)["modules"] = []string{"x402", "cas"}
		}),
		"no assets":     edit(func(m obj) { m["assets"] = obj{} }),
		"trailing data": append(append([]byte{}, base...), []byte("{}")...),
		"not json":      []byte("<html>"),
	}
	for name, raw := range cases {
		if _, err := ParseManifest(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExpiry(t *testing.T) {
	m, err := ParseManifest(readVector(t, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Date(2026, 12, 26, 9, 0, 0, 0, time.UTC)
	if err := m.CheckFresh(exp.Add(-time.Second)); err != nil {
		t.Errorf("a second before expiry: %v", err)
	}
	for _, now := range []time.Time{exp, exp.Add(time.Hour)} {
		if err := m.CheckFresh(now); !errors.Is(err, ErrExpired) {
			t.Errorf("at %s: %v", now, err)
		}
	}
}

var versionOrder = []struct {
	a, b string
	want int
}{
	{"0.2.0", "0.1.10", 1},
	{"0.1.10", "0.1.9", 1},
	{"0.1.9", "0.1.10", -1},
	{"1.0.0", "0.99.99", 1},
	{"0.2.0", "0.2.0", 0},
	{"0.2.0-rc1", "0.2.0", -1},
	{"0.2.0", "0.2.0-rc1", 1},
	{"0.2.0-rc1", "0.2.0-rc2", -1},
	{"0.2.0-rc1", "0.1.10", 1},
	{"10.0.0", "9.0.0", 1},
}

func TestCompareVersions(t *testing.T) {
	for _, c := range versionOrder {
		got, err := CompareVersions(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("CompareVersions(%s, %s) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "1.2", "v1.2.3", "1.2.3.4", "1.2.x"} {
		if _, err := CompareVersions(bad, "1.2.3"); err == nil {
			t.Errorf("%q accepted as a version", bad)
		}
	}
}

// install.sh reads the same manifest with sed and orders versions with
// awk. Run its functions on the vector and compare with the Go reading:
// the two readers of one signed file must not disagree about what it says.
func TestInstallShReadsTheManifestLikeGo(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script := repoFile(t, "deploy/release/install.sh")
	var funcs []string
	for _, name := range []string{"die", "mf_top", "mf_block_line", "field", "vercmp"} {
		re := regexp.MustCompile(`(?ms)^` + name + `\(\)\s*\{(?:[^\n]*\}\n|[^\n]*\n.*?^\}\n)`)
		f := re.FindString(script)
		if f == "" {
			t.Fatalf("install.sh has no function %s", name)
		}
		funcs = append(funcs, f)
	}
	mfPath, _ := filepath.Abs(filepath.Join("testdata", "sshsig", "release.json"))
	run := func(body string) string {
		t.Helper()
		cmd := exec.Command(sh, "-c", "set -eu\n"+strings.Join(funcs, "\n")+"\nMF='"+mfPath+"'\n"+body)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sh: %v\n%s", err, out)
		}
		return string(out)
	}

	m, err := ParseManifest(readVector(t, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, a, _ := m.AssetFor("default", "linux", "amd64")
	got := run(`
mf_top schema; echo
mf_top version; echo
mf_top expires_at; echo
mf_top next_key_fingerprint optional; echo
l=$(mf_block_line assets anet-linux-amd64); field "$l" gz_sha256; echo; field "$l" sha256; echo; field "$l" variant; echo
v=$(mf_block_line variants default); printf '%s' "$v" | sed -n 's/.*"modules": \[\([^]]*\)\].*/\1/p' | tr -d '" '; echo
mf_block_line assets anet-darwin-arm64 >/dev/null && echo found || echo absent
`)
	want := strings.Join([]string{
		m.Schema, m.Version, m.ExpiresAt, m.NextKeyFingerprint,
		a.GzSHA256, a.SHA256, a.Variant,
		strings.Join(m.Variants["default"].Modules, ","),
		"absent",
	}, "\n") + "\n"
	if got != want {
		t.Errorf("install.sh reads the manifest as\n%s\nGo reads it as\n%s", got, want)
	}

	for _, c := range versionOrder {
		out := strings.TrimSpace(run("vercmp '" + c.a + "' '" + c.b + "'"))
		want := map[int]string{-1: "-1", 0: "0", 1: "1"}[c.want]
		if out != want {
			t.Errorf("install.sh vercmp %s %s = %s, Go says %s", c.a, c.b, out, want)
		}
	}
}
