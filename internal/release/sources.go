package release

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
)

// Source kinds.
const (
	// SourceFiles serves release.json, release.json.sig and <asset>.gz at
	// Source.URL + "/" + name, as a download base serves them under DLPath.
	SourceFiles = "files"
	// SourceNPM is an npm registry. Source.URL is the registry root and
	// Source.Package the package of the build; the directory dl/ of the
	// package's latest tarball holds the same three files.
	SourceNPM = "npm"
	// SourceCDN is a content delivery network for npm packages (jsDelivr).
	// Source.URL is the package's root there; the latest version is read
	// from URL@latest/package.json first, and the three files from
	// URL@<version>/dl/<name> after it. A file asked for under @latest is
	// resolved by the network on its own, and release.json and its
	// signature could come from two versions; under the version both come
	// from the one package.
	SourceCDN = "cdn"
)

// Source is one place a release is downloaded from.
//
// A source decides where the bytes come from, and nothing else: whatever
// it serves is verified against the signed manifest exactly as the files
// of any other source are. A source that does not serve a release (an
// HTTP error, a timeout, an HTML page where a manifest should be, a
// missing file, a redirect to a host it may not use, a malformed tarball)
// is skipped, and so is a source whose manifest does not verify
// (ErrUnverified); the update fails only when no source serves one that
// does.
type Source struct {
	Name string // how the source is shown
	Kind string // SourceFiles, SourceNPM or SourceCDN
	URL  string // https only; for SourceNPM the registry root, for SourceCDN the package's root
	// Allow is the hosts the first request and every redirect may go to:
	// an exact host ("github.com", or "127.0.0.1:43123" with a port), or
	// "*.suffix" for any subdomain of suffix (not suffix itself). A pattern
	// without a port matches the default https port only. nil allows any
	// https host.
	Allow   []string
	Package string // SourceNPM: the package name (NPMPackage)
}

// Location is where the source is read from: the directory of the files,
// the package on the registry, or the package's root on the network.
func (s Source) Location() string {
	if s.Kind == SourceNPM {
		return s.URL + "/" + s.Package
	}
	return s.URL
}

// base is the source as Found.Base reports it: the location, without
// DLPath where it ends in it, so that a source made by BaseSource reports
// the base it was made from.
func (s Source) base() string {
	if s.Kind == SourceNPM {
		return s.Location()
	}
	return strings.TrimSuffix(s.URL, DLPath)
}

// tarballURL is the registry's address of version of the package. It is
// constructed rather than taken from the package metadata, so the
// metadata cannot send the download elsewhere; registry.npmjs.org and
// registry.npmmirror.com both serve tarballs under this name.
func (s Source) tarballURL(version string) string {
	short := s.Package[strings.LastIndex(s.Package, "/")+1:]
	return s.URL + "/" + s.Package + "/-/" + short + "-" + version + ".tgz"
}

// cdnDir is the directory of a SourceCDN package's files for version.
func (s Source) cdnDir(version string) string { return s.URL + "@" + version + DLPath }

// githubDownload is the directory the assets of the latest GitHub release
// are served from.
const githubDownload = "https://github.com/ANetResearch/ANet/releases/latest/download"

// Sources are the default sources of the build of variant for goos and
// goarch, as install.sh uses them. probed are raced (OrderSources) and
// tried fastest first; rest follow in the order given. The npm sources
// and jsDelivr are left out for a build that has no npm package.
func Sources(variant, goos, goarch string) (probed, rest []Source) {
	pkg := NPMPackage(variant, goos, goarch)
	if pkg != "" {
		probed = append(probed, Source{Name: "npmmirror", Kind: SourceNPM, URL: "https://registry.npmmirror.com",
			Allow: []string{"registry.npmmirror.com", "cdn.npmmirror.com"}, Package: pkg})
	}
	probed = append(probed, Source{Name: "GitHub", Kind: SourceFiles, URL: githubDownload,
		Allow: []string{"github.com", "*.githubusercontent.com"}})
	if pkg != "" {
		rest = append(rest,
			Source{Name: "npmjs", Kind: SourceNPM, URL: "https://registry.npmjs.org",
				Allow: []string{"registry.npmjs.org"}, Package: pkg},
			Source{Name: "jsDelivr", Kind: SourceCDN, URL: "https://cdn.jsdelivr.net/npm/" + pkg,
				Allow: []string{"cdn.jsdelivr.net"}})
	}
	for _, b := range DefaultBases {
		host := strings.TrimPrefix(b, "https://")
		rest = append(rest, Source{Name: host, Kind: SourceFiles, URL: b + DLPath, Allow: []string{host}})
	}
	return probed, rest
}

// BaseSource is the source of a download base given explicitly (--base,
// ANET_INSTALL_BASE): the files under DLPath of base, from any https
// host.
func BaseSource(base string) (Source, error) {
	b, err := normalizeBase(base)
	if err != nil {
		return Source{}, err
	}
	return Source{Name: b, Kind: SourceFiles, URL: b + DLPath}, nil
}

