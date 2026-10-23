//go:build !windows

package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	anetversion "github.com/ANetResearch/ANet/internal/version"
)

// install.sh end to end: the whole script, piped into sh as `curl | sh`
// does, in a temporary HOME, against a release signed by a key made for the
// test and served over TLS by local sources that stand in for npmmirror,
// GitHub and the rest. The binary of that release is a shell script that
// answers like anet (version, help, init, up, status, stop, hub-register)
// and contacts nothing: no test registers anything on a real hub.

// shRelease is a signed release of the stand-in binary.
type shRelease struct {
	t        *testing.T
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	version  string
	asset    string
	pkg      string
	bin      []byte
	gz       []byte
	manifest []byte
	sig      []byte
}

const e2eAID = "bafyreie2etestaid4installsh7summary2golden5output3fresh6take"

// fakeAnetScript answers like anet. Its data directory is that of anet;
// FAKE_HUB=pending makes hub-register answer as a hub that admits by AID.
func fakeAnetScript(ver string) string { return fakeAnetScriptVariant(ver, "default") }

// fakeAnetScriptVariant is fakeAnetScript for the build variant: the shell
// build reports the shell module and has `anet init --shell-controller`,
// which, as anet's own, refuses while a node runs.
func fakeAnetScriptVariant(ver, variant string) string {
	mods, initHelp := "cas,mcp,x402", "anet init"
	if variant == "shell" {
		mods, initHelp = "cas,mcp,shell,x402", "anet init [--shell-controller <aid> [--yes]]"
	}
	return `#!/bin/sh
d=${ANET_DATA_DIR:-${ANET_HOME:-$HOME/.anet}}
echo "anet $*" >> "$HOME/anet-calls.log"
case "$1" in
  version) echo 'anet ` + ver + ` (commit abc, built 2026-10-20T00:00:00Z)'; echo 'modules: ` + mods + `' ;;
  help) printf '  ` + initHelp + `\n  anet up\n  anet status\n  anet stop\n  anet hub-register <url>\n  anet agents wire\n  anet doctor\n' ;;
  init) mkdir -p "$d"
        if [ "$2" = --shell-controller ]; then
          [ -f "$d/.running" ] && { echo 'a node is running; stop it first' >&2; exit 1; }
          printf '%s\n' "$3" > "$d/.controller"; echo "anet init --shell-controller"; exit 0
        fi
        echo 'init: inbound closed; no public capabilities; spending limits 0' ;;
  up) mkdir -p "$d"; touch "$d/.running"; echo 'anet ` + ver + ` is running in the background' ;;
  status) [ -f "$d/.running" ] || { echo 'no daemon is running' >&2; exit 1; }
          printf '{\n  "aid": "` + e2eAID + `",\n  "hub_url": "%s",\n  "version": "` + ver + `"\n}\n' "$(cat "$d/.hub" 2>/dev/null)" ;;
  stop) rm -f "$d/.running"; echo stopped ;;
  hub-register)
    case "${FAKE_HUB:-}" in
      pending) echo 'This hub admits by AID; send this line to its operator:'; echo '  → anet-hub admit ` + e2eAID + `' ;;
      refused) echo 'the hub refused the registration' >&2; exit 1 ;;
      *) printf '%s\n' "$2" > "$d/.hub"; echo "Joined $2 as $4." ;;
    esac ;;
esac
`
}

func newSHRelease(t *testing.T, ver string) *shRelease { return newSHReleaseVariant(t, ver, "default") }

// newSHReleaseVariant is a release of one build variant, default or shell.
func newSHReleaseVariant(t *testing.T, ver, variant string) *shRelease {
	t.Helper()
	pub, priv := newKey(t)
	prefix, tags, mods := "anet", "", `"cas", "mcp", "x402"`
	if variant == "shell" {
		prefix, tags, mods = "anet-shell", "shell", `"cas", "mcp", "shell", "x402"`
	}
	r := &shRelease{t: t, pub: pub, priv: priv, version: ver,
		asset: prefix + "-" + runtime.GOOS + "-" + runtime.GOARCH, pkg: NPMPackage(variant, runtime.GOOS, runtime.GOARCH)}
	if r.pkg == "" {
		t.Skip("no npm package for this platform")
	}
	r.bin = []byte(fakeAnetScriptVariant(ver, variant))
	var zb bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zb, 9)
	zw.Write(r.bin)
	zw.Close()
	r.gz = zb.Bytes()
	expires := time.Now().UTC().Add(60 * 24 * time.Hour).Format("2006-01-02T15:04:05Z")
	r.manifest = []byte(fmt.Sprintf(`{
  "schema": "anet-release/1",
  "version": "%s",
  "commit": "6295665a1b2c3d4e5f60718293a4b5c6d7e8f901",
  "built_at": "2026-10-20T08:33:44Z",
  "released_at": "2026-10-20T09:00:00Z",
  "expires_at": "%s",
  "go": "go1.26.6",
  "key_fingerprint": "%s",
  "next_key_fingerprint": "",
  "variants": {
    "%s": {"asset_prefix": "%s", "tags": "%s", "modules": [%s]}
  },
  "assets": {
    "%s": {"variant": "%s", "os": "%s", "arch": "%s", "gz_sha256": "%s", "gz_size": %d, "sha256": "%s", "size": %d}
  }
}
`, ver, expires, Fingerprint(pub), variant, prefix, tags, mods, r.asset, variant, runtime.GOOS, runtime.GOARCH, hexsum(r.gz), len(r.gz), hexsum(r.bin), len(r.bin)))
	r.sig = signSSHSIG(priv, r.manifest, Namespace)
	return r
}

