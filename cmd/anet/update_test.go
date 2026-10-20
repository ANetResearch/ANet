package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/release"
	"github.com/ANetResearch/ANet/internal/version"
	"github.com/ANetResearch/ANet/module"
)

// verifyDirRelease writes a release directory that names this test binary
// as the build of its variant for this platform, signed by a key made for
// the test, and makes --verify-dir trust that key. edit may change the
// manifest text before it is signed.
func verifyDirRelease(t *testing.T, edit func(string) string) (dir string, pub ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	prevTrust, prevVariant := verifyTrust, version.Variant
	t.Cleanup(func() { verifyTrust, version.Variant = prevTrust, prevVariant })
	verifyTrust = func() release.Trust { return release.Trust{Keys: []ed25519.PublicKey{pub}} }

	mods := module.Compiled()
	variant, prefix := "default", "anet"
	if slices.Contains(mods, "shell") {
		variant, prefix = "shell", "anet-shell"
	}
	version.Variant = variant
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	quoted := make([]string, len(mods))
	for i, m := range mods {
		quoted[i] = `"` + m + `"`
	}
	asset := prefix + "-" + runtime.GOOS + "-" + runtime.GOARCH
	manifest := fmt.Sprintf(`{
  "schema": "anet-release/1",
  "version": "%s",
  "commit": "6295665a1b2c3d4e5f60718293a4b5c6d7e8f901",
  "built_at": "2026-09-27T08:33:44Z",
  "released_at": "%s",
  "expires_at": "%s",
  "go": "go1.26.6",
  "key_fingerprint": "%s",
  "next_key_fingerprint": "",
  "variants": {
    "%s": {"asset_prefix": "%s", "tags": "", "modules": [%s]}
  },
  "assets": {
    "%s": {"variant": "%s", "os": "%s", "arch": "%s", "gz_sha256": "%s", "gz_size": 100, "sha256": "%s", "size": %d}
  }
}
`, version.V, time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05Z"),
		time.Now().Add(24*time.Hour).UTC().Format("2006-01-02T15:04:05Z"), release.Fingerprint(pub),
		variant, prefix, strings.Join(quoted, ", "),
		asset, variant, runtime.GOOS, runtime.GOARCH, strings.Repeat("0", 64), hex.EncodeToString(sum[:]), len(bin))
	if edit != nil {
		manifest = edit(manifest)
	}
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	sig := release.Sign(priv, []byte(manifest), release.Namespace)
	if err := os.WriteFile(filepath.Join(dir, "release.json.sig"), sig, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, pub
}

// install.ps1 reads exactly one JSON line on stdout, with these keys.
func TestUpdateVerifyDirPrintsOneJSONLine(t *testing.T) {
	dir, pub := verifyDirRelease(t, nil)
	for _, args := range [][]string{
		{"--verify-dir", dir},
		{"--verify-dir=" + dir, "--installed", "0.0.1"},
		{"--installed", version.V, "--verify-dir", dir},
	} {
		var err error
		out, errOut := captureCLI(t, func() { err = runUpdate(args) })
		if err != nil || errOut != "" {
			t.Fatalf("%v: %v, stderr %q", args, err, errOut)
		}
		if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
			t.Fatalf("%v: stdout is not one line: %q", args, out)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("%v: %v: %q", args, err, out)
		}
		var keys []string
		for k := range got {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if strings.Join(keys, ",") != "asset,commit,expires_at,key_fingerprint,modules,variant,version" {
			t.Fatalf("keys %v", keys)
		}
		prefix := "anet"
		if version.Variant != "default" {
			prefix += "-" + version.Variant
		}
		if got["version"] != version.V || got["key_fingerprint"] != release.Fingerprint(pub) || got["variant"] != version.Variant ||
			got["asset"] != prefix+"-"+runtime.GOOS+"-"+runtime.GOARCH || got["commit"] != "6295665a1b2c3d4e5f60718293a4b5c6d7e8f901" {
			t.Fatalf("answer %v", got)
		}
		if _, ok := got["modules"].([]any); !ok {
			t.Fatalf("modules is not a list: %v", got["modules"])
		}
	}
}

// Every refusal is one line on stderr that begins with the mode's name,
// nothing on stdout, and status 1.
func TestUpdateVerifyDirRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(string) string
		args []string
		want string
	}{
		{"another binary", func(m string) string {
			return regexp.MustCompile(`"sha256": "[0-9a-f]{64}"`).ReplaceAllString(m, `"sha256": "`+strings.Repeat("1", 64)+`"`)
		}, nil, "is not anet"},
		{"another version", func(m string) string {
			return strings.Replace(m, `"version": "`+version.V+`"`, `"version": "`+version.V+`-rc.1"`, 1)
		}, nil, "reports version"},
		{"a downgrade", nil, []string{"--installed", "999.0.0"}, "older than the installed version"},
		{"an unreadable installed version", nil, []string{"--installed", "x"}, "installed version"},
		{"no release here", nil, []string{"--verify-dir", "/nonexistent/anet-verify-dir"}, "release.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := verifyDirRelease(t, c.edit)
			args := append([]string{"--verify-dir", dir}, c.args...)
			if len(c.args) > 0 && c.args[0] == "--verify-dir" {
				args = c.args
			}
			var err error
			out, errOut := captureCLI(t, func() { err = runUpdate(args) })
			if !errors.Is(err, errExit1) || out != "" {
				t.Fatalf("err %v, stdout %q", err, out)
			}
			if !strings.HasPrefix(errOut, "anet update --verify-dir: ") || strings.Count(errOut, "\n") != 1 || !strings.Contains(errOut, c.want) {
				t.Fatalf("stderr %q, want one line saying %q", errOut, c.want)
			}
		})
	}
	// A binary built from source carries no variant and vouches for nothing.
	dir, _ := verifyDirRelease(t, nil)
	version.Variant = ""
	var err error
	_, errOut := captureCLI(t, func() { err = runUpdate([]string{"--verify-dir", dir}) })
	if !errors.Is(err, errExit1) || !strings.Contains(errOut, "no release variant") {
		t.Fatalf("source build: %v %q", err, errOut)
	}
}

// The mode takes exactly its two flags, each with a value.
func TestUpdateVerifyDirUsage(t *testing.T) {
	for _, args := range [][]string{
		{"--verify-dir"},
		{"--verify-dir="},
		{"--installed", "0.3.3"},
		{"--verify-dir", "d", "--installed"},
		{"--verify-dir", "d", "--installed="},
		{"--verify-dir", "d", "--check"},
		{"--verify-dir", "d", "--base", "https://example.org"},
	} {
		var err error
		out, errOut := captureCLI(t, func() { err = runUpdate(args) })
		if !isUsage(err) || out != "" || errOut != "" {
			t.Errorf("%v: err %v, stdout %q, stderr %q; want a usage error", args, err, out, errOut)
		}
	}
}
