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

// DefaultBases are the project's own hosts. Each serves the release under
// DLPath; Sources tries them last. Which one answers does not matter to
// what is accepted: every file is checked against the signed manifest.
var DefaultBases = []string{"https://agentnetwork.org.cn", "https://hub.agentnetwork.org.cn"}

// DLPath is where a base serves release.json, its signature and the assets.
const DLPath = "/dl"

// ErrReplaceIncomplete is a replacement that failed half way on Windows:
// the binary was moved aside, the new one could not take its name, and the
// old one could not be renamed back. The error names both files.
var ErrReplaceIncomplete = errors.New("the binary was not replaced and is no longer in place")

// Updater finds, verifies and installs a release over one binary.
type Updater struct {
	// Sources are tried in order (see Check). When there are none, each of
	// Bases is a source (BaseSource), and DefaultBases when Bases is empty
	// too.
	Sources []Source
	Bases   []string     // https only
	Client  *http.Client // nil: a default client; redirects are followed to https only, and to hosts the source allows
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
	// Base is where the release was found: Source.Location, without
	// DLPath where it ends in it. For a source made by BaseSource it is
	// the base.
	Base      string
	Source    Source
	Manifest  *Manifest
	Signer    string // fingerprint of the key that signed the manifest
	AssetName string
	Asset     Asset
	Modules   []string // the module set the chosen variant must report
	Cmp       int      // CompareVersions(Manifest.Version, Current)
	// Raw and Sig are release.json and its signature as verified, for the
	// record SaveInstalled keeps beside the installed binary.
	Raw, Sig []byte
	// gz is the asset's .gz as unpacked from an npm tarball, which Apply
	// installs without downloading it again.
	gz []byte
}

// Check fetches and verifies the manifest. It returns a Found whether the
// release is newer, the same or older than Current; Found.Cmp says which,
// and Apply refuses an older one.
//
// Sources are tried in order, but only to find one that SERVES a release —
// an HTTP error, an HTML page where a manifest should be, a redirect to a
// host the source may not use, or a malformed tarball moves on to the
// next. A release that is served and then fails verification stops the
// search: a mirror serving a bad signature is something to report, not
// route around.
//
// A verified release older than Current is a mirror that lags behind: the
// next source is tried. When no source serves Current or a newer release,
// the first lagging one is returned, and Apply refuses it. The version in
// an npm package's metadata is compared with Current before its tarball
// is downloaded, so a lagging package is downloaded only when it is that
// fallback.
func (u *Updater) Check(ctx context.Context) (*Found, error) {
	srcs, err := u.sources()
	if err != nil {
		return nil, err
	}
	type lagged struct {
		src   Source
		meta  *npmMeta // an npm source not downloaded yet
		found *Found   // a source whose release is verified
	}
	var tried []string
	var lagging []lagged
	var ns *notServingError
	for _, s := range srcs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var meta *npmMeta
		if s.Kind == SourceNPM {
			m, err := u.npmLatest(ctx, s)
			if err != nil {
				tried = append(tried, s.Name+": "+err.Error())
				continue
			}
			if c, err := CompareVersions(m.Version, u.Current); err == nil && c < 0 {
				lagging = append(lagging, lagged{src: s, meta: m})
				tried = append(tried, s.Name+": "+m.Version+" is older than "+u.Current)
				continue
			}
			meta = m
		}
		f, err := u.checkSource(ctx, s, meta)
		switch {
		case errors.As(err, &ns):
			tried = append(tried, s.Name+": "+err.Error())
			continue
		case err != nil:
			return nil, err
		case f.Cmp < 0:
			lagging = append(lagging, lagged{src: s, found: f})
			tried = append(tried, s.Name+": "+f.Manifest.Version+" is older than "+u.Current)
			continue
		}
		return f, nil
	}
	for _, l := range lagging {
		if l.found != nil {
			return l.found, nil
		}
		f, err := u.checkSource(ctx, l.src, l.meta)
		if errors.As(err, &ns) {
			tried = append(tried, l.src.Name+": "+err.Error())
			continue
		}
		return f, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no release manifest found (%s)", strings.Join(tried, "; "))
}

// sources are the sources Check tries: Sources, or else Bases, or else
// DefaultBases.
func (u *Updater) sources() ([]Source, error) {
	if len(u.Sources) > 0 {
		return u.Sources, nil
	}
	bases := u.Bases
	if len(bases) == 0 {
		bases = DefaultBases
	}
	srcs := make([]Source, 0, len(bases))
	for _, b := range bases {
		s, err := BaseSource(b)
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, s)
	}
	return srcs, nil
}

