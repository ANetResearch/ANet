package release

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// DefaultBases are the hosts a release is fetched from, in the order
// install.sh tries them. Each serves the release under DLPath. Which one
// answers does not matter to what is accepted: every file is checked
// against the signed manifest.
var DefaultBases = []string{"https://agentnetwork.org.cn", "https://hub.agentnetwork.org.cn"}

// DLPath is where a base serves release.json, its signature and the assets.
const DLPath = "/dl"

// Updater finds, verifies and installs a release over one binary.
type Updater struct {
	Bases   []string     // https only; see DefaultBases
	Client  *http.Client // nil: a client that follows redirects to https only
	Trust   Trust
	Now     func() time.Time
	Current string // the running version
	Variant string // "default" or "shell"
	GOOS    string
	GOARCH  string
	Exe     string // the file Apply replaces; symlinks already resolved
	// Probe runs a candidate binary and reports what it says it is. nil
	// runs `<path> version`.
	Probe func(ctx context.Context, path string) (version string, modules []string, err error)
}

// Found is a verified release that Apply can install.
type Found struct {
	Base      string
	Manifest  *Manifest
	Signer    string // fingerprint of the key that signed the manifest
	AssetName string
	Asset     Asset
	Modules   []string // the module set the chosen variant must report
	Cmp       int      // CompareVersions(Manifest.Version, Current)
	// Raw and Sig are release.json and its signature as verified, for the
	// record SaveInstalled keeps beside the installed binary.
	Raw, Sig []byte
}

// Check fetches and verifies the manifest. It returns a Found whether the
// release is newer, the same or older than Current; Found.Cmp says which,
// and Apply refuses an older one.
//
// Bases are tried in order, but only to find one that SERVES a manifest —
// a 404, or an HTML page where a manifest should be, moves on to the next.
// A manifest that is served and then fails verification stops the search:
// a mirror serving a bad signature is something to report, not route
// around.
func (u *Updater) Check(ctx context.Context) (*Found, error) {
	bases := u.Bases
	if len(bases) == 0 {
		bases = DefaultBases
	}
	var tried []string
	for _, b := range bases {
		base, err := normalizeBase(b)
		if err != nil {
			return nil, err
		}
		raw, err := u.get(ctx, base+DLPath+"/release.json", MaxManifestBytes)
		if err == nil && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			err = errors.New("not a manifest")
		}
		var sig []byte
		if err == nil {
			sig, err = u.get(ctx, base+DLPath+"/release.json.sig", MaxSigBytes)
			if err == nil && !bytes.HasPrefix(bytes.TrimSpace(sig), []byte(sshsigBegin)) {
				err = errors.New("not a signature")
			}
		}
		if err != nil {
			tried = append(tried, base+": "+err.Error())
			continue
		}
		return u.verify(base, raw, sig)
	}
	return nil, fmt.Errorf("no release manifest found (%s)", strings.Join(tried, "; "))
}

func (u *Updater) verify(base string, raw, sig []byte) (*Found, error) {
	m, s, err := VerifyManifest(raw, sig, u.Trust)
	if err != nil {
		return nil, fmt.Errorf("%s%s/release.json: %w", base, DLPath, err)
	}
	if err := m.CheckFresh(u.now()); err != nil {
		return nil, err
	}
	name, a, err := m.AssetFor(u.Variant, u.GOOS, u.GOARCH)
	if err != nil {
		return nil, err
	}
	cmp, err := CompareVersions(m.Version, u.Current)
	if err != nil {
		return nil, fmt.Errorf("comparing with the installed version: %w", err)
	}
	return &Found{
		Base: base, Manifest: m, Signer: Fingerprint(s.PublicKey),
		AssetName: name, Asset: a, Modules: m.Variants[u.Variant].Modules, Cmp: cmp,
		Raw: raw, Sig: sig,
	}, nil
}