// tarball is the npm package of the build: package/dl/ holds the release.
func (r *shRelease) tarball() []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"package/package.json", []byte(`{"name": "` + r.pkg + `", "version": "` + r.version + `"}`)},
		{"package/dl/release.json", r.manifest},
		{"package/dl/release.json.sig", r.sig},
		{"package/dl/" + r.asset + ".gz", r.gz},
	} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write(f.body)
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

// hitCount counts the requests by path; the installer probes sources in
// parallel, so the handler runs concurrently.
type hitCount struct {
	mu sync.Mutex
	m  map[string]int
}

func newHitCount() *hitCount { return &hitCount{m: map[string]int{}} }

func (h *hitCount) add(path string) {
	h.mu.Lock()
	h.m[path]++
	h.mu.Unlock()
}

func (h *hitCount) get(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[path]
}

func (h *hitCount) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fmt.Sprint(h.m)
}

// serve: /npm/<pkg>/latest and the tarball (an npm registry), and
// /files/<file> (a source of kind files). hits counts the requests.
func (r *shRelease) server(hits *hitCount) *httptest.Server {
	tgz := r.tarball()
	base := r.pkg[strings.LastIndex(r.pkg, "/")+1:]
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		hits.add(q.URL.Path)
		switch q.URL.Path {
		case "/npm/" + r.pkg + "/latest":
			fmt.Fprintf(w, `{"name":%q,"version":%q,"dist":{"tarball":"https://elsewhere.invalid/x.tgz"}}`, r.pkg, r.version)
		case "/npm/" + r.pkg + "/-/" + base + "-" + r.version + ".tgz":
			http.Redirect(w, q, "/cdn/pkg.tgz", http.StatusFound)
		case "/cdn/pkg.tgz":
			w.Write(tgz)
		case "/files/release.json":
			w.Write(r.manifest)
		case "/files/release.json.sig":
			w.Write(r.sig)
		case "/files/" + r.asset + ".gz":
			w.Write(r.gz)
		default:
			http.NotFound(w, q)
		}
	}))
	r.t.Cleanup(srv.Close)
	return srv
}

// freePort is a loopback address nothing listens on (V1_API in the tests,
// so that a real AgentNetwork 1.x on this machine is never asked anything).
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	l.Close()
	return a
}

// e2eScript is install.sh with the test key in place of the release key,
// the given sources, and the fixed places of the machine (LINK_DIR, the
// usual places of anet, V1_API, the systemd unit directories) moved into
// root.
func e2eScript(t *testing.T, r *shRelease, sources, root, v1api string) string {
	t.Helper()
	s := repoFile(t, "deploy/release/install.sh")
	line := Identity + ` namespaces="` + Namespace + `" ` + AuthorizedKey(r.pub)
	rep := func(old, new string) {
		t.Helper()
		if strings.Count(s, old) != 1 {
			t.Fatalf("install.sh: %q occurs %d times", old, strings.Count(s, old))
		}
		s = strings.Replace(s, old, new, 1)
	}
	keyFunc := regexp.MustCompile(`(?m)^release_allowed_signers\(\) \{\n  echo '[^\n]*'\n\}`).FindString(s)
	if keyFunc == "" {
		t.Fatal("install.sh: no release_allowed_signers")
	}
	rep(keyFunc, "release_allowed_signers() {\n  echo '"+line+"'\n}")
	rep("  V1_API=127.0.0.1:3998\n", "  V1_API="+v1api+"\n")
	rep("  LINK_DIR=/usr/local/bin;", "  LINK_DIR="+filepath.Join(root, "usr-local-bin")+";")
	rep(`UNIT_DIRS="/etc/systemd/system /lib/systemd/system /usr/lib/systemd/system"`, `UNIT_DIRS="`+filepath.Join(root, "etc-systemd")+`"`)
	rep("for _c in /usr/local/bin/anet /opt/homebrew/bin/anet ", "for _c in "+filepath.Join(root, "usr-local-bin", "anet")+" ")
	if !strings.HasSuffix(s, "\nmain \"$@\"\n") {
		t.Fatal("install.sh does not end with main \"$@\"")
	}
	return strings.TrimSuffix(s, "main \"$@\"\n") + "source_defaults() {\n" + sources + "\n  PROBE_TIMEOUT=2\n}\nmain \"$@\"\n"
}

type e2eRun struct {
	stdout, stderr string
	code           int
	home           string
	calls          string
}

// runE2E pipes script into `sh -s -- args` (stdin is the script, as under
// curl | sh), without a controlling terminal, in env.
func runE2E(t *testing.T, e *legacyEnv, script string, args ...string) e2eRun {
	t.Helper()
	// ANET_INSTALL_E2E_SH runs the script with another shell (bash, zsh,
	// dash), as `curl | bash` would.
	shell := "sh"
	if v := os.Getenv("ANET_INSTALL_E2E_SH"); v != "" {
		shell = v
	}
	sh, err := exec.LookPath(shell)
	if err != nil {
		t.Skip("no " + shell)
	}
	for _, tool := range []string{"curl", "ssh-keygen", "gunzip", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	cmd := exec.Command(sh, append([]string{"-s", "--"}, args...)...)
	cmd.Stdin = strings.NewReader(script)
	env := []string{"HOME=" + e.home, "PATH=" + strings.Join(e.path, ":"), "SHELL=" + e.shell,
		"TMPDIR=" + t.TempDir(), "USER=tester", "LOGNAME=tester", "LANG=C"}
	for k, v := range e.env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(e.home, "anet-calls.log"))
	return e2eRun{out.String(), errb.String(), code, e.home, string(calls)}
}