// checkSource fetches and verifies the release s serves. An error that
// means s serves no release is a *notServingError; any other is a
// verification failure. meta is the package metadata of an npm source
// when Check has already read it.
func (u *Updater) checkSource(ctx context.Context, s Source, meta *npmMeta) (*Found, error) {
	var raw, sig []byte
	var files map[string][]byte
	var where string
	switch s.Kind {
	case SourceFiles:
		where = s.URL + "/release.json"
		var err error
		raw, err = u.get(ctx, s.Allow, where, MaxManifestBytes)
		if err == nil {
			sig, err = u.get(ctx, s.Allow, s.URL+"/release.json.sig", MaxSigBytes)
		}
		if err != nil {
			return nil, notServing(err)
		}
	case SourceNPM:
		if meta == nil {
			m, err := u.npmLatest(ctx, s)
			if err != nil {
				return nil, notServing(err)
			}
			meta = m
		}
		tarURL := s.tarballURL(meta.Version)
		var err error
		files, err = u.npmFiles(ctx, s, tarURL, meta.Dist.Integrity)
		if err != nil {
			return nil, notServing(fmt.Errorf("%s: %w", tarURL, err))
		}
		raw, sig = files["release.json"], files["release.json.sig"]
		if raw == nil || sig == nil {
			return nil, notServing(fmt.Errorf("%s: no package/dl/release.json and release.json.sig", tarURL))
		}
		where = tarURL + ": package/dl/release.json"
	default:
		return nil, fmt.Errorf("source %s: unknown kind %q", s.Name, s.Kind)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return nil, notServing(fmt.Errorf("%s: not a manifest", where))
	}
	if !bytes.HasPrefix(bytes.TrimSpace(sig), []byte(sshsigBegin)) {
		return nil, notServing(fmt.Errorf("%s.sig: not a signature", where))
	}
	f, err := u.verify(where, raw, sig)
	if err != nil {
		return nil, err
	}
	f.Source, f.Base = s, s.base()
	if s.Kind == SourceNPM {
		gz, ok := files[f.AssetName+".gz"]
		if !ok {
			return nil, notServing(fmt.Errorf("%s: no package/dl/%s.gz", s.tarballURL(meta.Version), f.AssetName))
		}
		f.gz = gz
	}
	return f, nil
}

// verify checks the manifest at where (named in errors) and its signature.
func (u *Updater) verify(where string, raw, sig []byte) (*Found, error) {
	m, s, err := VerifyManifest(raw, sig, u.Trust)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	if err := m.CheckFresh(u.now()); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
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
		Manifest: m, Signer: Fingerprint(s.PublicKey),
		AssetName: name, Asset: a, Modules: m.Variants[u.Variant].Modules, Cmp: cmp,
		Raw: raw, Sig: sig,
	}, nil
}

// Apply downloads f's asset (an npm source's was unpacked by Check), checks
// it against the manifest, and replaces u.Exe with it.
//
// The replacement is a rename of a fully written, fully checked file in
// the same directory, so the old binary is either untouched or replaced
// whole: every failure before the rename removes the temporary file and
// leaves u.Exe as it was. On Windows, where a running executable can be
// renamed but not replaced, the rename is preceded by moving u.Exe aside
// (replaceExe in replace_windows.go); a failure after that step puts it
// back, and ErrReplaceIncomplete reports the one case in which that also
// failed.
func (u *Updater) Apply(ctx context.Context, f *Found) (err error) {
	if f.Cmp < 0 {
		return fmt.Errorf("%w: %s < %s", ErrDowngrade, f.Manifest.Version, u.Current)
	}
	gz := f.gz
	switch {
	case f.Source.Kind == SourceNPM && gz == nil:
		return fmt.Errorf("%s.gz: not unpacked from the package", f.AssetName)
	case f.Source.Kind != SourceNPM:
		gz, err = u.get(ctx, f.Source.Allow, f.Source.URL+"/"+f.AssetName+".gz", f.Asset.GzSize)
		if err != nil {
			return fmt.Errorf("download %s.gz: %w", f.AssetName, err)
		}
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
	tmp, err := os.CreateTemp(filepath.Dir(u.Exe), updateTempPattern)
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
	if err = chmodTemp(tmp, mode); err != nil {
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
	if err = replaceExe(tmpName, u.Exe); err != nil {
		return fmt.Errorf("replace %s: %w", u.Exe, err)
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

// client is u.Client, following redirects to https URLs on the hosts allow
// admits (see Source.Allow) only.
func (u *Updater) client(allow []string) *http.Client {
	c := http.Client{Timeout: 10 * time.Minute}
	if u.Client != nil {
		c = *u.Client
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing a redirect to %s: releases are fetched over https only", req.URL.Redacted())
		}
		if !hostAllowed(req.URL, allow) {
			return fmt.Errorf("refusing a redirect to the host %s: not a host this source may use", req.URL.Host)
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &c
}

// open requests a URL on a host allow admits and returns the response of
// status 200; the caller closes its body.
func (u *Updater) open(ctx context.Context, allow []string, rawURL string) (*http.Response, error) {
	if err := checkURL(rawURL, allow); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := u.client(allow).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// get fetches a URL on a host allow admits, reading at most limit bytes;
// more is an error, not a truncation, because nothing fetched here is
// valid at any other length.
func (u *Updater) get(ctx context.Context, allow []string, rawURL string, limit int64) ([]byte, error) {
	resp, err := u.open(ctx, allow, rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
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
