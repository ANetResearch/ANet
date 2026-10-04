package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// apexDir holds what agentnetwork.org.cn serves from its release root:
// SKILL.md (also as /llms-full.txt), llms.txt and the source of site.json,
// with publish.sh and the nginx locations (deploy/release/apex/README.md).
var apexDir = filepath.Join("..", "..", "deploy", "release", "apex")

func readApex(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(apexDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var apexLiteralWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// apexHelp is what `anet help --all` promises: the commands, and for
// commands with literal subcommands (agents wire, peers allow|trust, …) the
// set of them.
type apexHelp struct {
	cmds map[string]bool
	subs map[string]map[string]bool
}

func apexReadHelp(t *testing.T) apexHelp {
	t.Helper()
	h := apexHelp{cmds: map[string]bool{}, subs: map[string]map[string]bool{}}
	for _, line := range strings.Split(usageAllText(), "\n") {
		syntax := regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), 2)[0]
		f := strings.Fields(syntax)
		if len(f) < 2 || f[0] != "anet" || !apexLiteralWord.MatchString(f[1]) {
			continue
		}
		h.cmds[f[1]] = true
		if len(f) > 2 {
			for _, s := range strings.Split(strings.Trim(f[2], "[]"), "|") {
				if apexLiteralWord.MatchString(s) {
					if h.subs[f[1]] == nil {
						h.subs[f[1]] = map[string]bool{}
					}
					h.subs[f[1]][s] = true
				}
			}
		}
	}
	if len(h.cmds) < 30 {
		t.Fatalf("only %d commands read from the help — the parse is probably broken", len(h.cmds))
	}
	return h
}

// problems lists what is wrong with one invocation, f being the words after
// "anet" and f[0] a command word: a command the help does not list, a
// subcommand it does not list, a flag the flag checker refuses.
func (h apexHelp) problems(f []string) []string {
	cmd := f[0]
	if !h.cmds[cmd] {
		return []string{fmt.Sprintf("`anet %s` is not a command", cmd)}
	}
	var p []string
	if len(f) > 1 && h.subs[cmd] != nil && apexLiteralWord.MatchString(f[1]) && !h.subs[cmd][f[1]] {
		p = append(p, fmt.Sprintf("`anet %s %s` is not a subcommand the help lists", cmd, f[1]))
	}
	for _, a := range f[1:] {
		if strings.HasPrefix(a, "--") {
			if err := checkFlags(cmd, []string{a, "v"}); err != nil {
				p = append(p, err.Error())
			}
		}
	}
	return p
}

var (
	apexShellSep = regexp.MustCompile(`&&|\|\||[|;]`)
	// Prose also ends a command at a comma, a parenthesis or the end of a
	// sentence: "via anet identity protect in a terminal." names one
	// command, and its last word keeps no full stop.
	apexProseSep = regexp.MustCompile(`&&|\|\||[|;,()]|\.(\s|$)`)
)

// apexInvocations returns, for each command in text that starts with the word
// "anet", the words after it.
func apexInvocations(text string, prose bool) [][]string {
	sep := apexShellSep
	if prose {
		sep = apexProseSep
	}
	var out [][]string
	for _, seg := range sep.Split(text, -1) {
		f := strings.Fields(seg)
		for len(f) > 0 && f[0] != "anet" {
			f = f[1:]
		}
		if len(f) < 2 {
			continue
		}
		f = f[1:]
		if f[0] == "--id" && len(f) > 2 {
			f = f[2:]
		}
		if prose {
			for i := range f {
				f[i] = strings.TrimRight(f[i], ".:!?")
			}
		}
		out = append(out, f)
	}
	return out
}