// e2eEnv is a temporary machine for install.sh as a whole: the CA of the
// test sources for curl, a stand-in npm (so that the npm of this machine is
// never asked), and ~/.local/bin first on PATH unless noLocalBin.
func e2eEnv(t *testing.T, srv *httptest.Server, noLocalBin bool) *legacyEnv {
	t.Helper()
	e := newLegacyEnv(t)
	ca := filepath.Join(e.root, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	e.env["CURL_CA_BUNDLE"] = ca
	e.env["SSL_CERT_FILE"] = ca
	e.env["ANET_LANG"] = "en"
	e.fakeNpm(filepath.Join(e.root, "npm-empty"), false)
	if !noLocalBin {
		e.path = append([]string{filepath.Join(e.home, ".local", "bin")}, e.path...)
	}
	return e
}

func e2eSources(srv *httptest.Server, githubUp bool) string {
	host := strings.TrimPrefix(srv.URL, "https://")
	gh := srv.URL + "/files"
	if !githubUp {
		gh = "https://127.0.0.1:1/files"
	}
	return `  PROBED="npm|npmmirror|` + srv.URL + `/npm|` + host + `
files|GitHub|` + gh + `|` + host + ` 127.0.0.1:1"
  REST="npm|npmjs|https://127.0.0.1:1/npm|127.0.0.1:1
files|agentnetwork.org.cn|` + srv.URL + `/files|` + host + `"`
}

// normalize replaces what differs between runs: the time stamps, the
// seconds a download took, the temporary directories.
func normalize(s string, e *legacyEnv) string {
	s = regexp.MustCompile(`\d{8}T\d{6}Z`).ReplaceAllString(s, "<UTC>")
	s = regexp.MustCompile(`\(\d+\.\d MB in \d+ s\)`).ReplaceAllString(s, "(<size> MB in <n> s)")
	s = strings.ReplaceAll(s, e.root, "<root>")
	return s
}

// installedVersion is the release the stand-in carries: this installer's
// own version, so that no source counts as lagging.
func installedVersion(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^installer_version\(\) \{ echo '([^']*)'; \}$`).FindStringSubmatch(repoFile(t, "deploy/release/install.sh"))
	if m == nil {
		t.Fatal("install.sh has no installer_version")
	}
	return m[1]
}

// Spec C/E: a fresh install prints a phase line per phase and the final
// block, nothing else; the log holds the details, privately.
func TestInstallShGoldenFresh(t *testing.T) {
	ver := installedVersion(t)
	r := newSHRelease(t, ver)
	hits := newHitCount()
	srv := r.server(hits)
	e := e2eEnv(t, srv, false)
	script := e2eScript(t, r, e2eSources(srv, false), e.root, freePort(t))
	res := runE2E(t, e, script)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	want := "Downloaded anet " + ver + " for " + runtime.GOOS + "-" + runtime.GOARCH + " from npmmirror (<size> MB in <n> s)\n" +
		"Starting the node and joining https://hub.agentnetwork.org.cn\n" +
		"\n" +
		"anet " + ver + " is installed and running.\n" +
		"  AID   " + e2eAID + "\n" +
		"  Hub   https://hub.agentnetwork.org.cn (joined)\n" +
		"Run 'anet' to get started.  Details: ~/.anet/logs/install-<UTC>.log\n"
	if got := normalize(res.stdout, e); got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}
	if res.stderr != "" {
		t.Errorf("stderr: %q", res.stderr)
	}
	t.Logf("fresh install, as printed:\n%s", res.stdout)
	checkLog(t, e.home, "using npmmirror: anet "+ver, "signature ok", "sha256 ok (.gz and binary)",
		"init: inbound closed", "anet hub-register https://hub.agentnetwork.org.cn --name ", "--passphrase-later",
		"added the anet PATH block to "+filepath.Join(e.home, ".profile"))
	mustContain(t, "calls", res.calls, "anet init\n", "anet up\n", "anet hub-register https://hub.agentnetwork.org.cn --name ")
	// The tarball came from the npm source; the files source was not asked
	// for the .gz.
	if hits.get("/cdn/pkg.tgz") != 1 || hits.get("/files/"+r.asset+".gz") != 0 {
		t.Errorf("requests: %v", hits.String())
	}
	if b, err := os.ReadFile(filepath.Join(e.home, ".local", "bin", "anet")); err != nil || !bytes.Equal(b, r.bin) {
		t.Errorf("installed binary: %v", err)
	}
}

