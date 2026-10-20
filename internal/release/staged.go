package release

// staged.go is `anet update --verify-dir`: the check a binary makes of
// itself against a release directory, before an installer that cannot
// verify an SSHSIG signature (install.ps1 on Windows, whose platform has
// neither an Ed25519 API nor a recent enough ssh-keygen) installs it.
//
// The installer has already checked the binary against SHA-256 values
// embedded in the signed installer script. What it cannot check, the binary
// checks here with the release key compiled into it, using the same
// verifier as `anet update` and `anet doctor`: the signature over
// release.json, its freshness, that the manifest names this very file (by
// sha256 and size) as the build of this variant for this platform, that the
// version and module set the binary reports are the manifest's, and the
// downgrade rule. A binary that is not the one the manifest names cannot
// vouch for anything, and fails the sha256 comparison.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Self is what the running binary is, as VerifyStaged compares it with a
// manifest.
type Self struct {
	Exe     string   // the file the running binary was started from
	Version string   // version.V
	Variant string   // version.Variant: "default", "shell", or "" for a source build
	GOOS    string   // runtime.GOOS
	GOARCH  string   // runtime.GOARCH
	Modules []string // module.Compiled(), sorted
}

// Verified is what VerifyStaged vouches for, printed by `anet update
// --verify-dir` as one JSON line.
type Verified struct {
	Version        string   `json:"version"`
	Commit         string   `json:"commit"`
	ExpiresAt      string   `json:"expires_at"`
	KeyFingerprint string   `json:"key_fingerprint"`
	Variant        string   `json:"variant"`
	Asset          string   `json:"asset"`
	Modules        []string `json:"modules"`
}

// VerifyStaged checks the release in dir (release.json and release.json.sig)
// against trust at time now, and self against the release. installed is the
// version of the anet the release would replace, "" when there is none; a
// release older than it is refused (ErrDowngrade). It reads; it writes
// nothing and fetches nothing.
func VerifyStaged(dir string, trust Trust, now time.Time, self Self, installed string) (*Verified, error) {
	raw, err := readLimited(filepath.Join(dir, "release.json"), MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	sig, err := readLimited(filepath.Join(dir, "release.json.sig"), MaxSigBytes)
	if err != nil {
		return nil, err
	}
	m, s, err := VerifyManifest(raw, sig, trust)
	if err != nil {
		return nil, err
	}
	if err := m.CheckFresh(now); err != nil {
		return nil, err
	}
	if self.Variant == "" {
		return nil, errors.New("this binary carries no release variant (a build from source): it is not a build of any release")
	}
	name, a, err := m.AssetFor(self.Variant, self.GOOS, self.GOARCH)
	if err != nil {
		return nil, err
	}
	mods := m.Variants[self.Variant].Modules
	if self.Variant == "default" && slices.Contains(mods, "shell") {
		return nil, errors.New("release.json lists the shell module in the default variant; refusing it")
	}
	sum, size, err := fileDigest(self.Exe)
	if err != nil {
		return nil, fmt.Errorf("cannot read this binary to compare it with the release: %w", err)
	}
	if sum != a.SHA256 || size != a.Size {
		return nil, fmt.Errorf("this binary (%s, sha256 %s, %d bytes) is not %s of release %s (sha256 %s, %d bytes)",
			self.Exe, sum, size, name, m.Version, a.SHA256, a.Size)
	}
	if self.Version != m.Version {
		return nil, fmt.Errorf("this binary reports version %s, the manifest says %s", self.Version, m.Version)
	}
	if !sameModules(self.Modules, mods) {
		return nil, fmt.Errorf("this binary reports modules %q, the manifest says %q for the %s variant",
			strings.Join(self.Modules, ","), strings.Join(mods, ","), self.Variant)
	}
	if installed != "" {
		cmp, err := CompareVersions(m.Version, installed)
		if err != nil {
			return nil, fmt.Errorf("the installed version: %w", err)
		}
		if cmp < 0 {
			return nil, fmt.Errorf("%w: release %s, installed %s", ErrDowngrade, m.Version, installed)
		}
	}
	return &Verified{
		Version:        m.Version,
		Commit:         m.Commit,
		ExpiresAt:      m.ExpiresAt,
		KeyFingerprint: Fingerprint(s.PublicKey),
		Variant:        self.Variant,
		Asset:          name,
		Modules:        append([]string{}, mods...),
	}, nil
}

// fileDigest is the sha256 and the length of the file at path, read once.
func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