// The text agentnetwork.org.cn serves as /SKILL.md, /llms-full.txt and
// /llms.txt (deploy/release/apex) may only teach commands this binary has.
//
// The website's own copy went on teaching `anet whoami`, `anet daemon &` and
// `anet board` for months after 0.2.0 removed them, and an agent that reads
// it runs what it says. Every `anet …` inside code in those files must be a
// command `anet help --all` lists, a subcommand listed for it, and flags the
// flag checker accepts. The 0.1.x commands the files name in order to say
// they are gone are listed below, and must stay gone.
func TestApexDocsTeachOnlyRealCommands(t *testing.T) {
	gone := map[string]bool{"whoami": true, "board": true, "brain": true, "task publish": true}
	h := apexReadHelp(t)
	for g := range gone {
		f := strings.Fields(g)
		if (len(f) == 1 && h.cmds[f[0]]) || (len(f) == 2 && h.subs[f[0]][f[1]]) {
			t.Errorf("the apex docs say `anet %s` is gone, but the help lists it", g)
		}
	}

	fence := regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")
	inline := regexp.MustCompile("`([^`\n]+)`")
	seen := 0
	for _, name := range []string{"SKILL.md", "llms.txt"} {
		text := readApex(t, name)
		// A removed command may be named only inline, in a paragraph that
		// says which release it belonged to.
		type snippet struct {
			text      string
			namesGone bool
		}
		var code []snippet
		for _, m := range fence.FindAllStringSubmatch(text, -1) {
			for _, l := range strings.Split(m[1], "\n") {
				code = append(code, snippet{l, false})
			}
		}
		for _, para := range strings.Split(fence.ReplaceAllString(text, ""), "\n\n") {
			for _, line := range strings.Split(para, "\n") {
				if strings.HasPrefix(line, "    ") { // an indented code block
					code = append(code, snippet{line, false})
					continue
				}
				for _, m := range inline.FindAllStringSubmatch(line, -1) {
					code = append(code, snippet{m[1], strings.Contains(para, "0.1.x")})
				}
			}
		}

		for _, sn := range code {
			line := sn.text
			if i := strings.Index(line, "#"); i >= 0 {
				line = line[:i]
			}
			for _, f := range apexInvocations(line, false) {
				cmd := f[0]
				if !apexLiteralWord.MatchString(cmd) {
					continue
				}
				seen++
				if gone[cmd] || (len(f) > 1 && gone[cmd+" "+f[1]]) {
					if !sn.namesGone {
						t.Errorf("%s: %q teaches a command 0.2 removed", name, "anet "+strings.Join(f, " "))
					}
					continue
				}
				for _, p := range h.problems(f) {
					t.Errorf("%s: %s (%q)", name, p, "anet "+strings.Join(f, " "))
				}
			}
		}
	}
	if seen < 30 {
		t.Fatalf("only %d anet invocations found in the apex docs — the parse is probably broken", seen)
	}
}

// --- site.json ----------------------------------------------------------------
//
// The website used to carry its own copy of the install command and of what
// to tell an agent, inside content-hashed bundles that browsers keep for a
// year: a fix to that text needed a website rebuild, and returning visitors
// kept the old one anyway. The website now reads /site.json at run time
// (CUX §2.12), served with Cache-Control: no-cache from the release root.
// deploy/release/apex/site.json is its source: the served file without
// "version", which publish.sh adds from the release that /dl/ serves.

// siteJSON is the served /site.json.
type siteJSON struct {
	V         int    `json:"v"`
	Version   string `json:"version"`
	Install   string `json:"install"`
	SkillURL  string `json:"skill_url"`
	AgentHint string `json:"agent_hint"`
}

var (
	siteSourceKeys = []string{"agent_hint", "install", "skill_url", "v"}
	siteServedKeys = []string{"agent_hint", "install", "skill_url", "v", "version"}
)

// apexDecodeSite decodes b strictly and requires exactly the keys in want.
func apexDecodeSite(b []byte, want []string) (siteJSON, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return siteJSON{}, err
	}
	var got []string
	for k := range raw {
		got = append(got, k)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return siteJSON{}, fmt.Errorf("keys are %v, want exactly %v", got, want)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s siteJSON
	if err := dec.Decode(&s); err != nil {
		return siteJSON{}, err
	}
	if dec.More() {
		return siteJSON{}, fmt.Errorf("data after the object")
	}
	return s, nil
}

func apexSiteSource(t *testing.T) siteJSON {
	t.Helper()
	s, err := apexDecodeSite([]byte(readApex(t, "site.json")), siteSourceKeys)
	if err != nil {
		t.Fatalf("deploy/release/apex/site.json: %v", err)
	}
	return s
}