// DefaultProbeTimeout bounds each probe of OrderSources when it is given
// no timeout.
const DefaultProbeTimeout = 3 * time.Second

// probeReadBytes is how much of a probe's answer is read before it is
// discarded.
const probeReadBytes = 4 << 10

// OrderSources probes the sources of probed in parallel and returns those
// that answered, fastest first, followed by rest unchanged. A source that
// does not answer within timeout (DefaultProbeTimeout when 0), or answers
// with an HTTP error, is left out; the whole wait is bounded by about
// timeout plus one second.
//
// A probe is a GET of URL/Package/latest for an npm source, of
// URL@latest/package.json for a CDN source and of URL/release.json for a
// files source. Redirects are not followed: a 2xx
// or 3xx answer shows the source is reachable, and only the time it took
// is used. A nil client is a default one.
func OrderSources(ctx context.Context, client *http.Client, probed, rest []Source, timeout time.Duration) []Source {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	c := http.Client{}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	wait, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	type result struct {
		i  int
		d  time.Duration
		ok bool
	}
	ch := make(chan result, len(probed))
	for i, s := range probed {
		go func() {
			d, ok := probeSource(wait, &c, s, timeout)
			ch <- result{i, d, ok}
		}()
	}
	var answered []result
collect:
	for range probed {
		select {
		case r := <-ch:
			if r.ok {
				answered = append(answered, r)
			}
		case <-wait.Done():
			break collect
		}
	}
	slices.SortStableFunc(answered, func(a, b result) int {
		if c := cmp.Compare(a.d, b.d); c != 0 {
			return c
		}
		return cmp.Compare(a.i, b.i)
	})
	out := make([]Source, 0, len(answered)+len(rest))
	for _, r := range answered {
		out = append(out, probed[r.i])
	}
	return append(out, rest...)
}

// probeSource is one probe of OrderSources: how long s took to answer,
// and whether it answered with a 2xx or 3xx status.
func probeSource(ctx context.Context, c *http.Client, s Source, timeout time.Duration) (time.Duration, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var target string
	switch s.Kind {
	case SourceNPM:
		target = s.URL + "/" + s.Package + "/latest"
	case SourceCDN:
		target = s.URL + "@latest/package.json"
	case SourceFiles:
		target = s.URL + "/release.json"
	default:
		return 0, false
	}
	if checkURL(target, s.Allow) != nil {
		return 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Cache-Control", "no-cache")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, probeReadBytes)); err != nil {
		return 0, false
	}
	return time.Since(start), resp.StatusCode >= 200 && resp.StatusCode < 400
}

// checkURL refuses a URL that is not https or whose host allow does not
// admit.
func checkURL(rawURL string, allow []string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing %s: releases are fetched over https only", u.Redacted())
	}
	if !hostAllowed(u, allow) {
		return fmt.Errorf("refusing the host %s: not a host this source may use", u.Host)
	}
	return nil
}

// hostAllowed reports whether allow admits the host of u (see
// Source.Allow).
func hostAllowed(u *url.URL, allow []string) bool {
	if allow == nil {
		return true
	}
	host := strings.ToLower(u.Host)
	name := strings.ToLower(u.Hostname())
	defaultPort := u.Port() == "" || u.Port() == "443"
	for _, p := range allow {
		p = strings.ToLower(p)
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			if defaultPort && suffix != "" && strings.HasSuffix(name, "."+suffix) && len(name) > len(suffix)+1 {
				return true
			}
			continue
		}
		if _, _, err := net.SplitHostPort(p); err == nil {
			if host == p {
				return true
			}
			continue
		}
		if defaultPort && name == p {
			return true
		}
	}
	return false
}

// notServingError is a source that does not serve a release: Check moves
// on to the next source.
type notServingError struct{ err error }

func (e *notServingError) Error() string { return e.err.Error() }
func (e *notServingError) Unwrap() error { return e.err }

func notServing(err error) error { return &notServingError{err} }

