// Package release verifies a signed anet release and installs it over the
// running binary — the Go half of A2A-DESIGN §13.2, whose shell half is
// deploy/release/install.sh.
//
// A release is a manifest, release.json, and its signature,
// release.json.sig, made by the release key with `ssh-keygen -Y sign -n
// anet-release@agentnetwork.org.cn`. The manifest names every asset by the
// sha256 of its .gz and of the binary inside, and the module set each
// variant must report. Nothing else is trusted: not the host serving the
// files, not TLS, not the file names. A host that can replace the binaries
// cannot make this package install them, and a host that serves an older
// signed release, or one whose validity has run out, is refused rather than
// followed.
//
// This package depends on the standard library only. The trust root is the
// allowed_signers file compiled into the binary (keys.go).
package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schema is the manifest format this package reads. A manifest naming
// another is refused: a reader that guesses at a newer format has
// stopped verifying what the signer meant.
const Schema = "anet-release/1"

// Size ceilings for what is fetched before it is verified. The manifest
// and its signature are small; anything bigger is not them.
const (
	MaxManifestBytes = 1 << 20
	MaxSigBytes      = 16 << 10
)

// timeLayout is the one timestamp form the manifest uses: UTC, seconds,
// "Z". install.sh compares these as strings, which is only correct if
// every one of them has exactly this shape — so the shape is enforced
// here too, not just produced by build-release.sh.
const timeLayout = "2006-01-02T15:04:05Z"

// Manifest is release.json.
type Manifest struct {
	Schema     string `json:"schema"`
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	BuiltAt    string `json:"built_at"`    // the commit time; stamped into every binary as BuiltAt
	ReleasedAt string `json:"released_at"` // when the manifest was signed
	ExpiresAt  string `json:"expires_at"`  // after this, no installer or updater accepts it
	Go         string `json:"go"`
	// KeyFingerprint names the key that signed this manifest. It is
	// checked against the key inside the signature, so it cannot say
	// something the signature does not.
	KeyFingerprint string `json:"key_fingerprint"`
	// NextKeyFingerprint is the pre-committed next release key; see
	// NextKeyFingerprint in keys.go.
	NextKeyFingerprint string             `json:"next_key_fingerprint"`
	Variants           map[string]Variant `json:"variants"`
	Assets             map[string]Asset   `json:"assets"`
}

// Variant is one build flavour. The module list is what `anet version`
// prints on its `modules:` line for that build, sorted — the property an
// installer checks on the binary it is about to install.
type Variant struct {
	AssetPrefix string   `json:"asset_prefix"`
	Tags        string   `json:"tags"`
	Modules     []string `json:"modules"`
}

// Asset is one downloadable binary: <name>.gz under the download path.
type Asset struct {
	Variant  string `json:"variant"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	GzSHA256 string `json:"gz_sha256"`
	GzSize   int64  `json:"gz_size"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}

var (
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	commitRe  = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fpRe      = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	nameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	moduleRe  = regexp.MustCompile(`^[a-z0-9_]+$`)
)

// ErrExpired, ErrDowngrade: the two refusals a correctly signed manifest
// can still earn. Callers distinguish them from signature failures because
// the advice differs — a clock to check, or a mirror serving an old release.
var (
	ErrExpired   = errors.New("release manifest has expired")
	ErrDowngrade = errors.New("release manifest is older than the installed version")
)