// --controller AID: the shell build, and the grant of anet init
// --shell-controller AID --yes, written while no node runs (a running node
// is stopped first and started again); the summary names the AID and how to
// revoke it. An AID that is not whole stops the install before anything is
// downloaded.
func TestInstallShController(t *testing.T) {
	const ctl = "bafyreiamv7l6aww6oeup3i4mrtvbes5ha7xg4sr2x4gqakzqt3xkxg6g44"
	ver := installedVersion(t)
	r := newSHReleaseVariant(t, ver, "shell")
	srv := r.server(newHitCount())
	for _, running := range []bool{false, true} {
		e := e2eEnv(t, srv, false)
		if running {
			if err := os.MkdirAll(filepath.Join(e.home, ".anet"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.home, ".anet", ".running"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		script := e2eScript(t, r, e2eSources(srv, false), e.root, freePort(t))
		res := runE2E(t, e, script, "--controller", ctl)
		if res.code != 0 {
			t.Fatalf("running=%v: exit %d\nstdout:\n%s\nstderr:\n%s", running, res.code, res.stdout, res.stderr)
		}
		mustContain(t, "stdout", res.stdout, "anet "+ver+" is installed and running.\n",
			"Remote control: allowed for "+ctl+" only; revoke with: anet peers remove "+ctl+"\n")
		grant := "anet init --shell-controller " + ctl + " --yes\n"
		gi, ui := strings.Index(res.calls, grant), strings.LastIndex(res.calls, "anet up\n")
		if gi < 0 || ui < gi {
			t.Errorf("running=%v: the grant is not written before the node starts:\n%s", running, res.calls)
		}
		if running && !strings.Contains(res.calls[:gi], "anet stop\n") {
			t.Errorf("the running node was not stopped before the grant:\n%s", res.calls)
		}
		if b, err := os.ReadFile(filepath.Join(e.home, ".anet", ".controller")); err != nil || string(b) != ctl+"\n" {
			t.Errorf("running=%v: grant %q %v", running, b, err)
		}
		if b, err := os.ReadFile(filepath.Join(e.home, ".local", "bin", "anet")); err != nil || !bytes.Equal(b, r.bin) {
			t.Errorf("installed binary is not the shell build: %v", err)
		}
		checkLog(t, e.home, r.asset+".gz (shell variant", "granted, as --controller on the command line requested",
			"to revoke: anet peers remove "+ctl)
	}

	e := e2eEnv(t, srv, false)
	script := e2eScript(t, r, e2eSources(srv, false), e.root, freePort(t))
	res := runE2E(t, e, script, "--controller", ctl[:40])
	if res.code == 0 || !strings.Contains(res.stderr, "is not an AID") {
		t.Errorf("a partial AID: exit %d, stderr %q", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".local", "bin", "anet")); err == nil {
		t.Error("a partial AID installed anet")
	}
	res = runE2E(t, e, script, "--controller", ctl, "--without", "shell")
	if res.code == 0 || !strings.Contains(res.stderr, "--without shell") {
		t.Errorf("--controller with --without shell: exit %d, stderr %q", res.code, res.stderr)
	}
}

// checkLog: the log of the run is in ~/.anet/logs, 0600 in a 0700
// directory, and holds the details.
func checkLog(t *testing.T, home string, wants ...string) string {
	t.Helper()
	logs, _ := filepath.Glob(filepath.Join(home, ".anet", "logs", "install-*.log"))
	if len(logs) != 1 {
		t.Fatalf("logs: %v", logs)
	}
	if fi, err := os.Stat(filepath.Dir(logs[0])); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("log directory mode %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(logs[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v %v", fi.Mode(), err)
	}
	b, _ := os.ReadFile(logs[0])
	mustContain(t, "log", string(b), wants...)
	return string(b)
}

// Spec C/E: taking over AgentNetwork 1.x — an npm installation with its
// daemon and data, and a 1.x binary that only root can remove (sudo needs a
// password, and there is no terminal): one phase line, one takeover line,
// one Note, and the PATH this shell needs, since a command it may have
// remembered was removed.
func TestInstallShGoldenTakeover(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write every directory")
	}
	ver := installedVersion(t)
	r := newSHRelease(t, ver)
	hits := newHitCount()
	srv := r.server(hits)
	e := e2eEnv(t, srv, false)
	e.fakeSudo()
	prefix := filepath.Join(e.root, "npm-global")
	link, pkg, platBin := e.npmTree(prefix, "1.1.15", "https://"+legacyRepo)
	e.fakeNpm(prefix, false)
	sys := e.fakeAnet(filepath.Join(e.root, "sysbin", "anet"), markerV1, "1.1.15")
	if err := os.Chmod(filepath.Dir(sys), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(sys), 0o755) })
	e.path = append(e.path, filepath.Join(prefix, "bin"), filepath.Dir(sys))
	data := filepath.Join(e.home, ".anet")
	for name, body := range map[string]string{"api_token": "t", "anet/identity.json": "{}", "anet.db": "db"} {
		e.write(filepath.Join(data, filepath.FromSlash(name)), body, 0o600)
	}
	script := e2eScript(t, r, e2eSources(srv, true), e.root, freePort(t))
	res := runE2E(t, e, script)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	moved, _ := filepath.Glob(data + "-legacy-1.1.15-*")
	if len(moved) != 1 {
		t.Fatalf("moved: %v", moved)
	}
	want := "Downloaded anet " + ver + " for " + runtime.GOOS + "-" + runtime.GOARCH + " from <src> (<size> MB in <n> s)\n" +
		"Replacing AgentNetwork 1.1.15\n" +
		"Starting the node and joining https://hub.agentnetwork.org.cn\n" +
		"\n" +
		"anet " + ver + " is installed and running.\n" +
		"  AID   " + e2eAID + "\n" +
		"  Hub   https://hub.agentnetwork.org.cn (joined)\n" +
		"Replaced AgentNetwork 1.1.15; its data is kept in ~/.anet-legacy-1.1.15-<UTC>\n" +
		"Note: <root>/sysbin/anet (AgentNetwork 1.1.15, owned by root) is no longer used; remove it with: sudo rm <root>/sysbin/anet\n" +
		"Run 'anet' in a new terminal, or in this one after: export PATH=\"$HOME/.local/bin:$PATH\"  Details: ~/.anet/logs/install-<UTC>.log\n"
	got := regexp.MustCompile(`from (npmmirror|GitHub) \(`).ReplaceAllString(normalize(res.stdout, e), "from <src> (")
	if got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}
	t.Logf("1.x takeover, as printed:\n%s", res.stdout)
	checkLog(t, e.home, "stopping the daemon of AgentNetwork 1.1.15", "moving the data of the earlier release aside",
		"npm package @agentnetwork/anet 1.1.15", "Upgraded from anet 1.1.15")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the npm command link is still there: %v", err)
	}
	if _, err := os.Stat(pkg); !os.IsNotExist(err) {
		t.Errorf("the npm package is still there: %v", err)
	}
	mustContain(t, "calls", e.calls(), platBin+" stop\n")
	for _, l := range strings.Split(strings.TrimSpace(e.calls()), "\n") {
		if strings.HasPrefix(l, "sudo ") && !strings.HasPrefix(l, "sudo -n ") {
			t.Errorf("sudo without -n and without a terminal: %q", l)
		}
	}
}

