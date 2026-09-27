package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANetCore/anetcid"
	"github.com/ANetResearch/ANetCore/delegation"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// SI-7 / §7.5: /console carries no control token, runs its scripts only with the response nonce, and
// sends the page security headers.
func TestConsolePageHasNoTokenAndStrictHeaders(t *testing.T) {
	p := newPlane(t)
	p.d.mu.Lock()
	p.d.cfg.HubURL = "https://hub.example.org:8443/some/path"
	p.d.cfg.Name = `a</script><script>alert(1)</script>`
	p.d.mu.Unlock()
	resp, body := p.req(t, "GET", "/console", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("/console = %d", resp.StatusCode)
	}
	if bytes.Contains(body, []byte(p.token)) {
		t.Fatal("the console page contains the control token")
	}
	if bytes.Contains(body, []byte("Bearer")) || bytes.Contains(body, []byte("A.token")) {
		t.Fatal("the console page still builds a bearer header")
	}
	csp := resp.Header.Get("Content-Security-Policy")
	m := regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("CSP without a script nonce: %q", csp)
	}
	nonce := m[1]
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'",
		"connect-src 'self' https://hub.example.org:8443", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") && strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP allows inline script: %q", csp)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q", got)
	}
	// Every script element carries the nonce.
	scripts := regexp.MustCompile(`<script[^>]*>`).FindAll(body, -1)
	if len(scripts) < 2 {
		t.Fatalf("expected the bootstrap and the page script, found %d", len(scripts))
	}
	for _, s := range scripts {
		if !bytes.Contains(s, []byte(`nonce="`+nonce+`"`)) {
			t.Errorf("script without the nonce: %s", s)
		}
	}
	// No inline event handler attribute (a nonce does not cover those).
	if regexp.MustCompile(`(?i)<[^>]+\son[a-z]+\s*=`).Match(body) {
		t.Error("the page has an inline event handler attribute")
	}
	// The bootstrap carries aid/name/hub/nonce and nothing else, and the name cannot end the script.
	bm := regexp.MustCompile(`window\.__ANET = (\{.*?\});</script>`).FindSubmatch(body)
	if bm == nil {
		t.Fatal("no window.__ANET bootstrap")
	}
	var boot map[string]any
	if err := json.Unmarshal(bm[1], &boot); err != nil {
		t.Fatalf("bootstrap is not JSON: %v", err)
	}
	for k := range boot {
		switch k {
		case "aid", "name", "hub", "nonce":
		default:
			t.Errorf("bootstrap carries %q", k)
		}
	}
	if boot["aid"] != p.d.AID() || boot["nonce"] != nonce {
		t.Errorf("bootstrap %v", boot)
	}
	if bytes.Contains(body, []byte(`a</script><script>alert(1)`)) {
		t.Error("the name ended the bootstrap script")
	}
	// The page drives the API with the session cookie and the CSRF header, has no guest code, and uses
	// the four-type image allowlist.
	for _, want := range []string{`"X-Anet-CSRF": CSRF`, `/console/session`, `/console/switch`,
		`"image/png", "image/jpeg", "image/gif", "image/webp"`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("page lacks %s", want)
		}
	}
	for _, gone := range []string{"/guest/", "附完整交互内容", `ctl("/hub-register"`, `ctl("/end-accept"`,
		`indexOf("image/") === 0`, "内容已验证"} {
		if bytes.Contains(body, []byte(gone)) {
			t.Errorf("page still contains %q", gone)
		}
	}
}

