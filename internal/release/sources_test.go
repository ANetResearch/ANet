package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPackage is the npm package the registries in these tests serve.
const testPackage = "@agentnetwork/anet-test-x64"

// serveFiles is a release host serving files (path → body; a missing or
// nil entry is a 404).
func serveFiles(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := files[r.URL.Path]
		if b == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// filesSource is a files source for the release srv serves under /dl.
func filesSource(name string, srv *httptest.Server) Source {
	return Source{Name: name, Kind: SourceFiles, URL: srv.URL + DLPath}
}

// tarEntry is one entry of a package tarball made by makeTgz.
type tarEntry struct {
	name string
	typ  byte // 0: a regular file
	body []byte
	link string
}

func makeTgz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Linkname: e.link, ModTime: time.Unix(0, 0)}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pkgEntries are the entries npm pack writes for the package of f's
// release.
func (f *fixture) pkgEntries() []tarEntry {
	asset := "anet-" + runtime.GOOS + "-" + runtime.GOARCH + ".gz"
	return []tarEntry{
		{name: "package/package.json", body: []byte(`{"name":"` + testPackage + `"}`)},
		{name: "package/README.md", body: []byte("anet\n")},
		{name: "package/dl/release.json", body: f.manifest},
		{name: "package/dl/release.json.sig", body: f.sig},
		{name: "package/dl/" + asset, body: f.gz},
	}
}

// registry is an npm registry serving one version of testPackage.
type registry struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func (r *registry) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.requests...)
}

func (r *registry) tarballRequests() int {
	n := 0
	for _, p := range r.paths() {
		if strings.HasSuffix(p, ".tgz") {
			n++
		}
	}
	return n
}

// serveRegistry serves the metadata of testPackage at version and tgz as
// its tarball. integrity is the dist.integrity of the metadata: "" for
// none, "auto" for the sha512 of tgz. The metadata names a tarball URL on
// another host, which Check must not follow.
func serveRegistry(t *testing.T, version string, tgz []byte, integrity string) *registry {
	t.Helper()
	if integrity == "auto" {
		d := sha512.Sum512(tgz)
		integrity = "sha512-" + base64.StdEncoding.EncodeToString(d[:])
	}
	meta, _ := json.Marshal(map[string]any{
		"name": testPackage, "version": version,
		"dist": map[string]any{"integrity": integrity, "tarball": "https://elsewhere.invalid/x.tgz"},
	})
	reg := &registry{}
	tarPath := "/" + testPackage + "/-/anet-test-x64-" + version + ".tgz"
	reg.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg.mu.Lock()
		reg.requests = append(reg.requests, r.URL.Path)
		reg.mu.Unlock()
		switch r.URL.Path {
		case "/" + testPackage + "/latest":
			w.Write(meta)
		case tarPath:
			w.Write(tgz)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(reg.Close)
	return reg
}

func npmSource(name string, reg *registry) Source {
	return Source{Name: name, Kind: SourceNPM, URL: reg.URL, Package: testPackage}
}

// A source that does not serve a release is skipped: a 404, an HTML page,
// then an npm registry whose tarball holds the release. Apply installs the
// .gz unpacked from the tarball without another request.
func TestSourcesFallBackToAnNPMRegistry(t *testing.T) {
	f := newFixture(t)
	empty := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(empty.Close)
	spa := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<!doctype html><title>docs</title>")
	}))
	t.Cleanup(spa.Close)
	reg := serveRegistry(t, f.version, makeTgz(t, f.pkgEntries()), "auto")

	u, exe := f.updater(empty, "0.1.10")
	u.Sources = []Source{filesSource("empty", empty), filesSource("spa", spa), npmSource("registry", reg)}
	found, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if found.Source.Name != "registry" || found.Base != reg.URL+"/"+testPackage || found.Cmp != 1 {
		t.Fatalf("Check: source %+v, base %s, cmp %d", found.Source, found.Base, found.Cmp)
	}
	before := len(reg.paths())
	if err := u.Apply(context.Background(), found); err != nil {
		t.Fatal(err)
	}
	if got := reg.paths(); len(got) != before {
		t.Fatalf("Apply made further requests: %v", got[before:])
	}
	if got := reg.paths(); !reflect.DeepEqual(got, []string{"/" + testPackage + "/latest", "/" + testPackage + "/-/anet-test-x64-0.2.0.tgz"}) {
		t.Fatalf("registry requests: %v", got)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, f.bin) {
		t.Fatal("the binary was not replaced with the asset")
	}
}