const apexSkillURL = "https://agentnetwork.org.cn/SKILL.md"

// agentHintJoin is ux §3.4 (g) agent_hint.join in English: SKILL.md is
// English, and site.json carries the same sentence as SKILL.md. The
// sentence lives in the reason table and the three published files only.
var agentHintJoin = a2ashape.Sentence("agent_hint.join", a2ashape.English, nil)

// The source is what publish.sh can extend: exactly the served keys but
// "version", and a first line publish.sh puts "version" after.
func TestSiteJSONSource(t *testing.T) {
	text := readApex(t, "site.json")
	s := apexSiteSource(t)
	if first, _, _ := strings.Cut(text, "\n"); first != `{"v": 1,` {
		t.Errorf("site.json begins with %q; publish.sh adds \"version\" after a first line of exactly {\"v\": 1,", first)
	}
	if s.V != 1 {
		t.Errorf("v = %d, want 1", s.V)
	}
	if s.SkillURL != apexSkillURL {
		t.Errorf("skill_url = %q, want %q", s.SkillURL, apexSkillURL)
	}
	for name, v := range map[string]string{"install": s.Install, "agent_hint": s.AgentHint} {
		if v == "" || !utf8.ValidString(v) || strings.TrimSpace(v) != v {
			t.Errorf("%s is empty, not UTF-8 or padded: %q", name, v)
		}
		for _, r := range v {
			if r < 0x20 && !(r == '\n' && name == "install") {
				t.Errorf("%s holds control character %U", name, r)
			}
		}
	}
	if strings.Join(strings.Fields(s.AgentHint), " ") != s.AgentHint {
		t.Errorf("agent_hint is one sentence with single spaces: %q", s.AgentHint)
	}
}

// Every `anet …` the website shows is a command this binary has.
func TestSiteJSONCommandsInHelp(t *testing.T) {
	h := apexReadHelp(t)
	s := apexSiteSource(t)
	// v030-todo(KEL-10): `anet identity protect` (KEL L-10) is in the hint
	// before it is in the help; KEL-10 deletes this entry.
	pending := map[string]string{"identity protect": "KEL-10"}
	var waiting []string
	seen := 0
	for _, field := range []struct {
		name, text string
		prose      bool
	}{{"install", s.Install, false}, {"agent_hint", s.AgentHint, true}} {
		for _, f := range apexInvocations(field.text, field.prose) {
			if !apexLiteralWord.MatchString(f[0]) {
				t.Errorf("site.json %s: %q is not an anet command", field.name, "anet "+strings.Join(f, " "))
				continue
			}
			seen++
			p := h.problems(f)
			if len(f) > 1 && len(p) > 0 && pending[f[0]+" "+f[1]] != "" {
				waiting = append(waiting, fmt.Sprintf("`anet %s %s` (%s)", f[0], f[1], pending[f[0]+" "+f[1]]))
				continue
			}
			for _, e := range p {
				t.Errorf("site.json %s: %s (%q)", field.name, e, "anet "+strings.Join(f, " "))
			}
		}
	}
	if seen == 0 {
		t.Fatal("no anet command found in site.json — the agent hint names at least the join command; the parse is probably broken")
	}
	if len(waiting) > 0 {
		t.Skipf("v030-todo(KEL-10): the help does not list %s yet", strings.Join(waiting, ", "))
	}
}

var apexFencedBlock = regexp.MustCompile("(?s)```[a-z]*\n(.*?)```")

// The install command the website shows is SKILL.md's, word for word.
func TestSiteJSONInstallMatchesSkill(t *testing.T) {
	s := apexSiteSource(t)
	var installers []string
	for _, m := range apexFencedBlock.FindAllStringSubmatch(readApex(t, "SKILL.md"), -1) {
		if b := strings.TrimRight(m[1], "\n"); strings.Contains(b, "/install.sh | sh") {
			installers = append(installers, b)
		}
	}
	if len(installers) == 0 {
		t.Fatal("SKILL.md has no installer command (a code block running …/install.sh | sh)")
	}
	found := false
	for _, b := range installers {
		found = found || b == s.Install
	}
	if !found {
		t.Errorf("site.json install is not SKILL.md's installer command:\n site.json: %q\n SKILL.md:  %q", s.Install, installers[0])
	}
	// The pinned form: https only, TLS 1.2 or later, from this host. And
	// nothing from npm: the website stopped offering it (CUX-W2).
	if !strings.HasPrefix(s.Install, "curl --proto '=https' --tlsv1.2 -fsSL https://agentnetwork.org.cn/install.sh | sh") {
		t.Errorf("install is not the pinned https installer: %q", s.Install)
	}
	if strings.Contains(s.Install, "npm") {
		t.Errorf("install mentions npm: %q", s.Install)
	}
}

