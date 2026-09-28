package scripts

// These tests pin the SI-1 canary tooling (canary.py, the canary helpers in lib.sh) that joint.sh
// section C stands on. A search that cannot see an encoding, or that passes when it read nothing, would
// make every "0 hits" in a joint run meaningless; a tap that altered what it forwarded, or recorded it
// compressed, would do the same to the hub-traffic search and to the settlement check.

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func needPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
}

// canaryPy runs scripts/canary.py and returns its combined output and exit code.
func canaryPy(t *testing.T, args ...string) (string, int) {
	t.Helper()
	py, err := filepath.Abs("canary.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", append([]string{py}, args...)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("canary.py: %v\n%s", err, out)
	return "", -1
}

// testCanary has the shape canary.py mint gives (lib.sh canary_new): two random halves around "~?~".
const testCanary = "anet-canary-goal-0123456789ab~?~cdef01234567"

func writeCanaries(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "canaries.tsv")
	if err := os.WriteFile(p, []byte("goal\t"+testCanary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func noise(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131 + 7)
	}
	return b
}

func b64Nested(t *testing.T) []byte {
	t.Helper()
	inner := base64.StdEncoding.EncodeToString(append(append(noise(2), testCanary...), noise(5)...))
	return []byte(base64.StdEncoding.EncodeToString(append([]byte("{\"envelope\":\""), inner...)))
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The canary is found however the hub side might hold it: raw, hex, base64 at each of the three
// alignments (a sealed envelope travels as base64 inside JSON, and where the plaintext starts in it is
// arbitrary), URL-safe base64, base64 of base64, and inside a gzip file. Each case is a file of its own,
// so a case the search misses fails on its own.
func TestTheCanarySearchSeesEveryEncoding(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	canaries := writeCanaries(t, dir)
	raw := []byte(testCanary)
	cases := map[string][]byte{
		"raw":      append(append(noise(100), raw...), noise(50)...),
		"hex":      []byte(hex.EncodeToString(append(noise(9), raw...))),
		"HEX":      []byte(strings.ToUpper(hex.EncodeToString(raw))),
		"nested":   b64Nested(t),
		"gzip":     gzipped(t, append(append(noise(300), raw...), noise(300)...)),
		"b64url/0": []byte(base64.URLEncoding.EncodeToString(append([]byte{0xfb, 0xff, 0xfe}, raw...))),
	}
	for k := 0; k < 3; k++ {
		stream := append(append(noise(k), raw...), noise(7)...)
		cases[fmt.Sprintf("base64/%d", k)] = []byte(`{"envelope":"` + base64.StdEncoding.EncodeToString(stream) + `"}`)
	}
	for name, content := range cases {
		d := filepath.Join(dir, strings.ReplaceAll(name, "/", "-"))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "blob"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		out, code := canaryPy(t, "scan", "--canaries", canaries, "--label", name, d)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1 (found): %s", name, code, out)
		}
	}

	// And nothing is found where the canary is not: not its fixed prefix, not another canary. (The base64
	// forms leave out up to two bytes at either end, since those characters depend on the neighbouring
	// bytes; a canary's random part is 96 bits, so what is left cannot match by chance.)
	clean := filepath.Join(dir, "clean")
	if err := os.MkdirAll(clean, 0o700); err != nil {
		t.Fatal(err)
	}
	other := "anet-canary-goal-0123456789ab~?~cdXf01234567"
	if err := os.WriteFile(filepath.Join(clean, "blob"), []byte("anet-canary-goal- and "+other+" and "+
		base64.StdEncoding.EncodeToString([]byte(other))), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, clean); code != 0 {
		t.Errorf("a directory without the canary: exit %d, want 0: %s", code, out)
	}
}

// b64Aligned is canary.py's _b64_aligned: the encodings of raw that appear in enc(stream) wherever raw
// sits, one per alignment, whole 3-byte groups only.
func b64Aligned(raw []byte, enc *base64.Encoding) []string {
	var out []string
	for j := 0; j < 3; j++ {
		k := (len(raw) - j) / 3 * 3
		if k >= 12 {
			out = append(out, enc.EncodeToString(raw[j:j+k]))
		}
	}
	return out
}

// Every canary lib.sh mints differs between standard and URL-safe base64 at every alignment, so the
// URL-safe search is one a joint run depends on. With the old alphabet (letters, digits, '-') the two
// forms were the same bytes: canary.py dropped the URL-safe needles as duplicates, and a scanner that had
// lost them passed every run (docs/notes/0026 §6 item 4, mutation si1-6).
func TestCanaryNewMintsCanariesWhoseURLSafeBase64Differs(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "canaries.tsv")
	var minted []string
	for i := 0; i < 6; i++ {
		v := strings.TrimSpace(libsh(t, dir, `canary_new "$1" goal`, file))
		minted = append(minted, v)
	}
	shape := regexp.MustCompile(`^anet-canary-goal-[0-9a-f]{12}~\?~[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for _, v := range minted {
		if !shape.MatchString(v) {
			t.Fatalf("canary_new minted %q", v)
		}
		if seen[v] {
			t.Fatalf("canary_new minted %q twice", v)
		}
		seen[v] = true
		std, url := b64Aligned([]byte(v), base64.StdEncoding), b64Aligned([]byte(v), base64.URLEncoding)
		if len(std) != 3 {
			t.Fatalf("%q: %d alignments", v, len(std))
		}
		for j := range std {
			if std[j] == url[j] {
				t.Errorf("%q: standard and URL-safe base64 are the same at alignment %d (%s)", v, j, std[j])
			}
		}
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if want := "goal\t" + strings.Join(minted, "\ngoal\t") + "\n"; string(b) != want {
		t.Fatalf("canaries file:\n%s\nwant:\n%s", b, want)
	}
}

// Content that reaches the hub side as URL-safe base64 only is found, at each alignment; for this canary
// the standard needles alone would not find it.
func TestTheCanarySearchSeesURLSafeBase64(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	canaries := writeCanaries(t, dir)
	for k := 0; k < 3; k++ {
		stream := append(append(noise(k), testCanary...), noise(7)...)
		content := []byte(`{"blob":"` + base64.URLEncoding.EncodeToString(stream) + `"}`)
		for _, n := range b64Aligned([]byte(testCanary), base64.StdEncoding) {
			if bytes.Contains(content, []byte(n)) {
				t.Fatalf("alignment %d: the URL-safe file holds a standard needle; the case tests nothing", k)
			}
		}
		d := filepath.Join(dir, fmt.Sprintf("b64url-%d", k))
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "blob"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		out, code := canaryPy(t, "scan", "--canaries", canaries, "--label", "b64url", d)
		if code != 1 {
			t.Errorf("URL-safe base64 at alignment %d: exit %d, want 1 (found): %s", k, code, out)
		}
	}
}

// The canary's '?' is percent-encoded when content travels in a URL; the search looks for that form.
func TestTheCanarySearchSeesPercentEncoding(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	canaries := writeCanaries(t, dir)
	for name, content := range map[string]string{
		"query":   "GET /x?q=" + url.QueryEscape("see "+testCanary) + " HTTP/1.1",
		"path":    "/agents/" + url.PathEscape(testCanary) + "/card",
		"tilde7E": strings.ReplaceAll(url.QueryEscape(testCanary), "~", "%7E"),
	} {
		d := filepath.Join(dir, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "log"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, code := canaryPy(t, "scan", "--canaries", canaries, "--label", name, d); code != 1 {
			t.Errorf("%s (%s): exit %d, want 1 (found): %s", name, content, code, out)
		}
	}
}

// A value in an SQLite database is not always a contiguous run of bytes in the file (overflow pages;
// here, a UTF-16 database, where text is stored in another encoding): the search also reads each
// database table by table, from a copy that includes its WAL.
func TestTheCanarySearchReadsDatabasesTableByTable(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	canaries := writeCanaries(t, dir)
	db := filepath.Join(dir, "data", "hub.db")
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		t.Fatal(err)
	}
	mk := exec.Command("python3", "-c", `
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.execute("PRAGMA encoding='UTF-16le'")
con.execute("PRAGMA journal_mode=WAL")
con.execute("CREATE TABLE relay_message (id INTEGER PRIMARY KEY, body TEXT)")
con.execute("INSERT INTO relay_message(body) VALUES (?)", ("to: " + sys.argv[2],))
con.commit()
`, db, testCanary)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Skipf("python3 cannot build an sqlite database here: %v %s", err, out)
	}
	b, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	wal, _ := os.ReadFile(db + "-wal")
	if bytes.Contains(b, []byte(testCanary)) || bytes.Contains(wal, []byte(testCanary)) {
		t.Fatal("the fixture holds the canary as UTF-8 bytes; the test would pass for the wrong reason")
	}
	out, code := canaryPy(t, "scan", "--canaries", canaries, "--expect", "hub.db", filepath.Dir(db))
	if code != 1 || !strings.Contains(out, "relay_message") {
		t.Errorf("exit %d, want 1 with the table named: %s", code, out)
	}
}

// "Nothing found" must not be what an empty or wrong search reports.
func TestTheCanarySearchDoesNotPassOnNothing(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	canaries := writeCanaries(t, dir)
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, empty); code != 2 {
		t.Errorf("an empty directory: exit %d, want 2: %s", code, out)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, filepath.Join(dir, "missing")); code != 2 {
		t.Errorf("a missing path: exit %d, want 2: %s", code, out)
	}
	if err := os.WriteFile(filepath.Join(empty, "other.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, "--expect", "hub.db", empty); code != 2 {
		t.Errorf("--expect hub.db where there is none: exit %d, want 2: %s", code, out)
	}
	if out, code := canaryPy(t, "scan", "--canaries", filepath.Join(dir, "none.tsv"), empty); code != 2 {
		t.Errorf("no canary list: exit %d, want 2 (not 1, which means found): %s", code, out)
	}
	blank := filepath.Join(dir, "blank.tsv")
	if err := os.WriteFile(blank, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := canaryPy(t, "scan", "--canaries", blank, empty); code != 2 {
		t.Errorf("an empty canary list: exit %d, want 2: %s", code, out)
	}
}

// --want turns the search into the positive control joint.sh runs over a recipient's own data.
func TestTheCanaryPositiveControl(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	canaries := writeCanaries(t, dir)
	has := filepath.Join(dir, "has")
	hasnt := filepath.Join(dir, "hasnt")
	for _, d := range []string{has, hasnt} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(has, "interactions.db-wal"), append(noise(40), testCanary...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hasnt, "interactions.db"), noise(40), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, "--want", "goal", has); code != 0 {
		t.Errorf("--want goal where it is: exit %d, want 0: %s", code, out)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, "--want", "goal", hasnt); code != 1 {
		t.Errorf("--want goal where it is not: exit %d, want 1: %s", code, out)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// The tap forwards requests unchanged and answers with the upstream's answer, and records both bodies
// as they are: uncompressed even when the client asked for gzip, and /x402/settle once more on its own.
// Started through lib.sh's canary_tap from a bin directory, it is stopped by stop_under of that directory.
func TestTheTapForwardsAndRecordsWhatTheHubSawAndIsStoppedByPath(t *testing.T) {
	needLinuxShell(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			http.Error(w, "the tap forwarded the client's Accept-Encoding", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		switch r.URL.Path {
		case "/x402/settle":
			_, _ = w.Write([]byte(`{"success":true,"transaction":"t1","network":"hub:x"}`))
		default:
			fmt.Fprintf(w, `{"method":%q,"path":%q,"sig":%q,"got":%q}`, r.Method, r.URL.RequestURI(),
				r.Header.Get("X-ANet-Sig"), body)
		}
	}))
	defer upstream.Close()

	// HOME apart from the bin directory: stop_under refuses $HOME/bin, as it refuses any directory a
	// script cannot own outright.
	home, base := t.TempDir(), t.TempDir()
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("canary.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "canary.py"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "tap")
	addr := freePort(t)
	out := libsh(t, home, `CANARY_PY="$1/canary.py"; canary_tap "$2" "$3" "$4" "$5" && echo " started"`,
		bin, dir, addr, upstream.URL, filepath.Join(base, "tap.log"))
	pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(out), " started"))
	if err != nil || !strings.HasSuffix(strings.TrimSpace(out), "started") {
		t.Fatalf("canary_tap: %q", out)
	}
	// Whatever the test does, the tap does not outlive it.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	send := func(method, path, body string) (int, string, http.Header) {
		req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-ANet-Sig", "sig-value")
		resp, err := http.DefaultClient.Do(req) // Go asks for gzip by itself
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	code, got, hdr := send("POST", "/relay/send?x=1", `{"envelope":"`+testCanary+`"}`)
	if code != 200 || hdr.Get("X-Upstream") != "yes" ||
		!strings.Contains(got, `"path":"/relay/send?x=1"`) || !strings.Contains(got, `"sig":"sig-value"`) ||
		!strings.Contains(got, testCanary) {
		t.Fatalf("forwarded request: %d %s", code, got)
	}
	settle := `{"x402Version":2,"paymentPayload":{"x402Version":2,"accepted":{"scheme":"anet-credit","network":"hub:x","amount":"3","asset":"credit","payTo":"p"},"payload":{"authorization":"YQ=="}},"paymentRequirements":{"scheme":"anet-credit","network":"hub:x","amount":"3","asset":"credit","payTo":"p"}}`
	if code, got, _ := send("POST", "/x402/settle", settle); code != 200 || !strings.Contains(got, `"success":true`) {
		t.Fatalf("settle through the tap: %d %s", code, got)
	}
	if code, _, _ := send("GET", "/agents", ""); code != 200 {
		t.Fatalf("GET through the tap: %d", code)
	}

	log, err := os.ReadFile(filepath.Join(dir, "traffic.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"POST /relay/send?x=1 -> 200", `{"envelope":"` + testCanary + `"}`,
		`"got":`, "x-anet-sig: sig-value", "GET /agents -> 200"} {
		if !bytes.Contains(log, []byte(want)) && !bytes.Contains(bytes.ToLower(log), []byte(want)) {
			t.Errorf("traffic.log lacks %q", want)
		}
	}
	idx, _ := os.ReadFile(filepath.Join(dir, "index.jsonl"))
	if n := bytes.Count(idx, []byte("\n")); n != 3 {
		t.Errorf("index.jsonl has %d lines, want 3", n)
	}
	reqs, _ := filepath.Glob(filepath.Join(dir, "settle", "*.req"))
	if len(reqs) != 1 {
		t.Fatalf("settle requests kept: %d, want 1", len(reqs))
	}
	if b, _ := os.ReadFile(reqs[0]); string(b) != settle {
		t.Errorf("the settle body was not kept byte for byte: %s", b)
	}
	if out, code := canaryPy(t, "settle", "--dir", dir); code != 0 {
		t.Errorf("settle check of a clean body: exit %d: %s", code, out)
	}

	// The tap runs from bin, so stopping by that path stops it.
	libsh(t, home, `stop_under "$1" 5`, bin)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the tap still listens after stop_under of its bin directory")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The structured settlement check: resource, description and extra empty wherever they appear, and
// nothing outside the x402 v2 objects; at least one body answered success.
func TestTheSettlementCheck(t *testing.T) {
	needPython(t)
	good := map[string]any{
		"x402Version": 2,
		"paymentPayload": map[string]any{"x402Version": 2,
			"accepted": map[string]any{"scheme": "anet-credit", "network": "hub:x", "amount": "3", "asset": "credit", "payTo": "p"},
			"payload":  map[string]any{"authorization": "YQ=="}},
		"paymentRequirements": map[string]any{"scheme": "anet-credit", "network": "hub:x", "amount": "3",
			"asset": "credit", "payTo": "p", "maxTimeoutSeconds": 600, "extra": map[string]any{}},
	}
	clone := func(edit func(m map[string]any)) map[string]any {
		b, _ := json.Marshal(good)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		edit(m)
		return m
	}
	sub := func(m map[string]any, k string) map[string]any { return m[k].(map[string]any) }
	cases := []struct {
		name    string
		body    any
		success bool
		want    int
	}{
		{"clean", good, true, 0},
		{"clean, but refused", good, false, 1},
		{"extra names the work", clone(func(m map[string]any) {
			sub(m, "paymentRequirements")["extra"] = map[string]any{"capability": "text.digest"}
		}), true, 1},
		{"a resource rides along", clone(func(m map[string]any) {
			m["resource"] = map[string]any{"url": "anet:capability/text.digest"}
		}), true, 1},
		{"description in accepted", clone(func(m map[string]any) {
			sub(sub(m, "paymentPayload"), "accepted")["description"] = "text.digest"
		}), true, 1},
		{"payload carries more than the authorization", clone(func(m map[string]any) {
			sub(sub(m, "paymentPayload"), "payload")["note"] = "x"
		}), true, 1},
		{"extensions", clone(func(m map[string]any) {
			sub(m, "paymentPayload")["extensions"] = map[string]any{"anet.task": "x"}
		}), true, 1},
		{"not JSON", "{", true, 1},
	}
	for _, c := range cases {
		dir := t.TempDir()
		sd := filepath.Join(dir, "settle")
		if err := os.MkdirAll(sd, 0o700); err != nil {
			t.Fatal(err)
		}
		var body []byte
		if s, ok := c.body.(string); ok {
			body = []byte(s)
		} else {
			body, _ = json.Marshal(c.body)
		}
		resp := fmt.Sprintf(`{"success":%v,"transaction":"t","network":"hub:x"}`, c.success)
		_ = os.WriteFile(filepath.Join(sd, "000001.req"), body, 0o600)
		_ = os.WriteFile(filepath.Join(sd, "000001.resp"), []byte(resp), 0o600)
		if out, code := canaryPy(t, "settle", "--dir", dir); code != c.want {
			t.Errorf("%s: exit %d, want %d: %s", c.name, code, c.want, out)
		}
	}
	// No settlement seen at all is not a pass.
	if out, code := canaryPy(t, "settle", "--dir", t.TempDir()); code != 1 {
		t.Errorf("no settle bodies: exit %d, want 1: %s", code, out)
	}
}

// A file the search could not read is not a file without the canary: a database whose tables cannot be
// read, or a file this user cannot open, makes the scan exit 2 ("not searched in full"), not 0. A hit
// elsewhere still reports as a hit.
func TestTheCanarySearchDoesNotPassOnWhatItCouldNotRead(t *testing.T) {
	needPython(t)
	dir := t.TempDir()
	canaries := writeCanaries(t, dir)
	broken := filepath.Join(dir, "broken")
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	// An SQLite header over garbage: the bytes are searched, the tables cannot be.
	if err := os.WriteFile(filepath.Join(broken, "hub.db"), append([]byte("SQLite format 3\x00"), noise(4000)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, "--expect", "hub.db", broken); code != 2 ||
		!strings.Contains(out, "not searched in full") {
		t.Errorf("an unreadable database: exit %d, want 2: %s", code, out)
	}
	if err := os.WriteFile(filepath.Join(broken, "hit.log"), []byte(testCanary), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, broken); code != 1 {
		t.Errorf("a hit beside an unreadable database: exit %d, want 1: %s", code, out)
	}

	if os.Geteuid() != 0 { // root reads a mode-000 file
		closed := filepath.Join(dir, "closed")
		if err := os.MkdirAll(closed, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(closed, "ok.log"), noise(10), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(closed, "hub.db-wal"), noise(10), 0o000); err != nil {
			t.Fatal(err)
		}
		if out, code := canaryPy(t, "scan", "--canaries", canaries, closed); code != 2 {
			t.Errorf("a file that cannot be opened: exit %d, want 2: %s", code, out)
		}
	}
}

// Where the sqlite3 CLI is not installed, canary.py stands in for it so that ANetHub's
// deploy/hub-db-roll.sh — the backup joint.sh section C searches — runs. Run here against a fixture
// hub.db whose relay row holds the canary: the backup is written, holds no relay row, and the search
// finds no canary in it, while the live database it came from still has one.
func TestTheSQLiteStandInRunsTheHubBackup(t *testing.T) {
	needLinuxShell(t)
	roll, err := filepath.Abs(filepath.Join("..", "..", "ANetHub", "deploy", "hub-db-roll.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(roll); err != nil {
		t.Skipf("no ANetHub checkout beside this repository: %v", err)
	}
	dir := t.TempDir()
	canaries := writeCanaries(t, dir)
	data := filepath.Join(dir, "hub")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	mk := exec.Command("python3", "-c", `
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
con.execute("PRAGMA journal_mode=WAL")
con.execute("CREATE TABLE relay_message (id INTEGER PRIMARY KEY AUTOINCREMENT, to_aid TEXT NOT NULL, size INTEGER, created_at INTEGER, payload BLOB)")
con.execute("CREATE TABLE agents (aid TEXT PRIMARY KEY, name TEXT)")
con.execute("INSERT INTO relay_message(to_aid,size,created_at,payload) VALUES ('bafyagent', 1, 1, ?)", (b"x" * 700 + sys.argv[2].encode() + b"y" * 900,))
con.execute("INSERT INTO agents VALUES ('bafyagent', 'a')")
con.commit()
`, filepath.Join(data, "hub.db"), testCanary)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Skipf("python3 cannot build an sqlite database here: %v %s", err, out)
	}
	py, _ := filepath.Abs("canary.py")
	standin := filepath.Join(dir, "standin")
	if err := os.MkdirAll(standin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(standin, "sqlite3"),
		[]byte("#!/bin/sh\nexec python3 '"+py+"' sqlite3 \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", roll)
	cmd.Env = append(os.Environ(), "PATH="+standin+":"+os.Getenv("PATH"), "HUB_DATA_DIR="+data, "FORCE_WEEKLY=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hub-db-roll.sh with the stand-in: %v\n%s", err, out)
	}
	backups, _ := filepath.Glob(filepath.Join(data, "hub-backup-*.db"))
	if len(backups) != 1 {
		t.Fatalf("backups: %v, want one", backups)
	}
	if out, code := canaryPy(t, "sqlite3", backups[0], "SELECT COUNT(*) FROM relay_message;",
		"SELECT COUNT(*) FROM agents;"); code != 0 || out != "0\n1\n" {
		t.Errorf("the backup's relay and agent rows: exit %d, %q, want 0 and 1", code, out)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, "--expect", filepath.Base(backups[0]), backups[0]); code != 0 {
		t.Errorf("the backup holds the relay row's canary: exit %d: %s", code, out)
	}
	if out, code := canaryPy(t, "scan", "--canaries", canaries, filepath.Join(data, "hub.db")); code != 1 {
		t.Errorf("the live database no longer holds the canary, so the backup check proves nothing: exit %d: %s", code, out)
	}
	// What the stand-in does not do, it refuses, rather than doing something else.
	if out, code := canaryPy(t, "sqlite3", backups[0], ".dump"); code != 2 {
		t.Errorf(".dump: exit %d, want 2: %s", code, out)
	}
	if out, code := canaryPy(t, "sqlite3", backups[0], "SELECT nothing FROM nowhere;"); code != 1 || !strings.Contains(out, "Error:") {
		t.Errorf("a failing statement: exit %d, want 1 with the error: %s", code, out)
	}
}