// hubOrigin keeps only a well-formed origin in the CSP.
func TestHubOriginForCSP(t *testing.T) {
	for in, want := range map[string]string{
		"https://agentnetwork.org.cn":         "https://agentnetwork.org.cn",
		"http://127.0.0.1:8088/":              "http://127.0.0.1:8088",
		"https://h.example/x?y=1":             "https://h.example",
		"https://u:p@h.example":               "",
		"javascript:alert(1)":                 "",
		"https://h.example; script-src *":     "",
		"":                                    "",
		"https://[2001:db8::1]:8443/relay/v2": "https://[2001:db8::1]:8443",
		"https://a;script-src.example":        "", // url.Parse accepts ';' in a host
		"https://a'b.example":                 "",
	} {
		if got := hubOrigin(in); got != want {
			t.Errorf("hubOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// storeAttachment stores one attachment for interaction ix through the normal store path.
func storeAttachment(t *testing.T, d *Daemon, ix, name, declared string, data []byte) string {
	t.Helper()
	cid, err := anetcid.SumRaw(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.storeMsgAttachments(ix, 1, []delegation.Attachment{
		{Name: name, Mime: declared, Size: int64(len(data)), CID: cid, Data: data},
	}); err != nil {
		t.Fatal(err)
	}
	return cid
}

var (
	svgWithScript = []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>fetch("/pull")</script></svg>`)
	pngHeader     = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
)

// §7.6 / C11: an SVG with a script, declared image/svg+xml by the peer, is stored with its sniffed
// type and served as a download with the sandbox CSP; it is never rendered inline.
func TestAttachmentSVGWithScriptIsServedAsADownload(t *testing.T) {
	p := newPlane(t)
	cid := storeAttachment(t, p.d, "ix-svg", "pic.svg", "image/svg+xml", svgWithScript)
	a, err := p.d.ix.AttachmentData("ix-svg", cid)
	if err != nil {
		t.Fatal(err)
	}
	if a.Mime == "image/svg+xml" || strings.HasPrefix(a.Mime, "image/") {
		t.Errorf("stored type %q follows the peer's declaration, want the sniffed type", a.Mime)
	}
	// A row stored before types were sniffed on receipt still carries the peer's declaration; the
	// handler must not trust it either.
	legacy := svgWithScript[:len(svgWithScript)-6]
	legacyCID, _ := anetcid.SumRaw(legacy)
	if err := p.d.ix.AddAttachment("ix-svg", 1, interactions.Attachment{Name: "old.svg", Mime: "image/svg+xml",
		Size: int64(len(legacy)), CID: legacyCID, Data: legacy}); err != nil {
		t.Fatal(err)
	}
	lresp, _ := p.req(t, "GET", "/attachment?interaction_id=ix-svg&cid="+legacyCID, "", p.bearer)
	if ct := lresp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("a legacy row declared image/svg+xml was served as %q", ct)
	}
	s := p.session(t)
	resp, body := p.req(t, "GET", "/attachment?interaction_id=ix-svg&cid="+cid, "", func(r *http.Request) {
		r.AddCookie(s.cookie) // an <img>/<a> load: cookie only, no CSRF header
	})
	if resp.StatusCode != 200 || !bytes.Equal(body, svgWithScript) {
		t.Fatalf("/attachment = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition %q", cd)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "sandbox") {
		t.Errorf("CSP %q", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff")
	}
	if cc := resp.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") || strings.Contains(cc, "max-age=31536000") {
		t.Errorf("Cache-Control %q keeps the long immutable cache", cc)
	}
}

// Only the four raster types are inline; HTML and text are downloads too.
func TestAttachmentInlineOnlyForRasterImages(t *testing.T) {
	for _, c := range []struct {
		name   string
		data   []byte
		inline string
	}{
		{"a.png", pngHeader, "image/png"},
		{"a.gif", []byte("GIF89a" + strings.Repeat("\x00", 20)), "image/gif"},
		{"a.jpg", []byte("\xff\xd8\xff\xe0" + strings.Repeat("\x00", 20)), "image/jpeg"},
		{"a.html", []byte("<html><script>1</script></html>"), ""},
		{"a.txt", []byte("hello"), ""},
		{"a.svg", svgWithScript, ""},
	} {
		rec := httptest.NewRecorder()
		writeAttachment(rec, c.name, c.data)
		ct := rec.Header().Get("Content-Type")
		cd := rec.Header().Get("Content-Disposition")
		if c.inline != "" && (ct != c.inline || cd != "") {
			t.Errorf("%s: %q %q, want inline %s", c.name, ct, cd, c.inline)
		}
		if c.inline == "" && (ct != "application/octet-stream" || !strings.HasPrefix(cd, "attachment")) {
			t.Errorf("%s: %q %q, want a download", c.name, ct, cd)
		}
		if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: no sandbox CSP", c.name)
		}
	}
}

// --- /pull ---

func pullAtt(name string, data []byte) *interactions.Attachment {
	cid, _ := anetcid.SumRaw(data)
	return &interactions.Attachment{Name: name, Mime: sniffMime(data), Size: int64(len(data)), CID: cid, Data: data}
}

func TestPullRefusesEmptyRelativeAndProtectedOutDirs(t *testing.T) {
	d := newBareDaemon(t)
	for _, out := range []string{"", "  ", "relative/dir", "."} {
		if _, err := d.Pull("ix", out); err == nil {
			t.Errorf("out_dir %q accepted", out)
		}
	}
	if _, err := d.Pull("ix", d.layout.Root); err == nil {
		t.Error("the data dir accepted as out_dir")
	}
	if _, err := d.Pull("ix", filepath.Join(d.layout.Root, "sub", "new")); err == nil {
		t.Error("a new dir inside the data dir accepted")
	}
	link := filepath.Join(t.TempDir(), "looks-harmless")
	if err := os.Symlink(d.layout.Root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pull("ix", link); err == nil {
		t.Error("a symlink to the data dir accepted as out_dir")
	}
	if _, err := d.Pull("ix", filepath.Join(link, "deeper")); err == nil {
		t.Error("a path through a symlink into the data dir accepted")
	}
	work := execWorkRoot()
	if _, err := d.Pull("ix", filepath.Join(work, "x")); err == nil {
		t.Error("the exec work dir accepted as out_dir")
	}
	// The control route answers 400 for these.
	p := newPlaneFor(t, d, "tok")
	resp, _ := p.req(t, "POST", "/pull", `{"interaction_id":"ix","out_dir":""}`, p.bearer)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/pull with empty out_dir = %d", resp.StatusCode)
	}
}

func TestPullWritesIntoAFreshSubdirAndNeverOverwrites(t *testing.T) {
	out := t.TempDir()
	// A file of the user's with the same name as the attachment, in the out dir itself.
	if err := os.WriteFile(filepath.Join(out, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := pullInto(out, "ix0123456789abcdef", []*interactions.Attachment{pullAtt("notes.txt", []byte("peer"))})
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(out, "anet-ix0123456789")
	if len(res) != 1 || res[0].Path != filepath.Join(sub, "notes.txt") {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "notes.txt")); string(b) != "mine" {
		t.Fatal("the user's file was changed")
	}
	// An existing file in the subdir with other content is not overwritten; the next name is used.
	if err := os.WriteFile(filepath.Join(sub, "report.pdf"), []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = pullInto(out, "ix0123456789abcdef", []*interactions.Attachment{pullAtt("report.pdf", []byte("newer"))})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(sub, "report.pdf")); string(b) != "older" {
		t.Fatal("an existing file was overwritten")
	}
	if res[0].Path != filepath.Join(sub, "report-1.pdf") {
		t.Fatalf("want report-1.pdf, got %s", res[0].Path)
	}
}

func TestPullDoesNotFollowASymlinkAtTheDestination(t *testing.T) {
	out := t.TempDir()
	sub := filepath.Join(out, "anet-ixsymlink")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(sub, "keys.txt")); err != nil {
		t.Fatal(err)
	}
	res, err := pullInto(out, "ixsymlink", []*interactions.Attachment{pullAtt("keys.txt", []byte("attacker key"))})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Fatal("the pull wrote through a symbolic link")
	}
	if fi, _ := os.Lstat(filepath.Join(sub, "keys.txt")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symbolic link was replaced")
	}
	if res[0].Path == filepath.Join(sub, "keys.txt") {
		t.Fatal("the result claims the symlinked path")
	}
	// A symbolic link in place of the per-interaction subdir is refused.
	out2 := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(out2, "anet-ixsubdir")); err != nil {
		t.Fatal(err)
	}
	if _, err := pullInto(out2, "ixsubdir", []*interactions.Attachment{pullAtt("a.txt", []byte("x"))}); err == nil {
		t.Fatal("a symlinked subdir was written into")
	}
}

func TestPullNeutralizesDotfilesAndRepullIsANoOp(t *testing.T) {
	out := t.TempDir()
	atts := []*interactions.Attachment{pullAtt(".envrc", []byte("export X=1")), pullAtt("a.txt", []byte("a"))}
	res, err := pullInto(out, "ixdot", atts)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Name != "_envrc" {
		t.Fatalf("dotfile name %q", res[0].Name)
	}
	sub := filepath.Join(out, "anet-ixdot")
	st1, _ := os.Stat(filepath.Join(sub, "a.txt"))
	time.Sleep(20 * time.Millisecond)
	res2, err := pullInto(out, "ixdot", atts)
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(sub)
	if len(ents) != 2 {
		t.Fatalf("a second pull wrote files: %d entries", len(ents))
	}
	for _, r := range res2 {
		if !r.AlreadyPresent {
			t.Errorf("%s not reported as already present", r.Name)
		}
	}
	st2, _ := os.Stat(filepath.Join(sub, "a.txt"))
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Error("the second pull rewrote a file")
	}
}

// The end-to-end pull through the store: sniffed types, per-interaction subdir.
func TestPullThroughTheStore(t *testing.T) {
	d := newBareDaemon(t)
	storeAttachment(t, d, "ix-e2e", "../../evil.png", "text/html", pngHeader)
	out := t.TempDir()
	res, err := d.Pull("ix-e2e", out)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Name != "evil.png" || res[0].Mime != "image/png" ||
		filepath.Dir(res[0].Path) != filepath.Join(out, "anet-ix-e2e") {
		t.Fatalf("result %+v", res)
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":          "report.pdf",
		"../../etc/passwd":    "passwd",
		`..\..\win.ini`:       "win.ini",
		".bashrc":             "_bashrc",
		"...hidden":           "_hidden",
		"invoice‮fdp.exe":     "invoicefdp.exe",
		"a\x00b\x1fc\x7f.txt": "abc.txt",
		"CON.txt":             "_CON.txt",
		"nul":                 "_nul",
		"trailing. . ":        "trailing",
		"":                    "attachment",
		".":                   "attachment",
		"..":                  "attachment",
		"/":                   "attachment",
		"‏x⁦y⁩.md":            "xy.md",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 200) + ".tar.gz"
	got := safeName(long)
	if len(got) > maxSafeNameBytes || !strings.HasSuffix(got, ".gz") || !utf8.ValidString(got) {
		t.Errorf("long name -> %q (%d bytes)", got, len(got))
	}
}

// Optional real-browser check (A2A-DESIGN §7.5): with the CSP in force, the console opened through a
// ticket starts a session and shows the directory fetched from the hub origin. Runs when a headless
// Chromium is available (ANET_TEST_CHROME, or the Playwright cache).
func TestConsoleLoadsTheDirectoryUnderItsCSP(t *testing.T) {
	chrome := os.Getenv("ANET_TEST_CHROME")
	if chrome == "" {
		home, _ := os.UserHomeDir()
		m, _ := filepath.Glob(filepath.Join(home, ".cache/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-linux64/chrome-headless-shell"))
		if len(m) > 0 {
			chrome = m[len(m)-1]
		}
	}
	if chrome == "" {
		t.Skip("no headless Chromium (set ANET_TEST_CHROME)")
	}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/graph":
			_, _ = w.Write([]byte(`{"nodes":[{"aid":"Eagent-directory-probe","name":"DirectoryProbeAgent","caps":["probe"],"summary":"csp check","avg_rating":0,"review_count":0}],"edges":[]}`))
		case "/stats":
			_, _ = w.Write([]byte(`{"agents":1,"tasks_completed":0,"reviews":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	p := newPlane(t)
	p.d.mu.Lock()
	p.d.cfg.HubURL = hub.URL
	p.d.cfg.Name = "CSPProbeNode"
	p.d.mu.Unlock()
	tk := p.ticket(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu",
		"--virtual-time-budget=8000", "--dump-dom", p.origin()+"/console#t="+tk).Output()
	if err != nil {
		t.Skipf("headless Chromium did not run: %v", err)
	}
	// Both strings below exist only in rendered markup, not in the page source: the status line is
	// built from the session answer and the agent name comes from the hub.
	dom := string(out)
	if !strings.Contains(dom, "已连接 · <b>CSPProbeNode</b>") {
		t.Fatal("the console did not start a session from the ticket")
	}
	// loadThreads marks the rail after a successful session call (cookie + CSRF accepted).
	if !strings.Contains(dom, `data-loaded="1"`) {
		t.Fatal("the console could not call the control API with its session")
	}
	if !strings.Contains(dom, "DirectoryProbeAgent") {
		t.Fatal("the directory from the hub did not render under the CSP")
	}
}