// Spec A: a source whose release.json does not verify (a signature that is
// not the release key's, here from the npm tarball and from the files of
// GitHub, or a correctly signed manifest that has expired) is passed over,
// with a line in the log, for the next source, which is checked in the
// same way. Spec C: when no source serves a release that verifies, the
// install ends, names the sources, says what to do and names the log;
// nothing is installed.
func TestInstallShBadSignatureSkipsSource(t *testing.T) {
	ver := installedVersion(t)
	r := newSHRelease(t, ver)
	bad := *r
	bad.sig = signSSHSIG(r.priv, []byte("something else"), Namespace)
	badSrv := bad.server(newHitCount())
	stale := *r
	stale.manifest = regexp.MustCompile(`"expires_at": "[^"]*"`).ReplaceAll(r.manifest, []byte(`"expires_at": "2020-01-01T00:00:00Z"`))
	stale.sig = signSSHSIG(r.priv, stale.manifest, Namespace)
	staleSrv := stale.server(newHitCount())
	hits := newHitCount()
	srv := r.server(hits)
	host := strings.TrimPrefix(srv.URL, "https://")
	badHost, staleHost := strings.TrimPrefix(badSrv.URL, "https://"), strings.TrimPrefix(staleSrv.URL, "https://")

	e := e2eEnv(t, srv, false)
	sources := `  PROBED="npm|npmmirror|` + badSrv.URL + `/npm|` + badHost + `"
  REST="files|GitHub|` + badSrv.URL + `/files|` + badHost + `
files|jsDelivr|` + staleSrv.URL + `/files|` + staleHost + `
files|agentnetwork.org.cn|` + srv.URL + `/files|` + host + `"`
	res := runE2E(t, e, e2eScript(t, r, sources, e.root, freePort(t)), "--no-start")
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "from agentnetwork.org.cn (", "anet "+ver+" is installed.")
	checkLog(t, e.home,
		"  npmmirror: release.json is not signed by the anet release key (", "; passed over\n",
		"  GitHub: release.json is not signed by the anet release key (",
		"  jsDelivr: the release manifest expired at 2020-01-01T00:00:00Z",
		"using agentnetwork.org.cn: anet "+ver)
	if _, err := os.Stat(filepath.Join(e.home, ".local", "bin", "anet")); err != nil {
		t.Errorf("not installed from the source that verifies: %v", err)
	}

	e2 := e2eEnv(t, srv, false)
	sources = `  PROBED="npm|npmmirror|` + badSrv.URL + `/npm|` + badHost + `"
  REST="files|GitHub|` + badSrv.URL + `/files|` + badHost + `
files|jsDelivr|` + staleSrv.URL + `/files|` + staleHost + `"`
	res = runE2E(t, e2, e2eScript(t, r, sources, e2.root, freePort(t)), "--no-start")
	if res.code != 1 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stderr", res.stderr, "Error: no download source served a release manifest that verifies with the anet release key (",
		"; not verified: npmmirror, GitHub, jsDelivr.\n", "Name another source with --base URL.",
		"Nothing was installed or changed.\nDetails: ~/.anet/logs/install-")
	if _, err := os.Stat(filepath.Join(e2.home, ".local", "bin", "anet")); err == nil {
		t.Error("installed although no source verifies")
	}
	checkLog(t, e2.home, "source npmmirror", "  jsDelivr: the release manifest expired")
}

// jsDelivr (kind cdn): the latest version is read from
// <package>@latest/package.json, and release.json, its signature and the
// .gz are then fetched under @<version>, never under @latest (where a cache
// could pair the manifest with the signature of another version).
func TestInstallShCDNResolvesTheVersionFirst(t *testing.T) {
	ver := installedVersion(t)
	r := newSHRelease(t, ver)
	hits := newHitCount()
	root := "/jsd/" + r.pkg
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		hits.add(q.URL.Path)
		switch q.URL.Path {
		case root + "@latest/package.json":
			fmt.Fprintf(w, `{"name": %q, "version": %q}`, r.pkg, r.version)
		case root + "@" + r.version + "/dl/release.json":
			w.Write(r.manifest)
		case root + "@" + r.version + "/dl/release.json.sig":
			w.Write(r.sig)
		case root + "@" + r.version + "/dl/" + r.asset + ".gz":
			w.Write(r.gz)
		case root + "@latest/dl/release.json", root + "@latest/dl/release.json.sig":
			w.Write(signSSHSIG(r.priv, []byte("another version"), Namespace))
		default:
			http.NotFound(w, q)
		}
	}))
	t.Cleanup(cdn.Close)
	host := strings.TrimPrefix(cdn.URL, "https://")
	e := e2eEnv(t, cdn, false)
	sources := `  PROBED="npm|npmmirror|https://127.0.0.1:1/npm|127.0.0.1:1"
  REST="cdn|jsDelivr|` + cdn.URL + root + `|` + host + `"`
	res := runE2E(t, e, e2eScript(t, r, sources, e.root, freePort(t)), "--no-start")
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "from jsDelivr (")
	checkLog(t, e.home, "  jsDelivr: anet "+ver+", "+cdn.URL+root+"@"+ver+"/dl\n", "using jsDelivr: anet "+ver)
	for _, p := range []string{root + "@latest/dl/release.json", root + "@latest/dl/release.json.sig"} {
		if hits.get(p) != 0 {
			t.Errorf("%s was fetched: %v", p, hits.String())
		}
	}
	for _, p := range []string{root + "@latest/package.json", root + "@" + ver + "/dl/release.json", root + "@" + ver + "/dl/" + r.asset + ".gz"} {
		if hits.get(p) != 1 {
			t.Errorf("%s: %d requests: %v", p, hits.get(p), hits.String())
		}
	}
}