// VerifyManifest checks sig over raw with trust and parses the result.
// The order matters: nothing in raw is looked at until the signature over
// it has verified.
func VerifyManifest(raw, sig []byte, trust Trust) (*Manifest, *Signature, error) {
	s, err := trust.Verify(raw, sig)
	if err != nil {
		return nil, nil, fmt.Errorf("release.json.sig: %w", err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return nil, nil, err
	}
	if fp := Fingerprint(s.PublicKey); m.KeyFingerprint != fp {
		return nil, nil, fmt.Errorf("release.json: key_fingerprint %s, but it is signed by %s", m.KeyFingerprint, fp)
	}
	return m, s, nil
}

// ParseManifest decodes and validates release.json. It does not verify the
// signature; VerifyManifest does both, in the right order.
func ParseManifest(raw []byte) (*Manifest, error) {
	if len(raw) > MaxManifestBytes {
		return nil, fmt.Errorf("release.json: %d bytes, more than a manifest can be", len(raw))
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("release.json: %w", err)
	}
	if dec.More() {
		return nil, errors.New("release.json: trailing data after the manifest")
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("release.json: %w", err)
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Schema != Schema {
		return fmt.Errorf("schema %q, this anet reads %q", m.Schema, Schema)
	}
	if !versionRe.MatchString(m.Version) {
		return fmt.Errorf("version %q is not x.y.z", m.Version)
	}
	if !commitRe.MatchString(m.Commit) {
		return fmt.Errorf("commit %q is not a full commit id", m.Commit)
	}
	var times [3]time.Time
	for i, f := range []struct{ name, v string }{
		{"built_at", m.BuiltAt}, {"released_at", m.ReleasedAt}, {"expires_at", m.ExpiresAt},
	} {
		t, err := time.Parse(timeLayout, f.v)
		if err != nil {
			return fmt.Errorf("%s %q is not YYYY-MM-DDTHH:MM:SSZ", f.name, f.v)
		}
		times[i] = t
	}
	if !times[2].After(times[1]) {
		return errors.New("expires_at is not after released_at")
	}
	if !fpRe.MatchString(m.KeyFingerprint) {
		return fmt.Errorf("key_fingerprint %q is not SHA256:<base64>", m.KeyFingerprint)
	}
	if m.NextKeyFingerprint != "" && !fpRe.MatchString(m.NextKeyFingerprint) {
		return fmt.Errorf("next_key_fingerprint %q is not SHA256:<base64>", m.NextKeyFingerprint)
	}
	if len(m.Variants) == 0 || len(m.Assets) == 0 {
		return errors.New("no variants or no assets")
	}
	for name, v := range m.Variants {
		if !nameRe.MatchString(name) || !nameRe.MatchString(v.AssetPrefix) {
			return fmt.Errorf("variant %q: bad name or asset_prefix", name)
		}
		if !sort.StringsAreSorted(v.Modules) {
			return fmt.Errorf("variant %q: modules are not sorted", name)
		}
		for _, mod := range v.Modules {
			if !moduleRe.MatchString(mod) {
				return fmt.Errorf("variant %q: bad module name %q", name, mod)
			}
		}
	}
	for name, a := range m.Assets {
		v, ok := m.Variants[a.Variant]
		if !ok {
			return fmt.Errorf("asset %q: unknown variant %q", name, a.Variant)
		}
		if want := v.AssetPrefix + "-" + a.OS + "-" + a.Arch; name != want {
			return fmt.Errorf("asset %q: name does not match %s", name, want)
		}
		if !nameRe.MatchString(a.OS) || !nameRe.MatchString(a.Arch) {
			return fmt.Errorf("asset %q: bad os or arch", name)
		}
		if !sha256Re.MatchString(a.GzSHA256) || !sha256Re.MatchString(a.SHA256) {
			return fmt.Errorf("asset %q: sha256 is not 64 lowercase hex digits", name)
		}
		if a.GzSize <= 0 || a.Size <= 0 {
			return fmt.Errorf("asset %q: sizes must be positive", name)
		}
	}
	return nil
}

// Expires returns expires_at.
func (m *Manifest) Expires() time.Time {
	t, _ := time.Parse(timeLayout, m.ExpiresAt)
	return t
}

// CheckFresh refuses a manifest whose validity has run out. Expiry is what
// bounds a freeze: without it, a mirror could serve the last release it
// saw, correctly signed, for ever after a fix has shipped.
func (m *Manifest) CheckFresh(now time.Time) error {
	if !now.Before(m.Expires()) {
		return fmt.Errorf("%w: it expired at %s and it is now %s (if this clock is wrong, fix it; otherwise the mirror is serving a stale release)",
			ErrExpired, m.ExpiresAt, now.UTC().Format(timeLayout))
	}
	return nil
}

// AssetFor picks the asset for one variant and platform.
func (m *Manifest) AssetFor(variant, goos, goarch string) (string, Asset, error) {
	v, ok := m.Variants[variant]
	if !ok {
		return "", Asset{}, fmt.Errorf("release %s has no %q variant", m.Version, variant)
	}
	name := v.AssetPrefix + "-" + goos + "-" + goarch
	a, ok := m.Assets[name]
	if !ok || a.Variant != variant {
		return "", Asset{}, fmt.Errorf("release %s has no %s build for %s/%s", m.Version, variant, goos, goarch)
	}
	return name, a, nil
}

// CompareVersions orders two x.y.z[-pre] versions: -1, 0 or 1. A
// pre-release sorts before its release; two pre-releases of the same
// x.y.z compare as strings. install.sh implements the same order in awk.
func CompareVersions(a, b string) (int, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := 0; i < 3; i++ {
		if pa.n[i] != pb.n[i] {
			if pa.n[i] < pb.n[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0, nil
	case pa.pre == "":
		return 1, nil
	case pb.pre == "":
		return -1, nil
	case pa.pre < pb.pre:
		return -1, nil
	default:
		return 1, nil
	}
}

type version struct {
	n   [3]uint64
	pre string
}

func parseVersion(s string) (version, error) {
	var v version
	if !versionRe.MatchString(s) {
		return v, fmt.Errorf("version %q is not x.y.z", s)
	}
	core, pre, _ := strings.Cut(s, "-")
	v.pre = pre
	for i, p := range strings.Split(core, ".") {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return v, fmt.Errorf("version %q: %w", s, err)
		}
		v.n[i] = n
	}
	return v, nil
}
