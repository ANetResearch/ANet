package release

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fixture is a release host: a manifest in the layout build-release.sh
// writes, signed by a key made for the test, and one asset — a shell
// script that answers `version` the way anet does, so the default probe
// runs for real.
type fixture struct {
	t        *testing.T
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	version  string
	modules  string // what the asset's `version` prints
	manifest []byte
	sig      []byte
	gz       []byte
	bin      []byte
	files    map[string][]byte // path → body served; nil entry = 404
	binBody  []byte            // when set, the asset instead of the rendered script
	now      time.Time
}

func newFixture(t *testing.T) *fixture {
	if runtime.GOOS == "windows" {
		t.Skip("the asset is a shell script")
	}
	pub, priv := newKey(t)
	f := &fixture{t: t, pub: pub, priv: priv, version: "0.2.0", modules: "cas,mcp,x402",
		now: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	f.build(nil)
	return f
}

// build renders the asset and the manifest; edit may change the manifest
// fields before it is signed.
func (f *fixture) build(edit func(gzSHA, sha *string, gzSize, size *int64)) {
	f.bin = []byte(fmt.Sprintf("#!/bin/sh\necho 'anet %s (commit abc, built 2026-09-27T08:33:44Z)'\necho 'modules: %s'\n",
		f.version, f.modules))
	if f.binBody != nil {
		f.bin = f.binBody
	}
	var zb bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zb, 9)
	zw.Write(f.bin)
	zw.Close()
	f.gz = zb.Bytes()
	gzSHA, sha := hexsum(f.gz), hexsum(f.bin)
	gzSize, size := int64(len(f.gz)), int64(len(f.bin))
	if edit != nil {
		edit(&gzSHA, &sha, &gzSize, &size)
	}
	f.manifest = []byte(fmt.Sprintf(`{
  "schema": "anet-release/1",
  "version": "%s",
  "commit": "6295665a1b2c3d4e5f60718293a4b5c6d7e8f901",
  "built_at": "2026-09-27T08:33:44Z",
  "released_at": "2026-09-27T09:00:00Z",
  "expires_at": "2026-12-26T09:00:00Z",
  "go": "go1.26.6",
  "key_fingerprint": "%s",
  "next_key_fingerprint": "",
  "variants": {
    "default": {"asset_prefix": "anet", "tags": "", "modules": ["cas", "mcp", "x402"]},
    "shell": {"asset_prefix": "anet-shell", "tags": "shell", "modules": ["cas", "mcp", "shell", "x402"]}
  },
  "assets": {
    "anet-%[3]s-%[4]s": {"variant": "default", "os": "%[3]s", "arch": "%[4]s", "gz_sha256": "%[5]s", "gz_size": %[6]d, "sha256": "%[7]s", "size": %[8]d}
  }
}
`, f.version, Fingerprint(f.pub), runtime.GOOS, runtime.GOARCH, gzSHA, gzSize, sha, size))
	f.sig = signSSHSIG(f.priv, f.manifest, Namespace)
	f.files = map[string][]byte{
		"/dl/release.json":     f.manifest,
		"/dl/release.json.sig": f.sig,
		"/dl/anet-" + runtime.GOOS + "-" + runtime.GOARCH + ".gz": f.gz,
	}
}

func (f *fixture) server() *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := f.files[r.URL.Path]
		if !ok || b == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

// updater returns an Updater pointed at srv, replacing a fresh "old"
// binary in a temp dir.
func (f *fixture) updater(srv *httptest.Server, current string) (*Updater, string) {
	dir := f.t.TempDir()
	exe := filepath.Join(dir, "anet")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return &Updater{
		Bases:   []string{srv.URL},
		Client:  srv.Client(),
		Trust:   Trust{Keys: []ed25519.PublicKey{f.pub}},
		Now:     func() time.Time { return f.now },
		Current: current,
		Variant: "default",
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Exe:     exe,
	}, exe
}