// Spec A: a source that does not serve (here an npm registry that answers
// 404 for the package) is passed over; a lagging mirror (an older release
// than this installer) is passed over while a later source has the
// current one; --no-join starts the node and joins nothing; --no-start
// starts nothing.
func TestInstallShSourceFallback(t *testing.T) {
	ver := installedVersion(t)
	r := newSHRelease(t, ver)
	old := newSHRelease(t, "0.1.0")
	old.pub, old.priv = r.pub, r.priv
	old.sig = signSSHSIG(r.priv, old.manifest, Namespace)
	hits := newHitCount()
	srv := r.server(hits)
	oldHits := newHitCount()
	oldSrv := old.server(oldHits)
	e := e2eEnv(t, srv, false)
	// Both test servers have certificates of the same test CA.
	host, oldHost := strings.TrimPrefix(srv.URL, "https://"), strings.TrimPrefix(oldSrv.URL, "https://")
	sources := `  PROBED="npm|npmmirror|` + srv.URL + `/nothing|` + host + `"
  REST="files|jsDelivr|` + oldSrv.URL + `/files|` + oldHost + `
files|agentnetwork.org.cn|` + srv.URL + `/files|` + host + `"`
	script := e2eScript(t, r, sources, e.root, freePort(t))
	res := runE2E(t, e, script, "--no-join")
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "from agentnetwork.org.cn (", "Starting the node\n",
		"  Hub   not joined (--no-join)\n", "is installed and running.")
	mustNotContain(t, "calls", res.calls, "hub-register")
	logText := checkLog(t, e.home, "jsDelivr: release 0.1.0, older than "+ver+"; passed over", "using agentnetwork.org.cn")
	_ = logText

	e2 := e2eEnv(t, srv, false)
	res = runE2E(t, e2, e2eScript(t, r, e2eSources(srv, false), e2.root, freePort(t)), "--no-start")
	if res.code != 0 {
		t.Fatalf("--no-start: exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	mustContain(t, "stdout", res.stdout, "anet "+ver+" is installed.\nRun 'anet up' to get started.")
	mustNotContain(t, "calls", res.calls, "anet up", "hub-register")
}

// Spec C: when the hub admits by AID, the summary says so in one line with
// the operator's command; in Chinese with ANET_LANG=zh.
func TestInstallShGoldenPendingZh(t *testing.T) {
	ver := installedVersion(t)
	r := newSHRelease(t, ver)
	srv := r.server(newHitCount())
	e := e2eEnv(t, srv, false)
	e.env["ANET_LANG"] = "zh_CN.UTF-8"
	e.env["FAKE_HUB"] = "pending"
	res := runE2E(t, e, e2eScript(t, r, e2eSources(srv, false), e.root, freePort(t)), "--hub", "https://hub.example.test")
	if res.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", res.code, res.stdout, res.stderr)
	}
	want := "已下载 anet " + ver + "（" + runtime.GOOS + "-" + runtime.GOARCH + "，来源：npmmirror，<size> MB，用时 <n> 秒）\n" +
		"正在启动节点并加入 https://hub.example.test\n" +
		"\n" +
		"anet " + ver + " 已安装并在运行。\n" +
		"  AID   " + e2eAID + "\n" +
		"  Hub   https://hub.example.test （等待准入；由其运营者运行：anet-hub admit " + e2eAID + "）\n" +
		"运行 'anet' 开始使用。  详情：~/.anet/logs/install-<UTC>.log\n"
	got := normalize(res.stdout, e)
	got = regexp.MustCompile(`[\d.]+ MB，用时 \d+ 秒`).ReplaceAllString(got, "<size> MB，用时 <n> 秒")
	if got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}
}