// npmMeta is what Check reads of the metadata of a package's latest
// version.
type npmMeta struct {
	Version string `json:"version"`
	Dist    struct {
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

// maxNPMMetaBytes bounds the metadata of a package version.
const maxNPMMetaBytes = 1 << 20

// Bounds on a package tarball, which is read before anything in it is
// verified. Variables, so that a test can lower them.
var (
	maxTarballBytes  int64 = 256 << 20 // the tarball as downloaded
	maxTarTotalBytes int64 = 512 << 20 // the tarball unpacked, headers and entries outside dl/ included
	maxAssetGzBytes  int64 = 256 << 20 // one .gz in dl/
)

// npmLatest reads the metadata of the latest version of s.Package, or for
// a CDN source the package.json of its latest version (which has no
// integrity).
func (u *Updater) npmLatest(ctx context.Context, s Source) (*npmMeta, error) {
	where := s.URL + "/" + s.Package + "/latest"
	if s.Kind == SourceCDN {
		where = s.URL + "@latest/package.json"
	}
	b, err := u.get(ctx, s.Allow, where, maxNPMMetaBytes)
	if err != nil {
		return nil, err
	}
	var m npmMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errors.New("not npm package metadata")
	}
	if _, err := parseVersion(m.Version); err != nil {
		return nil, fmt.Errorf("package metadata: %w", err)
	}
	return &m, nil
}

// npmFiles downloads the tarball at tarURL and returns the files of its
// directory package/dl/ that a release uses, by name. integrity is the
// dist.integrity of the package metadata; a sha512 in it must match the
// tarball.
func (u *Updater) npmFiles(ctx context.Context, s Source, tarURL, integrity string) (map[string][]byte, error) {
	resp, err := u.open(ctx, s.Allow, tarURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body := newCapReader(resp.Body, maxTarballBytes, "the tarball")
	h := sha512.New()
	files, err := readDL(io.TeeReader(body, h))
	if err != nil {
		return nil, err
	}
	// The integrity covers the whole file, including anything after the
	// gzip stream.
	if _, err := io.Copy(io.Discard, io.TeeReader(body, h)); err != nil {
		return nil, err
	}
	if err := checkIntegrity(integrity, h.Sum(nil)); err != nil {
		return nil, err
	}
	return files, nil
}

// checkIntegrity compares sum, the sha512 of a tarball, with the sha512
// entries of an npm integrity string ("sha512-<base64>", possibly among
// others). An integrity with no sha512 entry checks nothing: it is not
// what the release is trusted for, only a way to notice a damaged
// download early.
func checkIntegrity(integrity string, sum []byte) error {
	checked := false
	for _, f := range strings.Fields(integrity) {
		b64, ok := strings.CutPrefix(f, "sha512-")
		if !ok {
			continue
		}
		b64, _, _ = strings.Cut(b64, "?")
		want, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(want) != sha512.Size {
			return fmt.Errorf("package metadata: malformed integrity %q", f)
		}
		if bytes.Equal(want, sum) {
			return nil
		}
		checked = true
	}
	if checked {
		return errors.New("the tarball does not match the sha512 integrity of the package metadata")
	}
	return nil
}

// readDL reads a gzip-compressed tar stream and returns the files of its
// directory package/dl/ that a release uses (dlLimit), by name.
//
// The stream is read before anything in it is verified, so it is read
// defensively: the unpacked size is bounded, as is each file kept; an
// entry with an absolute name or a ".." element is an error, as is
// anything but a regular file at package/dl/<name>, and a name that
// occurs twice there. Entries elsewhere are skipped.
func readDL(r io.Reader) (map[string][]byte, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("the tarball: %w", err)
	}
	total := newCapReader(zr, maxTarTotalBytes, "the unpacked tarball")
	tr := tar.NewReader(total)
	files := map[string][]byte{}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the tarball: %w", err)
		}
		if path.IsAbs(h.Name) || slices.Contains(strings.Split(h.Name, "/"), "..") {
			return nil, fmt.Errorf("the tarball: unsafe entry name %q", h.Name)
		}
		rest, ok := strings.CutPrefix(h.Name, "package/dl/")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(rest, "/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != '\x00' {
			return nil, fmt.Errorf("the tarball: package/dl/%s is not a regular file", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("the tarball: package/dl/%s occurs twice", name)
		}
		seen[name] = true
		limit := dlLimit(name)
		if limit == 0 {
			continue
		}
		if h.Size > limit {
			return nil, fmt.Errorf("the tarball: package/dl/%s: more than %d bytes", name, limit)
		}
		b, err := io.ReadAll(io.LimitReader(tr, limit+1))
		if err != nil {
			return nil, fmt.Errorf("the tarball: %w", err)
		}
		if int64(len(b)) > limit {
			return nil, fmt.Errorf("the tarball: package/dl/%s: more than %d bytes", name, limit)
		}
		files[name] = b
	}
	// The rest of the stream: the end-of-archive blocks, and whatever a
	// further gzip member holds, still within the bound.
	if _, err := io.Copy(io.Discard, total); err != nil {
		return nil, err
	}
	return files, nil
}

// dlLimit is the most a file of package/dl/ may hold, or 0 for a file a
// release does not use.
func dlLimit(name string) int64 {
	switch {
	case name == "release.json":
		return MaxManifestBytes
	case name == "release.json.sig":
		return MaxSigBytes
	case strings.HasSuffix(name, ".gz"):
		return maxAssetGzBytes
	}
	return 0
}

// capReader reads at most n bytes from r. A byte beyond them is an error,
// not the end of the stream: nothing read through it is valid at a
// greater length.
type capReader struct {
	r     io.Reader
	n     int64 // what may still be read
	limit int64
	what  string
}

func newCapReader(r io.Reader, limit int64, what string) *capReader {
	return &capReader{r: r, n: limit, limit: limit, what: what}
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		var one [1]byte
		k, err := c.r.Read(one[:])
		if k > 0 {
			return 0, fmt.Errorf("%s: more than %d bytes", c.what, c.limit)
		}
		return 0, err
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	k, err := c.r.Read(p)
	c.n -= int64(k)
	return k, err
}