// A release that fails verification ends the search, whichever kind of
// source serves it, though a later source serves a good one.
func TestSourcesBadSignatureIsRefused(t *testing.T) {
	f := newFixture(t)
	good := f.server()
	badSig := signSSHSIG(f.priv, []byte("something else"), Namespace)

	badFiles := maps.Clone(f.files)
	badFiles["/dl/release.json.sig"] = badSig
	filesSrv := serveFiles(t, badFiles)

	entries := f.pkgEntries()
	entries[3].body = badSig
	reg := serveRegistry(t, f.version, makeTgz(t, entries), "auto")

	for _, bad := range []Source{filesSource("bad files", filesSrv), npmSource("bad npm", reg)} {
		u, exe := f.updater(good, "0.1.10")
		u.Sources = []Source{bad, filesSource("good", good)}
		found, err := u.Check(context.Background())
		if err == nil {
			t.Fatalf("%s: a bad signature was routed around: %+v", bad.Name, found.Source)
		}
		if strings.Contains(err.Error(), "no release manifest found") {
			t.Fatalf("%s: a bad signature was taken for a source that serves nothing: %v", bad.Name, err)
		}
		untouched(t, exe)
	}
}

// A source serving an older signed release than the running one lags
// behind: the next is tried, and the first lagging one is returned only
// when none serves the running release or a newer one.
func TestSourcesLaggingMirror(t *testing.T) {
	f := newFixture(t)
	f.version = "0.1.9"
	f.build(nil)
	oldFiles := maps.Clone(f.files)
	oldTgz := makeTgz(t, f.pkgEntries())
	f.version = "0.2.0"
	f.build(nil)
	oldSrv := serveFiles(t, oldFiles)
	newSrv := serveFiles(t, f.files)
	oldReg := serveRegistry(t, "0.1.9", oldTgz, "auto")

	u, _ := f.updater(newSrv, "0.1.10")
	u.Sources = []Source{filesSource("old", oldSrv), npmSource("old npm", oldReg), filesSource("new", newSrv)}
	found, err := u.Check(context.Background())
	if err != nil || found.Source.Name != "new" || found.Cmp != 1 {
		t.Fatalf("a lagging mirror was not passed over: %+v %v", found, err)
	}
	if n := oldReg.tarballRequests(); n != 0 {
		t.Fatalf("the tarball of a lagging package was downloaded %d times", n)
	}

	// All lag: the first lagging source is the answer, and Apply refuses it.
	u.Sources = []Source{filesSource("old", oldSrv), npmSource("old npm", oldReg)}
	found, err = u.Check(context.Background())
	if err != nil || found.Source.Name != "old" || found.Base != oldSrv.URL || found.Cmp >= 0 {
		t.Fatalf("all lagging: %+v %v", found, err)
	}

	// The first lagging source an npm package: downloaded only then.
	u.Sources = []Source{npmSource("old npm", oldReg), filesSource("old", oldSrv)}
	found, err = u.Check(context.Background())
	if err != nil || found.Source.Name != "old npm" || found.Manifest.Version != "0.1.9" || found.Cmp >= 0 {
		t.Fatalf("all lagging, npm first: %+v %v", found, err)
	}
	if n := oldReg.tarballRequests(); n != 1 {
		t.Fatalf("the fallback tarball was downloaded %d times, want once", n)
	}
}