// Owner decision (2026-10-20) and spec E: on a terminal, one line says why,
// sudo asks for the password once (sudo -v), and within that session the
// 1.x binary only root could remove is removed and the command link is
// made in its place, so that `anet` runs in this shell at once. Without
// ~/.local/bin on PATH and without a terminal, nothing is asked.
func TestInstallShRootSession(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write every directory")
	}
	e := newLegacyEnv(t)
	linkDir := filepath.Join(e.root, "usr-local-bin")
	sys := e.fakeAnet(filepath.Join(linkDir, "anet"), markerV1, "1.1.5")
	if err := os.Chmod(linkDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(linkDir, 0o755) })
	// sudo: -v opens a session (logged); -n works within one, as root would:
	// it makes the root-owned directory writable for the command it runs.
	session := filepath.Join(e.root, "sudo-session")
	e.write(filepath.Join(e.bin, "sudo"), "#!/bin/sh\necho \"sudo $*\" >> '"+e.log+"'\n"+
		"case \"$1\" in\n  -v) touch '"+session+"'; exit 0 ;;\n  -n) shift; [ -e '"+session+"' ] || exit 1\n"+
		"     chmod u+w '"+linkDir+"'; \"$@\"; rc=$?; chmod 555 '"+linkDir+"'; exit $rc ;;\nesac\nexit 1\n", 0o755)
	e.path = append([]string{linkDir}, e.path...)
	body := `TTY_DEV=/dev/null
LINK_DIR='` + linkDir + `'; USER_MODE=1; OS_TAG=darwin
mkdir -p "$PREFIX"; printf '#!/bin/sh\n' > "$DEST"; chmod 755 "$DEST"
survey_anets; survey_units; root_session; replace_legacy_bins; command_link
echo "LINKED=$LINKED"; cat "$TMP/notes"`
	out, code := e.run(body)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	mustContain(t, "output", out, "Removing AgentNetwork 1.1.5 from "+linkDir+" requires administrator rights; macOS will ask for your password once.\n",
		"LINKED=1", "removed|"+sys+" (AgentNetwork 1.1.5) (sudo -n)", "linked|"+sys+" → ")
	if n := strings.Count(e.calls(), "sudo -v\n"); n != 1 {
		t.Errorf("sudo -v ran %d times:\n%s", n, e.calls())
	}
	if got, err := os.Readlink(sys); err != nil || got != filepath.Join(e.home, ".local", "bin", "anet") {
		t.Errorf("%s: link %q, %v", sys, got, err)
	}

	// ~/.local/bin on PATH: no link, no prompt.
	e2 := newLegacyEnv(t)
	e2.path = append([]string{filepath.Join(e2.home, ".local", "bin"), filepath.Join(e2.root, "lb")}, e2.path...)
	e2.write(filepath.Join(e2.bin, "sudo"), "#!/bin/sh\necho \"sudo $*\" >> '"+e2.log+"'\nexit 1\n", 0o755)
	out, _ = e2.run(`TTY_DEV=/dev/null
LINK_DIR='` + filepath.Join(e2.root, "lb") + `'; mkdir -p "$LINK_DIR"; USER_MODE=1; OS_TAG=linux
mkdir -p "$PREFIX"; printf '#!/bin/sh\n' > "$DEST"; chmod 755 "$DEST"
survey_anets; survey_units; root_session; command_link; echo "LINKED=$LINKED"`)
	mustContain(t, "output", out, "LINKED=0")
	mustNotContain(t, "output", out, "administrator rights")
	if strings.Contains(e2.calls(), "sudo -v") {
		t.Errorf("sudo asked: %s", e2.calls())
	}

	// No terminal: no prompt, the Note stays.
	e3 := newLegacyEnv(t)
	ro := filepath.Join(e3.root, "ro")
	sys3 := e3.fakeAnet(filepath.Join(ro, "anet"), markerV1, "1.1.5")
	os.Chmod(ro, 0o555)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	e3.fakeSudo()
	e3.path = append([]string{ro}, e3.path...)
	out, _ = e3.run(`LINK_DIR='` + ro + `'; USER_MODE=1; OS_TAG=linux
mkdir -p "$PREFIX"; printf '#!/bin/sh\n' > "$DEST"; chmod 755 "$DEST"
survey_anets; survey_units; root_session; replace_legacy_bins; command_link; cat "$TMP/notes"`)
	mustContain(t, "output", out, "manual|"+sys3+" (AgentNetwork 1.1.5, owned by root) is no longer used; remove it with: sudo rm "+sys3)
	if strings.Contains(e3.calls(), "sudo -v") {
		t.Errorf("sudo asked without a terminal: %s", e3.calls())
	}
}

// A systemd unit anet-daemon (the unit 1.x prints for its users to
// install, with Restart=always) that runs a 1.x binary is disabled before
// the daemon is stopped; one that runs something else is left alone.
func TestInstallShDisablesV1Unit(t *testing.T) {
	e := newLegacyEnv(t)
	v1 := e.fakeAnet(filepath.Join(e.root, "opt", "anet"), markerV1, "1.1.15")
	e.write(filepath.Join(e.home, ".config", "systemd", "user", "anet-daemon.service"),
		"[Service]\nExecStart="+v1+" daemon\nRestart=always\n", 0o644)
	e.write(filepath.Join(e.root, "etc-systemd", "anet-daemon.service"), "[Service]\nExecStart=/usr/bin/true daemon\n", 0o644)
	e.write(filepath.Join(e.bin, "systemctl"), "#!/bin/sh\necho \"systemctl $*\" >> '"+e.log+"'\n", 0o755)
	out, code := e.run(`OS_TAG=linux; UNIT_DIRS='` + filepath.Join(e.root, "etc-systemd") + `'
survey_units; stop_autostart; cat "$TMP/notes"`)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	mustContain(t, "calls", e.calls(), "systemctl --user disable --now anet-daemon.service\n")
	mustContain(t, "notes", out, "stopped|the systemd user unit anet-daemon")
	mustNotContain(t, "calls", e.calls(), "sudo")
}

// install.sh lists the same sources, in the same order, with the same
// hosts, as `anet update` (release.Sources); --base alone replaces them.
func TestInstallShSourcesMatchUpdate(t *testing.T) {
	for _, c := range []struct{ flags, variant, goos, goarch string }{
		{"", "default", "linux", "amd64"},
		{"--shell", "shell", "darwin", "arm64"},
	} {
		stdout, stderr, code := runInstallSnippet(t, []string{"say", "warn", "die", "usage", "removed_flag", "add_without", "parse_args", "source_defaults"},
			"parse_args --prefix /x "+c.flags+"; OS_TAG="+c.goos+"; ARCH_TAG="+c.goarch+
				`; case "$ARCH_TAG" in amd64) _cpu=x64 ;; *) _cpu=$ARCH_TAG ;; esac; PKG="@agentnetwork/${ASSET_PREFIX}-${OS_TAG}-${_cpu}"
source_defaults; printf '%s\n--\n%s\n' "$PROBED" "$REST"`)
		if code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", c.variant, code, stderr)
		}
		probed, rest := Sources(c.variant, c.goos, c.goarch)
		line := func(s Source) string { return s.Kind + "|" + s.Name + "|" + s.URL + "|" + strings.Join(s.Allow, " ") }
		var want []string
		for _, s := range probed {
			want = append(want, line(s))
		}
		want = append(want, "--")
		for _, s := range rest {
			want = append(want, line(s))
		}
		if got := strings.Split(strings.TrimSpace(stdout), "\n"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: install.sh sources\n %q\nanet update sources\n %q", c.variant, got, want)
		}
	}
	stdout, _, code := runInstallSnippet(t, []string{"say", "warn", "die", "usage", "removed_flag", "add_without", "parse_args", "order_sources"},
		`TMP=$(mktemp -d); parse_args --prefix /x --base https://example.org/; order_sources; cat "$TMP/sources"; rm -rf "$TMP"`)
	if code != 0 || strings.TrimSpace(stdout) != "files|https://example.org|https://example.org/dl|*" {
		t.Errorf("--base: exit %d, sources %q", code, stdout)
	}
}