// Apply downloads f's asset, checks it against the manifest, and replaces
// u.Exe with it.
//
// The replacement is a rename of a fully written, fully checked file in
// the same directory, so the old binary is either untouched or replaced
// whole: every failure before the rename removes the temporary file and
// leaves u.Exe as it was.
func (u *Updater) Apply(ctx context.Context, f *Found) (err error) {
	if f.Cmp < 0 {
		return fmt.Errorf("%w: %s < %s", ErrDowngrade, f.Manifest.Version, u.Current)
	}
	gz, err := u.get(ctx, f.Base+DLPath+"/"+f.AssetName+".gz", f.Asset.GzSize)
	if err != nil {
		return fmt.Errorf("download %s.gz: %w", f.AssetName, err)
	}
	if int64(len(gz)) != f.Asset.GzSize {
		return fmt.Errorf("%s.gz: %d bytes, the manifest says %d", f.AssetName, len(gz), f.Asset.GzSize)
	}
	if got := sha256hex(gz); got != f.Asset.GzSHA256 {
		return fmt.Errorf("%s.gz: sha256 %s, the manifest says %s", f.AssetName, got, f.Asset.GzSHA256)
	}

	mode := os.FileMode(0o755)
	if st, err := os.Stat(u.Exe); err == nil {
		mode = st.Mode().Perm() | 0o111
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.Exe), ".anet-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s (%w); run anet update as the user that can", u.Exe, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return fmt.Errorf("%s.gz: %w", f.AssetName, err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(zr, f.Asset.Size+1))
	if err != nil {
		return fmt.Errorf("%s.gz: %w", f.AssetName, err)
	}
	if n != f.Asset.Size {
		return fmt.Errorf("%s: %d bytes, the manifest says %d", f.AssetName, n, f.Asset.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.Asset.SHA256 {
		return fmt.Errorf("%s: sha256 %s, the manifest says %s", f.AssetName, got, f.Asset.SHA256)
	}
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}

	// The binary is the one the manifest names; ask it what it is anyway.
	// The module set is the claim a variant's name makes, and this is the
	// same check install.sh runs before it installs.
	probe := u.Probe
	if probe == nil {
		probe = ProbeBinary
	}
	ver, mods, err := probe(ctx, tmpName)
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run here: %w", err)
	}
	if ver != f.Manifest.Version {
		return fmt.Errorf("the downloaded binary reports version %s, the manifest says %s", ver, f.Manifest.Version)
	}
	if !sameModules(mods, f.Modules) {
		return fmt.Errorf("the downloaded binary reports modules %q, the manifest says %q for the %s variant",
			strings.Join(mods, ","), strings.Join(f.Modules, ","), u.Variant)
	}
	if err = os.Rename(tmpName, u.Exe); err != nil {
		return fmt.Errorf("replace %s: %w", u.Exe, err)
	}
	// The rename is durable once the directory entry is; without this a
	// crash right after could come back with the old name pointing nowhere
	// on some filesystems. Best effort: not every platform can fsync a
	// directory, and the rename has already happened.
	if d, derr := os.Open(filepath.Dir(u.Exe)); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// ProbeBinary runs `<path> version` and reads the version and the
// `modules:` line from it.
func ProbeBinary(ctx context.Context, path string) (string, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", nil, err
	}
	return ParseVersionOutput(string(out))
}

// ParseVersionOutput reads what `anet version` prints:
//
//	anet 0.2.0 (commit 1a2b3c4, built 2026-09-27T08:00:00Z)
//	modules: anetlink,blackboard,…
//
// "(none)" on the modules line is a kernel-only build, an empty set.
func ParseVersionOutput(out string) (string, []string, error) {
	var ver string
	var mods []string
	haveMods := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "anet "); ok && ver == "" {
			ver, _, _ = strings.Cut(rest, " ")
		}
		if rest, ok := strings.CutPrefix(line, "modules: "); ok {
			haveMods = true
			if rest != "(none)" && rest != "" {
				mods = strings.Split(rest, ",")
			}
		}
	}
	if ver == "" || !haveMods {
		return "", nil, fmt.Errorf("unexpected `version` output: %q", out)
	}
	return ver, mods, nil
}

func sameModules(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) client() *http.Client {
	c := http.Client{Timeout: 10 * time.Minute}
	if u.Client != nil {
		c = *u.Client
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing a redirect to %s: releases are fetched over https only", req.URL.Redacted())
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &c
}

// get fetches a URL, reading at most limit bytes; more is an error, not a
// truncation, because nothing fetched here is valid at any other length.
func (u *Updater) get(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("more than %d bytes", limit)
	}
	return b, nil
}

// normalizeBase accepts an https URL with no query or fragment and
// returns it without a trailing slash. Credentials in the URL are refused
// rather than carried: the base is printed (`anet update` shows where the
// manifest came from) and repeated in every error, and nothing here is
// secret — the release is public and its trust is the signature.
func normalizeBase(b string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(b))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("download base %q: must be an https:// URL", redactBase(b))
	}
	if u.User != nil {
		return "", fmt.Errorf("download base %s: credentials in the URL are not accepted", u.Redacted())
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// redactBase is b with any password masked, for an error about a string
// that did not parse as the URL it was meant to be.
func redactBase(b string) string {
	if u, err := url.Parse(strings.TrimSpace(b)); err == nil {
		return u.Redacted()
	}
	return "(unparseable URL)"
}

func sha256hex(b []byte) string {
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}