// Every way a package tarball can be malformed, or not be the file the
// metadata names, is a source that serves nothing: Check moves on, and
// nothing is written.
func TestSourcesTarballLimits(t *testing.T) {
	f := newFixture(t)
	good := f.server()
	with := func(edit func([]tarEntry) []tarEntry) []byte {
		return makeTgz(t, edit(f.pkgEntries()))
	}
	cases := map[string]struct {
		tgz       []byte
		integrity string
		total     int64 // maxTarTotalBytes, when set
		want      string
	}{
		"dot-dot entry": {tgz: with(func(e []tarEntry) []tarEntry {
			return append(e, tarEntry{name: "package/../x", body: []byte("x")})
		}), want: "unsafe entry name"},
		"absolute entry": {tgz: with(func(e []tarEntry) []tarEntry {
			return append([]tarEntry{{name: "/etc/x", body: []byte("x")}}, e...)
		}), want: "unsafe entry name"},
		"symlink manifest": {tgz: with(func(e []tarEntry) []tarEntry {
			e[2] = tarEntry{name: "package/dl/release.json", typ: tar.TypeSymlink, link: "/etc/passwd"}
			return e
		}), want: "not a regular file"},
		"hard link manifest": {tgz: with(func(e []tarEntry) []tarEntry {
			e[2] = tarEntry{name: "package/dl/release.json", typ: tar.TypeLink, link: "package/README.md"}
			return e
		}), want: "not a regular file"},
		"directory in dl": {tgz: with(func(e []tarEntry) []tarEntry {
			return append(e, tarEntry{name: "package/dl/sub/", typ: tar.TypeDir})
		}), want: "not a regular file"},
		"duplicate": {tgz: with(func(e []tarEntry) []tarEntry {
			return append(e, e[2])
		}), want: "occurs twice"},
		"oversized manifest": {tgz: with(func(e []tarEntry) []tarEntry {
			e[2].body = append(bytes.Repeat([]byte(" "), MaxManifestBytes), e[2].body...)
			return e
		}), want: "more than"},
		"over the unpacked total": {tgz: with(func(e []tarEntry) []tarEntry {
			return append(e, tarEntry{name: "package/filler", body: make([]byte, 64<<10)})
		}), total: 32 << 10, want: "the unpacked tarball: more than"},
		"no manifest": {tgz: with(func(e []tarEntry) []tarEntry {
			return append(e[:2], e[3:]...)
		}), want: "no package/dl/release.json"},
		"no asset": {tgz: with(func(e []tarEntry) []tarEntry {
			return e[:4]
		}), want: ".gz"},
		"not gzip": {tgz: []byte("<!doctype html>"), want: "the tarball"},
		"integrity mismatch": {tgz: makeTgz(t, f.pkgEntries()),
			integrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)), want: "integrity"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if c.total != 0 {
				old := maxTarTotalBytes
				maxTarTotalBytes = c.total
				t.Cleanup(func() { maxTarTotalBytes = old })
			}
			integrity := c.integrity
			if integrity == "" {
				integrity = "auto"
			}
			reg := serveRegistry(t, f.version, c.tgz, integrity)
			u, exe := f.updater(good, "0.1.10")
			u.Sources = []Source{npmSource("registry", reg)}
			_, err := u.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), "no release manifest found") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Check: %v; want a source that serves nothing (%q)", err, c.want)
			}
			u.Sources = []Source{npmSource("registry", reg), filesSource("good", good)}
			found, err := u.Check(context.Background())
			if err != nil || found.Source.Name != "good" {
				t.Fatalf("did not move on to the next source: %+v %v", found, err)
			}
			untouched(t, exe)
		})
	}
}

// Every request of a source, redirects included, goes to a host its Allow
// admits; a redirect elsewhere is a source that serves nothing.
func TestSourcesRedirectRestriction(t *testing.T) {
	f := newFixture(t)
	target := f.server()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	host := func(s *httptest.Server) string { u, _ := url.Parse(s.URL); return u.Host }

	u, exe := f.updater(target, "0.1.10")
	src := filesSource("redirect", redirect)
	src.Allow = []string{host(redirect)}
	u.Sources = []Source{src}
	_, err := u.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no release manifest found") || !strings.Contains(err.Error(), host(target)) {
		t.Fatalf("a redirect to a host not allowed was followed, or not named: %v", err)
	}

	// The first request is checked too.
	src.Allow = []string{host(target)}
	u.Sources = []Source{src}
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), host(redirect)) {
		t.Fatalf("a source URL on a host not allowed was requested: %v", err)
	}
	untouched(t, exe)

	src.Allow = []string{host(redirect), host(target)}
	u.Sources = []Source{src}
	found, err := u.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Apply(context.Background(), found); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, f.bin) {
		t.Fatal("the binary was not replaced with the asset")
	}
}

