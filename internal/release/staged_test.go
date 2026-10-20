package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stagedRelease is a release directory as install.ps1 lays it out for
// `anet update --verify-dir`, and the staged binary beside it, signed by a
// key made for the test.
type stagedRelease struct {
	pub     ed25519.PublicKey
	priv    ed25519.PrivateKey
	dir     string
	exe     string
	bin     []byte
	version string
	modules map[string]string // variant → JSON list body
	expires string
}

func newStagedRelease(t *testing.T) *stagedRelease {
	t.Helper()
	pub, priv := newKey(t)
	r := &stagedRelease{pub: pub, priv: priv, dir: t.TempDir(), version: "0.4.0",
		bin:     []byte("MZ a stand-in for the staged binary\x00\x01\x02"),
		modules: map[string]string{"default": `"cas", "mcp", "x402"`, "shell": `"cas", "mcp", "shell", "x402"`},
		expires: "2026-12-26T09:00:00Z"}
	r.exe = filepath.Join(t.TempDir(), "anet.exe.new")
	if err := os.WriteFile(r.exe, r.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	r.write(t, nil)
	return r
}

// write renders release.json for the binary r.bin as the shell build for
// windows/amd64 (and a default build with other bytes), lets edit change
// the asset fields before signing, and signs it.
func (r *stagedRelease) write(t *testing.T, edit func(sha *string, size *int64)) {
	t.Helper()
	sha, size := hexsum(r.bin), int64(len(r.bin))
	if edit != nil {
		edit(&sha, &size)
	}
	other := hexsum([]byte("the default build"))
	manifest := fmt.Sprintf(`{
  "schema": "anet-release/1",
  "version": "%s",
  "commit": "6295665a1b2c3d4e5f60718293a4b5c6d7e8f901",
  "built_at": "2026-09-27T08:33:44Z",
  "released_at": "2026-09-27T09:00:00Z",
  "expires_at": "%s",
  "go": "go1.26.6",
  "key_fingerprint": "%s",
  "next_key_fingerprint": "",
  "variants": {
    "default": {"asset_prefix": "anet", "tags": "", "modules": [%s]},
    "shell": {"asset_prefix": "anet-shell", "tags": "shell", "modules": [%s]}
  },
  "assets": {
    "anet-windows-amd64": {"variant": "default", "os": "windows", "arch": "amd64", "gz_sha256": "%s", "gz_size": 10, "sha256": "%s", "size": 17},
    "anet-shell-windows-amd64": {"variant": "shell", "os": "windows", "arch": "amd64", "gz_sha256": "%s", "gz_size": 10, "sha256": "%s", "size": %d}
  }
}
`, r.version, r.expires, Fingerprint(r.pub), r.modules["default"], r.modules["shell"], other, other, other, sha, size)
	r.writeFiles(t, []byte(manifest), signSSHSIG(r.priv, []byte(manifest), Namespace))
}

func (r *stagedRelease) writeFiles(t *testing.T, manifest, sig []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "release.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "release.json.sig"), sig, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *stagedRelease) self() Self {
	return Self{Exe: r.exe, Version: r.version, Variant: "shell", GOOS: "windows", GOARCH: "amd64",
		Modules: []string{"cas", "mcp", "shell", "x402"}}
}

func (r *stagedRelease) trust() Trust { return Trust{Keys: []ed25519.PublicKey{r.pub}} }

var stagedNow = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func TestVerifyStagedAcceptsTheReleaseBinary(t *testing.T) {
	r := newStagedRelease(t)
	for _, installed := range []string{"", "0.3.3", "0.4.0", "0.4.0-rc.1"} {
		v, err := VerifyStaged(r.dir, r.trust(), stagedNow, r.self(), installed)
		if err != nil {
			t.Fatalf("installed %q: %v", installed, err)
		}
		want := Verified{Version: "0.4.0", Commit: "6295665a1b2c3d4e5f60718293a4b5c6d7e8f901",
			ExpiresAt: "2026-12-26T09:00:00Z", KeyFingerprint: Fingerprint(r.pub), Variant: "shell",
			Asset: "anet-shell-windows-amd64", Modules: []string{"cas", "mcp", "shell", "x402"}}
		if fmt.Sprint(*v) != fmt.Sprint(want) {
			t.Fatalf("installed %q: %+v, want %+v", installed, *v, want)
		}
	}
	// The JSON line install.ps1 reads: exactly these keys, modules a list.
	v, _ := VerifyStaged(r.dir, r.trust(), stagedNow, r.self(), "")
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"version":"0.4.0","commit":"6295665a1b2c3d4e5f60718293a4b5c6d7e8f901","expires_at":"2026-12-26T09:00:00Z",` +
		`"key_fingerprint":"%s","variant":"shell","asset":"anet-shell-windows-amd64","modules":["cas","mcp","shell","x402"]}`
	if string(b) != fmt.Sprintf(want, Fingerprint(r.pub)) {
		t.Fatalf("JSON %s", b)
	}
	// A kernel-only variant prints an empty list, not null.
	r.modules["shell"] = ""
	r.write(t, nil)
	s := r.self()
	s.Modules = nil
	v, err = VerifyStaged(r.dir, r.trust(), stagedNow, s, "")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(v); !bytes.Contains(b, []byte(`"modules":[]`)) {
		t.Fatalf("empty module set: %s", b)
	}
}

func TestVerifyStagedRefuses(t *testing.T) {
	cases := []struct {
		name      string
		spoil     func(t *testing.T, r *stagedRelease, s *Self, installed *string, trust *Trust)
		want      string
		wantError error
	}{
		{"bad signature", func(t *testing.T, r *stagedRelease, _ *Self, _ *string, _ *Trust) {
			raw, _ := os.ReadFile(filepath.Join(r.dir, "release.json"))
			r.writeFiles(t, raw, signSSHSIG(r.priv, append(raw, ' '), Namespace))
		}, "release.json.sig", nil},
		{"another key", func(t *testing.T, r *stagedRelease, _ *Self, _ *string, trust *Trust) {
			other, _ := newKey(t)
			*trust = Trust{Keys: []ed25519.PublicKey{other}}
		}, "not the release key", nil},
		{"another namespace", func(t *testing.T, r *stagedRelease, _ *Self, _ *string, _ *Trust) {
			raw, _ := os.ReadFile(filepath.Join(r.dir, "release.json"))
			r.writeFiles(t, raw, signSSHSIG(r.priv, raw, OfficialNamespace))
		}, "namespace", nil},
		{"no signature", func(t *testing.T, r *stagedRelease, _ *Self, _ *string, _ *Trust) {
			os.Remove(filepath.Join(r.dir, "release.json.sig"))
		}, "release.json.sig", nil},
		{"expired", func(t *testing.T, r *stagedRelease, _ *Self, _ *string, _ *Trust) {
			r.expires = "2026-10-01T00:00:00Z"
			r.write(t, nil)
		}, "", ErrExpired},
		{"wrong sha256", func(t *testing.T, r *stagedRelease, _ *Self, _ *string, _ *Trust) {
			r.write(t, func(sha *string, _ *int64) { *sha = strings.Repeat("0", 64) })
		}, "is not anet-shell-windows-amd64 of release 0.4.0", nil},
		{"wrong size", func(t *testing.T, r *stagedRelease, _ *Self, _ *string, _ *Trust) {
			r.write(t, func(_ *string, size *int64) { *size++ })
		}, "is not anet-shell-windows-amd64", nil},
		{"another binary", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			s.Exe = filepath.Join(t.TempDir(), "other.exe")
			os.WriteFile(s.Exe, []byte("MZ something else"), 0o755)
		}, "is not anet-shell-windows-amd64", nil},
		{"binary missing", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			s.Exe = filepath.Join(t.TempDir(), "absent.exe")
		}, "cannot read this binary", nil},
		{"wrong modules", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			s.Modules = []string{"cas", "mcp", "x402"}
		}, "reports modules", nil},
		{"an extra module", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			s.Modules = append(s.Modules, "zzz")
		}, "reports modules", nil},
		{"wrong version", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			s.Version = "0.3.9"
		}, "reports version 0.3.9", nil},
		{"downgrade", func(t *testing.T, r *stagedRelease, _ *Self, installed *string, _ *Trust) {
			*installed = "0.4.1"
		}, "", ErrDowngrade},
		{"downgrade from a pre-release", func(t *testing.T, r *stagedRelease, s *Self, installed *string, _ *Trust) {
			r.version = "0.4.0-rc.1"
			r.write(t, nil)
			s.Version = r.version
			*installed = "0.4.0"
		}, "", ErrDowngrade},
		{"installed version unreadable", func(t *testing.T, r *stagedRelease, _ *Self, installed *string, _ *Trust) {
			*installed = "latest"
		}, "the installed version", nil},
		{"source build", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			s.Variant = ""
		}, "no release variant", nil},
		{"no build for this platform", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			s.GOARCH = "arm64"
		}, "no shell build for windows/arm64", nil},
		{"no such variant", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			s.Variant = "nosuch"
		}, "", ErrNoVariant},
		{"shell in the default variant", func(t *testing.T, r *stagedRelease, s *Self, _ *string, _ *Trust) {
			r.modules["default"] = `"cas", "shell"`
			r.write(t, nil)
			s.Variant = "default"
		}, "shell module in the default variant", nil},
		{"manifest too large", func(t *testing.T, r *stagedRelease, _ *Self, _ *string, _ *Trust) {
			r.writeFiles(t, bytes.Repeat([]byte(" "), MaxManifestBytes+1), []byte("x"))
		}, "larger than", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newStagedRelease(t)
			s, installed, trust := r.self(), "", r.trust()
			c.spoil(t, r, &s, &installed, &trust)
			v, err := VerifyStaged(r.dir, trust, stagedNow, s, installed)
			if err == nil {
				t.Fatalf("accepted: %+v", v)
			}
			if c.wantError != nil && !errors.Is(err, c.wantError) {
				t.Fatalf("error %v, want %v", err, c.wantError)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not say %q", err, c.want)
			}
		})
	}
}