// untouched asserts the old binary is still there and nothing else was
// left in its directory.
func untouched(t *testing.T, exe string) {
	t.Helper()
	b, err := os.ReadFile(exe)
	if err != nil || string(b) != "#!/bin/sh\necho old\n" {
		t.Fatalf("the installed binary was changed: %q %v", b, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(exe))
	if len(ents) != 1 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("left behind in the install directory: %v", names)
	}
}

func hexsum(b []byte) string {
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

func TestUpdateReplacesTheBinary(t *testing.T) {
	f := newFixture(t)
	srv := f.server()
	u, exe := f.updater(srv, "0.1.10")
	ctx := context.Background()
	found, err := u.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if found.Cmp != 1 || found.Manifest.Version != "0.2.0" || found.Signer != Fingerprint(f.pub) {
		t.Fatalf("Check: %+v", found)
	}
	if err := u.Apply(ctx, found); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(exe)
	if !bytes.Equal(b, f.bin) {
		t.Fatal("the binary was not replaced with the asset")
	}
	st, _ := os.Stat(exe)
	if st.Mode().Perm()&0o111 == 0 {
		t.Fatal("the new binary is not executable")
	}
	ents, _ := os.ReadDir(filepath.Dir(exe))
	if len(ents) != 1 {
		t.Fatalf("temporary files left behind: %d entries", len(ents))
	}
}

func TestUpdateSameVersionIsNoOpAndOlderIsRefused(t *testing.T) {
	f := newFixture(t)
	srv := f.server()

	u, _ := f.updater(srv, "0.2.0")
	found, err := u.Check(context.Background())
	if err != nil || found.Cmp != 0 {
		t.Fatalf("same version: %+v %v", found, err)
	}

	u, exe := f.updater(srv, "0.3.0")
	found, err = u.Check(context.Background())
	if err != nil || found.Cmp != -1 {
		t.Fatalf("older manifest: %+v %v", found, err)
	}
	if err := u.Apply(context.Background(), found); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("Apply of an older release: %v", err)
	}
	untouched(t, exe)
}

func TestUpdateRefusesAnExpiredManifest(t *testing.T) {
	f := newFixture(t)
	f.now = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	u, _ := f.updater(f.server(), "0.1.10")
	if _, err := u.Check(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired manifest: %v", err)
	}
}

func TestUpdateRefusesAnotherKey(t *testing.T) {
	f := newFixture(t)
	u, _ := f.updater(f.server(), "0.1.10")
	other, _ := newKey(t)
	u.Trust = Trust{Keys: []ed25519.PublicKey{other}}
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not the release key") {
		t.Fatalf("a manifest signed by an unknown key: %v", err)
	}
}

// Every way the downloaded bytes can differ from what the manifest names:
// each stops before the rename, and the old binary is left as it was.
func TestUpdateFailuresLeaveTheOldBinary(t *testing.T) {
	asset := "/dl/anet-" + runtime.GOOS + "-" + runtime.GOARCH + ".gz"
	cases := map[string]func(f *fixture){
		"gz replaced on the host": func(f *fixture) {
			f.files[asset] = append(append([]byte{}, f.gz...), 0)
		},
		"gz sha in manifest wrong": func(f *fixture) {
			f.build(func(gzSHA, _ *string, _, _ *int64) { *gzSHA = strings.Repeat("0", 64) })
		},
		"binary sha in manifest wrong": func(f *fixture) {
			f.build(func(_, sha *string, _, _ *int64) { *sha = strings.Repeat("0", 64) })
		},
		"binary size in manifest wrong": func(f *fixture) {
			f.build(func(_, _ *string, _, size *int64) { *size-- })
		},
		"binary reports other modules": func(f *fixture) {
			f.modules = "cas,mcp,shell,x402"
			f.build(nil)
		},
		"binary reports other version": func(f *fixture) {
			// Correct hashes, signed — of a binary that says it is 0.1.99.
			f.binBody = []byte("#!/bin/sh\necho 'anet 0.1.99 (commit x, built y)'\necho 'modules: cas,mcp,x402'\n")
			f.build(nil)
		},
		"binary does not run": func(f *fixture) {
			f.binBody = []byte("#!/bin/sh\nexit 3\n")
			f.build(nil)
		},
		"asset missing": func(f *fixture) { f.files[asset] = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(f)
			u, exe := f.updater(f.server(), "0.1.10")
			found, err := u.Check(context.Background())
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if err := u.Apply(context.Background(), found); err == nil {
				t.Fatal("Apply succeeded")
			}
			untouched(t, exe)
		})
	}
}