func TestHostAllowed(t *testing.T) {
	allow := []string{"github.com", "*.githubusercontent.com", "127.0.0.1:43123"}
	for raw, want := range map[string]bool{
		"https://github.com/x":                         true,
		"https://GitHub.com:443/x":                     true,
		"https://github.com:8443/x":                    false,
		"https://api.github.com/x":                     false,
		"https://objects.githubusercontent.com/x":      true,
		"https://a.b.githubusercontent.com/x":          true,
		"https://githubusercontent.com/x":              false,
		"https://evilgithubusercontent.com/x":          false,
		"https://objects.githubusercontent.com.evil/x": false,
		"https://127.0.0.1:43123/x":                    true,
		"https://127.0.0.1:43124/x":                    false,
		"https://127.0.0.1/x":                          false,
		"https://objects.githubusercontent.com:8443/x": false,
	} {
		u, _ := url.Parse(raw)
		if got := hostAllowed(u, allow); got != want {
			t.Errorf("hostAllowed(%s) = %v, want %v", raw, got, want)
		}
	}
	u, _ := url.Parse("https://anything.example:8443/")
	if !hostAllowed(u, nil) || hostAllowed(u, []string{}) {
		t.Error("nil must allow any host, an empty list none")
	}
}

// OrderSources puts the probed sources that answer first, fastest first,
// drops those that do not, and keeps rest as it is.
func TestOrderSources(t *testing.T) {
	fast := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+testPackage+"/latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"version":"0.2.0"}`)
	}))
	t.Cleanup(fast.Close)
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, "{}")
	}))
	t.Cleanup(slow.Close)
	// A redirect is an answer; it is not followed.
	moved := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		http.Redirect(w, r, "https://nowhere.invalid/release.json", http.StatusFound)
	}))
	t.Cleanup(moved.Close)
	failing := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(failing.Close)
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closed.Close()

	probed := []Source{
		{Name: "slow", Kind: SourceFiles, URL: slow.URL + "/dl"},
		{Name: "closed", Kind: SourceFiles, URL: closed.URL + "/dl"},
		{Name: "moved", Kind: SourceFiles, URL: moved.URL + "/dl"},
		{Name: "failing", Kind: SourceFiles, URL: failing.URL + "/dl"},
		{Name: "fast", Kind: SourceNPM, URL: fast.URL, Package: testPackage},
		{Name: "not allowed", Kind: SourceFiles, URL: fast.URL + "/dl", Allow: []string{"example.org"}},
	}
	rest := []Source{{Name: "rest 1"}, {Name: "rest 2"}}
	got := OrderSources(context.Background(), fast.Client(), probed, rest, 2*time.Second)
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	if want := []string{"fast", "moved", "slow", "rest 1", "rest 2"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("order %v, want %v", names, want)
	}
}

// A probe that does not answer is given up after the timeout.
func TestOrderSourcesIsBounded(t *testing.T) {
	done := make(chan struct{})
	hang := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(done) })

	start := time.Now()
	got := OrderSources(context.Background(), hang.Client(),
		[]Source{{Name: "hang", Kind: SourceFiles, URL: hang.URL + "/dl"}}, []Source{{Name: "rest"}}, 300*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("OrderSources took %v with a 300ms timeout", d)
	}
	if len(got) != 1 || got[0].Name != "rest" {
		t.Fatalf("a source that did not answer was kept: %+v", got)
	}
}