// apexCarries reports whether doc holds sentence word for word. Markdown
// joins a paragraph's lines with spaces, so a sentence wrapped over several
// lines is still the same sentence.
func apexCarries(doc, sentence string) bool {
	return strings.Contains(strings.Join(strings.Fields(doc), " "), sentence)
}

func TestSkillCarriesSentence(t *testing.T) {
	const s = "Join with anet hub-register even without a terminal; do not register again."
	for _, c := range []struct {
		doc  string
		want bool
	}{
		{"# x\n\n" + s + "\n", true},
		{"Intro.\nJoin with anet hub-register even without a\nterminal; do not register again. More.", true},
		{"Join with `anet hub-register` even without a terminal; do not register again.", false},
		{"Join with anet hub-register even without a terminal; do not register twice.", false},
		{"", false},
	} {
		if got := apexCarries(c.doc, s); got != c.want {
			t.Errorf("apexCarries(%q) = %v, want %v", c.doc, got, c.want)
		}
	}
}

// What the website tells an agent is ux §3.4 (g) agent_hint.join, the same
// sentence SKILL.md and llms.txt carry (a first join that needs no terminal).
func TestSiteJSONAgentHintMatchesSkill(t *testing.T) {
	s := apexSiteSource(t)
	if s.AgentHint != agentHintJoin {
		t.Errorf("site.json agent_hint is not agent_hint.join:\n got:  %q\n want: %q", s.AgentHint, agentHintJoin)
	}
	// SKILL.md and llms.txt tell an agent the same sentence (LEC §4.10).
	for _, doc := range []string{"SKILL.md", "llms.txt"} {
		if !apexCarries(readApex(t, doc), agentHintJoin) {
			t.Errorf("%s does not carry agent_hint.join word for word: %q", doc, agentHintJoin)
		}
	}
}

// --- publish.sh -----------------------------------------------------------------

// apexRunPublish runs publish.sh with a TMPDIR of its own, which it must leave
// empty whether it publishes or refuses.
func apexRunPublish(t *testing.T, args ...string) (string, error) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join(apexDir, "publish.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.CombinedOutput()
	if left := apexSnapshot(t, tmp); len(left) > 0 {
		t.Errorf("publish.sh left %v in TMPDIR", left)
	}
	return string(out), err
}

func apexWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// apexFakeDist is a build-release.sh dist/ with one archive; publish.sh checks
// its checksum (signatures only with --allowed-signers, not given here).
func apexFakeDist(t *testing.T, version string) string {
	t.Helper()
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("publish.sh checks archives with sha256sum, which this machine lacks")
	}
	d := t.TempDir()
	gz := "anet_" + version + "_linux_amd64.tar.gz"
	body := "archive " + version
	sum := sha256.Sum256([]byte(body))
	for name, content := range map[string]string{
		"release.json":     `{"version": "` + version + `"}`,
		"release.json.sig": "sig",
		"VERSION":          version + "\n",
		"checksums.txt":    hex.EncodeToString(sum[:]) + "  " + gz + "\n",
		"install.sh":       "#!/bin/sh\n",
		"install.sh.sig":   "sig",
		gz:                 body,
	} {
		apexWriteFile(t, filepath.Join(d, name), content)
	}
	return d
}

// apexSnapshot is every file under dir, by path relative to it, with its content.
func apexSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		m[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func apexSameSnapshot(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("%s changed the release root:\n before: %v\n after:  %v", what, before, after)
	}
}

// apexCopyWeb copies the web files to a directory a case may then break.
func apexCopyWeb(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, f := range []string{"SKILL.md", "llms.txt", "site.json"} {
		apexWriteFile(t, filepath.Join(d, f), readApex(t, f))
	}
	return d
}