// installer_version is the version of the release the installer belongs
// to; it moves with internal/version.V.
func TestInstallShVersionIsTheRelease(t *testing.T) {
	if got := installedVersion(t); got != anetversion.V {
		t.Errorf("install.sh installer_version %s, internal/version.V %s", got, anetversion.V)
	}
}

// fetch follows a redirect only to a host the source uses: an https URL
// whose host FETCH_ALLOW names.
func TestInstallShAllowURL(t *testing.T) {
	stdout, _, _ := runInstallSnippet(t, []string{"url_host", "allow_url"}, `
for u in https://github.com/x https://objects.githubusercontent.com/y https://release-assets.githubusercontent.com/z?sig=1 \
         https://githubusercontent.com/x https://evil.com/github.com http://github.com/x https://user@github.com/x \
         https://GitHub.com/x https://github.com:443/x https://evilgithub.com/x; do
  FETCH_ALLOW='github.com *.githubusercontent.com'
  if allow_url "$u"; then echo "yes $u"; else echo "no $u"; fi
done
FETCH_ALLOW='*'; allow_url https://anything.example/x && echo "yes any"`)
	want := "yes https://github.com/x\nyes https://objects.githubusercontent.com/y\nyes https://release-assets.githubusercontent.com/z?sig=1\n" +
		"no https://githubusercontent.com/x\nno https://evil.com/github.com\nno http://github.com/x\nno https://user@github.com/x\n" +
		"yes https://GitHub.com/x\nno https://github.com:443/x\nno https://evilgithub.com/x\nyes any\n"
	if stdout != want {
		t.Errorf("allow_url:\n%s\nwant:\n%s", stdout, want)
	}
}

// TestInstallShRealBinary runs install.sh end to end with a real anet build
// (ANET_INSTALL_E2E_BIN: a binary built with -X …/version.Variant=default),
// signed by a test key and served by the local test sources, in a temporary
// HOME, with --no-join: the real `anet version`, `help`, `init`, `up` and
// `status` behind the summary. The node it starts is stopped again with
// `anet stop` in that HOME. Skipped unless the variable is set.
func TestInstallShRealBinary(t *testing.T) {
	bin := os.Getenv("ANET_INSTALL_E2E_BIN")
	if bin == "" {
		t.Skip("ANET_INSTALL_E2E_BIN is not set")
	}
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	ver, mods, err := ParseVersionOutput(string(out))
	if err != nil {
		t.Fatal(err)
	}
	r := newSHRelease(t, ver)
	r.bin = body
	var zb bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zb, 6)
	zw.Write(r.bin)
	zw.Close()
	r.gz = zb.Bytes()
	r.manifest = regexp.MustCompile(`"gz_sha256": "[0-9a-f]+", "gz_size": \d+, "sha256": "[0-9a-f]+", "size": \d+`).ReplaceAll(r.manifest,
		[]byte(fmt.Sprintf(`"gz_sha256": "%s", "gz_size": %d, "sha256": "%s", "size": %d`, hexsum(r.gz), len(r.gz), hexsum(r.bin), len(r.bin))))
	quoted := make([]string, len(mods))
	for i, m := range mods {
		quoted[i] = `"` + m + `"`
	}
	r.manifest = bytes.Replace(r.manifest, []byte(`"modules": ["cas", "mcp", "x402"]`), []byte(`"modules": [`+strings.Join(quoted, ", ")+`]`), 1)
	r.sig = signSSHSIG(r.priv, r.manifest, Namespace)
	srv := r.server(newHitCount())
	e := e2eEnv(t, srv, false)
	script := e2eScript(t, r, e2eSources(srv, true), e.root, freePort(t))
	if v := installedVersion(t); v != ver {
		script = strings.Replace(script, "installer_version() { echo '"+v+"'; }", "installer_version() { echo '"+ver+"'; }", 1)
	}
	res := runE2E(t, e, script, "--no-join")
	dest := filepath.Join(e.home, ".local", "bin", "anet")
	t.Cleanup(func() {
		c := exec.Command(dest, "stop")
		c.Env = []string{"HOME=" + e.home, "PATH=/usr/bin:/bin"}
		c.Run()
	})
	t.Logf("exit %d\nstdout:\n%s\nstderr:\n%s", res.code, res.stdout, res.stderr)
	if res.code != 0 {
		t.Fatal("install failed")
	}
	mustContain(t, "stdout", res.stdout, "anet "+ver+" is installed and running.", "  AID   ", "  Hub   not joined (--no-join)")
	logText := checkLog(t, e.home, "anet init")
	t.Logf("log:\n%s", logText)
	bare := exec.Command(dest)
	bare.Env = []string{"HOME=" + e.home, "PATH=" + filepath.Dir(dest) + ":/usr/bin:/bin", "ANET_LANG=en"}
	b, _ := bare.CombinedOutput()
	t.Logf("bare `anet` prints:\n%s", b)
}