func TestDefaultSources(t *testing.T) {
	names := func(l []Source) (out []string) {
		for _, s := range l {
			out = append(out, s.Name)
		}
		return out
	}
	probed, rest := Sources("shell", "darwin", "arm64")
	pkg := "@agentnetwork/anet-shell-darwin-arm64"
	if got := names(probed); !reflect.DeepEqual(got, []string{"npmmirror", "GitHub"}) {
		t.Errorf("probed %v", got)
	}
	if got := names(rest); !reflect.DeepEqual(got, []string{"npmjs", "jsDelivr", "agentnetwork.org.cn", "hub.agentnetwork.org.cn"}) {
		t.Errorf("rest %v", got)
	}
	want := []Source{
		{Name: "npmmirror", Kind: SourceNPM, URL: "https://registry.npmmirror.com", Allow: []string{"registry.npmmirror.com", "cdn.npmmirror.com"}, Package: pkg},
		{Name: "GitHub", Kind: SourceFiles, URL: "https://github.com/ANetResearch/ANet/releases/latest/download", Allow: []string{"github.com", "*.githubusercontent.com"}},
		{Name: "npmjs", Kind: SourceNPM, URL: "https://registry.npmjs.org", Allow: []string{"registry.npmjs.org"}, Package: pkg},
		{Name: "jsDelivr", Kind: SourceFiles, URL: "https://cdn.jsdelivr.net/npm/" + pkg + "@latest/dl", Allow: []string{"cdn.jsdelivr.net"}},
		{Name: "agentnetwork.org.cn", Kind: SourceFiles, URL: "https://agentnetwork.org.cn/dl", Allow: []string{"agentnetwork.org.cn"}},
		{Name: "hub.agentnetwork.org.cn", Kind: SourceFiles, URL: "https://hub.agentnetwork.org.cn/dl", Allow: []string{"hub.agentnetwork.org.cn"}},
	}
	if got := append(probed, rest...); !reflect.DeepEqual(got, want) {
		t.Errorf("Sources:\n %+v\nwant\n %+v", got, want)
	}
	if s := want[0]; s.tarballURL("0.3.6") != "https://registry.npmmirror.com/"+pkg+"/-/anet-shell-darwin-arm64-0.3.6.tgz" ||
		s.Location() != "https://registry.npmmirror.com/"+pkg {
		t.Errorf("npmmirror tarball %s, location %s", s.tarballURL("0.3.6"), s.Location())
	}
	for _, s := range want {
		u, _ := url.Parse(s.URL)
		if !hostAllowed(u, s.Allow) {
			t.Errorf("%s: its own URL is not allowed", s.Name)
		}
	}
	probed, rest = Sources("default", "freebsd", "amd64")
	if got := names(append(probed, rest...)); !reflect.DeepEqual(got, []string{"GitHub", "agentnetwork.org.cn", "hub.agentnetwork.org.cn"}) {
		t.Errorf("a build without an npm package: %v", got)
	}
	if probed, _ := Sources("default", "windows", "amd64"); probed[0].Package != "@agentnetwork/anet-win32-x64" {
		t.Errorf("windows package %q", probed[0].Package)
	}
}

func TestBaseSource(t *testing.T) {
	s, err := BaseSource(" https://mirror.example/anet/ ")
	want := Source{Name: "https://mirror.example/anet", Kind: SourceFiles, URL: "https://mirror.example/anet/dl"}
	if err != nil || !reflect.DeepEqual(s, want) || s.base() != "https://mirror.example/anet" {
		t.Fatalf("BaseSource: %+v %v", s, err)
	}
	for _, bad := range []string{"http://mirror.example", "https://u:p@mirror.example", "mirror.example"} {
		if _, err := BaseSource(bad); err == nil {
			t.Errorf("BaseSource(%q) accepted", bad)
		}
	}
}

// The trust of a release does not depend on the source: a second key the
// test does not trust is refused from an npm registry as from a host.
func TestSourcesNPMRefusesAnotherKey(t *testing.T) {
	f := newFixture(t)
	reg := serveRegistry(t, f.version, makeTgz(t, f.pkgEntries()), "")
	u, exe := f.updater(f.server(), "0.1.10")
	other, _ := newKey(t)
	u.Trust = Trust{Keys: []ed25519.PublicKey{other}}
	u.Sources = []Source{npmSource("registry", reg)}
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not the release key") {
		t.Fatalf("a package signed by an unknown key: %v", err)
	}
	untouched(t, exe)
}