// apexServedSite is the site.json publish.sh must serve for the repository's
// source and release version v: the source with "version" after its first
// line's "v", byte for byte — the layout of CUX §2.12.
func apexServedSite(t *testing.T, v string) string {
	t.Helper()
	src := readApex(t, "site.json")
	return strings.Replace(src, `{"v": 1,`, `{"v": 1, "version": "`+v+`",`, 1)
}

func TestPublishWritesSiteJSON(t *testing.T) {
	t.Run("web takes the version of the release dl/ serves", func(t *testing.T) {
		root := t.TempDir()
		apexWriteFile(t, filepath.Join(root, "dl", "VERSION"), "9.8.7\n")
		if out, err := apexRunPublish(t, "--web", apexDir, "--root", root); err != nil {
			t.Fatalf("publish.sh --web: %v\n%s", err, out)
		}
		for _, f := range []string{"SKILL.md", "llms.txt"} {
			if got := apexSnapshot(t, root)[f]; got != readApex(t, f) {
				t.Errorf("%s in the release root differs from the repository's", f)
			}
		}
		b, err := os.ReadFile(filepath.Join(root, "site.json"))
		if err != nil {
			t.Fatalf("publish.sh --web wrote no site.json: %v", err)
		}
		if want := apexServedSite(t, "9.8.7"); string(b) != want {
			t.Errorf("served site.json:\n%s\nwant:\n%s", b, want)
		}
		s, err := apexDecodeSite(b, siteServedKeys)
		if err != nil {
			t.Fatalf("served site.json: %v", err)
		}
		src := apexSiteSource(t)
		if s.Version != "9.8.7" || s.V != src.V || s.Install != src.Install || s.SkillURL != src.SkillURL || s.AgentHint != src.AgentHint {
			t.Errorf("served site.json %+v is not the source %+v with version 9.8.7", s, src)
		}
		if fi, err := os.Stat(filepath.Join(root, "site.json")); err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("site.json mode: %v %v, want 0644", fi.Mode().Perm(), err)
		}
		for f := range apexSnapshot(t, root) {
			if strings.HasPrefix(filepath.Base(f), ".") {
				t.Errorf("publish.sh left %s behind", f)
			}
		}
	})

	t.Run("dist and web in one run take the new release's version, and every served path exists", func(t *testing.T) {
		root := t.TempDir()
		apexWriteFile(t, filepath.Join(root, "dl", "VERSION"), "1.0.0\n")
		dist := apexFakeDist(t, "9.9.9")
		if out, err := apexRunPublish(t, "--dist", dist, "--web", apexDir, "--root", root); err != nil {
			t.Fatalf("publish.sh --dist --web: %v\n%s", err, out)
		}
		b, err := os.ReadFile(filepath.Join(root, "site.json"))
		if err != nil {
			t.Fatal(err)
		}
		if want := apexServedSite(t, "9.9.9"); string(b) != want {
			t.Errorf("served site.json names another release:\n%s\nwant:\n%s", b, want)
		}
		// nginx serves nothing from the release root that publish.sh does
		// not write.
		aliases := regexp.MustCompile(`alias /var/www/anet-release/([^;]*);`).FindAllStringSubmatch(readApex(t, "nginx-locations.conf"), -1)
		if len(aliases) < 7 {
			t.Fatalf("only %d aliases in nginx-locations.conf", len(aliases))
		}
		for _, a := range aliases {
			if _, err := os.Stat(filepath.Join(root, a[1])); err != nil {
				t.Errorf("nginx serves /var/www/anet-release/%s, which publish.sh did not write: %v", a[1], err)
			}
		}
	})

	t.Run("no release under dl/, no site.json", func(t *testing.T) {
		root := t.TempDir()
		out, err := apexRunPublish(t, "--web", apexDir, "--root", root)
		if err == nil {
			t.Fatalf("publish.sh --web published with no release under dl/:\n%s", out)
		}
		if !strings.Contains(out, "--dist") {
			t.Errorf("the refusal does not say what to do (--dist): %s", out)
		}
		apexSameSnapshot(t, "a refused --web", map[string]string{}, apexSnapshot(t, root))
	})

	t.Run("a VERSION that is not a version is refused", func(t *testing.T) {
		for _, v := range []string{`1.0", "x": "y`, "v1.0.0", "", "1.0.0\n2.0.0", "1.0.0\r"} {
			root := t.TempDir()
			apexWriteFile(t, filepath.Join(root, "dl", "VERSION"), v+"\n")
			apexWriteFile(t, filepath.Join(root, "SKILL.md"), "old\n")
			before := apexSnapshot(t, root)
			if out, err := apexRunPublish(t, "--web", apexDir, "--root", root); err == nil {
				t.Errorf("VERSION %q accepted:\n%s", v, out)
			}
			apexSameSnapshot(t, fmt.Sprintf("VERSION %q", v), before, apexSnapshot(t, root))
		}
	})

	t.Run("a site.json publish.sh cannot extend is refused", func(t *testing.T) {
		src := readApex(t, "site.json")
		for name, bad := range map[string]string{
			"pretty-printed":   strings.Replace(src, `{"v": 1,`, "{\n \"v\": 1,", 1),
			"carries version":  strings.Replace(src, `{"v": 1,`, `{"v": 1, "version": "0.0.1",`, 1),
			"version later on": strings.Replace(src, `"skill_url"`, `"version": "0.0.1", "skill_url"`, 1),
		} {
			web := apexCopyWeb(t)
			apexWriteFile(t, filepath.Join(web, "site.json"), bad)
			root := t.TempDir()
			apexWriteFile(t, filepath.Join(root, "dl", "VERSION"), "9.8.7\n")
			apexWriteFile(t, filepath.Join(root, "SKILL.md"), "old\n")
			before := apexSnapshot(t, root)
			if out, err := apexRunPublish(t, "--web", web, "--root", root); err == nil {
				t.Errorf("%s: accepted:\n%s", name, out)
			}
			apexSameSnapshot(t, name, before, apexSnapshot(t, root))
		}
	})

	t.Run("a bad web dir publishes nothing, not even the dist", func(t *testing.T) {
		root := t.TempDir()
		apexWriteFile(t, filepath.Join(root, "dl", "VERSION"), "1.0.0\n")
		dist := apexFakeDist(t, "9.9.9")
		web := apexCopyWeb(t)
		if err := os.Remove(filepath.Join(web, "site.json")); err != nil {
			t.Fatal(err)
		}
		before := apexSnapshot(t, root)
		out, err := apexRunPublish(t, "--dist", dist, "--web", web, "--root", root)
		if err == nil || !strings.Contains(out, "site.json is missing") {
			t.Errorf("a web dir without site.json: err %v\n%s", err, out)
		}
		apexSameSnapshot(t, "a refused --dist --web", before, apexSnapshot(t, root))
	})

	// The old dl/ is deleted when a new one goes in, so a dist alone would
	// leave site.json naming a release nobody can download any more. A root
	// with no site.json (the hub site's) still takes a dist alone.
	t.Run("a dist alone over a published site.json is refused", func(t *testing.T) {
		root := t.TempDir()
		apexWriteFile(t, filepath.Join(root, "dl", "VERSION"), "1.0.0\n")
		if out, err := apexRunPublish(t, "--web", apexDir, "--root", root); err != nil {
			t.Fatalf("publish.sh --web: %v\n%s", err, out)
		}
		dist := apexFakeDist(t, "9.9.9")
		before := apexSnapshot(t, root)
		out, err := apexRunPublish(t, "--dist", dist, "--root", root)
		if err == nil {
			t.Fatalf("publish.sh --dist replaced dl/ under a site.json that names 1.0.0:\n%s", out)
		}
		if !strings.Contains(out, "--web") {
			t.Errorf("the refusal does not say what to do (--web): %s", out)
		}
		apexSameSnapshot(t, "a refused --dist", before, apexSnapshot(t, root))
		// What the refusal says to do works.
		if out, err := apexRunPublish(t, "--dist", dist, "--web", apexDir, "--root", root); err != nil {
			t.Fatalf("publish.sh --dist --web over a published site.json: %v\n%s", err, out)
		}
		if got, want := apexSnapshot(t, root)["site.json"], apexServedSite(t, "9.9.9"); got != want {
			t.Errorf("served site.json after --dist --web:\n%s\nwant:\n%s", got, want)
		}

		hub := t.TempDir()
		if out, err := apexRunPublish(t, "--dist", dist, "--root", hub); err != nil {
			t.Fatalf("publish.sh --dist into a root without site.json: %v\n%s", err, out)
		}
		if got := apexSnapshot(t, hub)["dl/VERSION"]; got != "9.9.9\n" {
			t.Errorf("dl/VERSION = %q after --dist, want 9.9.9", got)
		}
		if _, ok := apexSnapshot(t, hub)["site.json"]; ok {
			t.Error("publish.sh --dist wrote a site.json")
		}
	})
}