// A base that does not serve a manifest (404, or an HTML page in its
// place) is skipped; a base that serves a manifest with a bad signature
// ends the search.
func TestUpdateBaseSelection(t *testing.T) {
	f := newFixture(t)
	good := f.server()

	empty := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(empty.Close)
	spa := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<!doctype html><title>docs</title>")
	}))
	t.Cleanup(spa.Close)

	// All three test servers share one CA, so one client trusts all.
	u, _ := f.updater(good, "0.1.10")
	u.Bases = []string{empty.URL, spa.URL, good.URL}
	found, err := u.Check(context.Background())
	if err != nil || found.Base != good.URL {
		t.Fatalf("did not fall through to the base that serves a manifest: %+v %v", found, err)
	}

	bad := newFixture(t)
	bad.files["/dl/release.json.sig"] = signSSHSIG(bad.priv, []byte("something else"), Namespace)
	badSrv := bad.server()
	u.Bases = []string{badSrv.URL, good.URL}
	u.Trust = Trust{Keys: []ed25519.PublicKey{f.pub, bad.pub}}
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("a bad signature on the first base was routed around")
	}
}

func TestUpdateIsHTTPSOnly(t *testing.T) {
	f := newFixture(t)
	srv := f.server()
	u, _ := f.updater(srv, "0.1.10")
	u.Bases = []string{strings.Replace(srv.URL, "https://", "http://", 1)}
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("an http base was used: %v", err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(f.files[r.URL.Path])
	}))
	t.Cleanup(plain.Close)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	u.Bases = []string{redirect.URL}
	u.Client = redirect.Client()
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("a redirect to http was followed: %v", err)
	}
}

func TestNormalizeBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://agentnetwork.org.cn":         "https://agentnetwork.org.cn",
		"https://agentnetwork.org.cn/":        "https://agentnetwork.org.cn",
		" https://mirror.example/anet/ ":      "https://mirror.example/anet",
		"https://hub.agentnetwork.org.cn:443": "https://hub.agentnetwork.org.cn:443",
	} {
		got, err := normalizeBase(in)
		if err != nil || got != want {
			t.Errorf("normalizeBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"http://agentnetwork.org.cn", "ftp://x", "https://", "agentnetwork.org.cn",
		"https://x/?a=b", "https://x/#f", "https://user:s3cret@mirror.example",
	} {
		_, err := normalizeBase(bad)
		if err == nil {
			t.Errorf("normalizeBase(%q) accepted", bad)
			continue
		}
		// A credential in a base must not come back out in the error the
		// CLI prints.
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("normalizeBase(%q) repeats the password: %v", bad, err)
		}
	}
}

func TestParseVersionOutput(t *testing.T) {
	v, m, err := ParseVersionOutput("anet 0.2.0 (commit 1a2b3c4, built 2026-09-27T08:33:44Z)\nmodules: cas,mcp\n")
	if err != nil || v != "0.2.0" || strings.Join(m, ",") != "cas,mcp" {
		t.Fatalf("%q %v %v", v, m, err)
	}
	v, m, err = ParseVersionOutput("anet 0.2.0 (commit x, built y)\nmodules: (none)\n")
	if err != nil || v != "0.2.0" || len(m) != 0 {
		t.Fatalf("kernel-only: %q %v %v", v, m, err)
	}
	if _, _, err := ParseVersionOutput("anet 0.1.5 (commit x, built y)\n"); err == nil {
		t.Fatal("output without a modules line was accepted")
	}
}