// --- nginx ----------------------------------------------------------------------

// apexNginxLocation returns the body of `location <match> <path> { … }` in conf.
func apexNginxLocation(conf, match, path string) (string, bool) {
	head := regexp.MustCompile(`(?m)^\s*location\s+` + regexp.QuoteMeta(match) + `\s+` + regexp.QuoteMeta(path) + `\s*\{`)
	loc := head.FindStringIndex(conf)
	if loc == nil {
		return "", false
	}
	depth := 1
	for i := loc[1]; i < len(conf); i++ {
		switch conf[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return conf[loc[1]:i], true
			}
		}
	}
	return "", false
}

// /site.json is revalidated on every load (Cache-Control: no-cache), so a
// change reaches every visitor at their next page view, and it keeps the
// server block's security headers.
func TestNginxServesSiteJSONNoCache(t *testing.T) {
	conf := readApex(t, "nginx-locations.conf")
	body, ok := apexNginxLocation(conf, "=", "/site.json")
	if !ok {
		t.Fatal("nginx-locations.conf has no `location = /site.json`")
	}
	directives := map[string]bool{}
	for _, d := range strings.Split(body, ";") {
		directives[strings.Join(strings.Fields(d), " ")] = true
	}
	for _, want := range []string{
		"alias /var/www/anet-release/site.json",
		`types { } default_type "application/json"`,
		// nginx: "expires epoch" sends Cache-Control: no-cache (and an
		// Expires in 1970).
		"expires epoch",
	} {
		if !directives[want] {
			t.Errorf("location = /site.json lacks `%s;`:\n%s", want, body)
		}
	}
	// add_header would drop the server block's security headers here, and
	// any other caching directive would fight expires.
	for _, banned := range []string{"add_header", "Cache-Control", "proxy_", "try_files"} {
		if strings.Contains(body, banned) {
			t.Errorf("location = /site.json must not use %s:\n%s", banned, body)
		}
	}
	// /install.sh and /SKILL.md are revalidated on every use the same way,
	// and keep the server block's security headers: expires epoch, no
	// add_header (UX-16 left them with add_header, which dropped them).
	for _, loc := range []string{"/install.sh", "/SKILL.md"} {
		body, ok := apexNginxLocation(conf, "=", loc)
		if !ok {
			t.Errorf("nginx-locations.conf has no `location = %s`", loc)
			continue
		}
		directives := map[string]bool{}
		for _, d := range strings.Split(body, ";") {
			directives[strings.Join(strings.Fields(d), " ")] = true
		}
		if !directives["expires epoch"] {
			t.Errorf("location = %s lacks `expires epoch;`:\n%s", loc, body)
		}
		for _, banned := range []string{"add_header", "Cache-Control", "proxy_", "try_files"} {
			if strings.Contains(body, banned) {
				t.Errorf("location = %s must not use %s:\n%s", loc, banned, body)
			}
		}
	}
	// No location in the file drops the server block's headers.
	if strings.Contains(regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(conf, ""), "add_header") {
		t.Error("nginx-locations.conf uses add_header, which drops the server block's security headers")
	}
	// The website points agents at SKILL.md through skill_url: it is served.
	if _, ok := apexNginxLocation(conf, "=", strings.TrimPrefix(apexSkillURL, "https://agentnetwork.org.cn")); !ok {
		t.Errorf("site.json's skill_url %s is not a location in nginx-locations.conf", apexSkillURL)
	}
}
