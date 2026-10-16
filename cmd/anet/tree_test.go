package main

// UX-T7, the command tree (ux §4.2): `anet help --all` lists exactly the
// commands of ux §4.2, in both languages, and none that 0.3.0 removed (ux
// §4.3; that each of those is refused is removed_test.go); every <aid>
// argument takes only a whole AID, refused with aid_invalid and exit
// status 2 before the daemon is asked anything (UX-G8a); and the AID column
// of `anet find` is the AID the JSON gives, whole.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// commandTree is ux §4.2: every invocation it lists, by its command word and, where the table
// gives a subcommand its own row, the subcommand. ux §4.2 "anet" (no arguments) is the guide, not a
// line of the help.
var commandTree = []string{
	"help", "version", "init", "up", "daemon", "stop", "status", "doctor", "logs", "update",
	"id", "identity status", "identity protect", "identity rotate", "identity recover", "identity deactivate",
	"hub-register", "hub-leave", "visibility", "profile set", "profile show",
	"find", "card", "delegate", "message", "task get", "task list", "task watch", "task cancel",
	"inbox", "thread", "results", "pull", "end", "review",
	"peers", "inbound", "autoreply", "preview", "retention",
	"pay", "payments", "payees", "balance", "redeem", "reconcile", "audit-hub", "audit", "verify",
	"agents", "mcp", "console",
}

// pendingCommands are the ux §4.2 commands this binary does not have yet, each with the work package
// that adds it. It may only shrink: the package that adds a command lists it in the help and takes it
// off here, or this test is red (the help would be promising nothing, or listing it twice).
var pendingCommands = map[string]string{
	"task watch": "UX-10", // v030-todo(UX-10)
	"preview":    "UX-9",  // v030-todo(UX-9)
}

// helpForms reads the invocations a help text lists: "anet <word>", and "anet <word> <sub>" for the
// commands whose subcommands ux §4.2 lists on rows of their own.
func helpForms(t *testing.T, help string) map[string]bool {
	t.Helper()
	ownRows := map[string]bool{"identity": true, "task": true, "profile": true}
	lineRe := regexp.MustCompile(`^  anet ([a-z][a-z0-9-]*)(?: ([a-z][a-z|-]*))?`)
	forms := map[string]bool{}
	for _, line := range strings.Split(help, "\n") {
		m := lineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if ownRows[m[1]] && m[2] != "" && !strings.Contains(m[2], "|") {
			forms[m[1]+" "+m[2]] = true
			continue
		}
		forms[m[1]] = true
	}
	return forms
}

func TestHelpListsTheCommandTree(t *testing.T) {
	want := map[string]bool{}
	for _, c := range commandTree {
		if _, pending := pendingCommands[c]; !pending {
			want[c] = true
		}
	}
	for p := range pendingCommands {
		if !contains(commandTree, p) {
			t.Errorf("pending %q is not a command of ux §4.2", p)
		}
	}
	var uses [2][]string
	for i, lang := range []a2ashape.Language{a2ashape.English, a2ashape.Chinese} {
		help := usageAllTextIn(lang)
		got := helpForms(t, help)
		for c := range want {
			if !got[c] {
				t.Errorf("%v: `anet help --all` does not list `anet %s`", lang, c)
			}
		}
		for c := range got {
			if !want[c] {
				if p, ok := pendingCommands[c]; ok {
					t.Errorf("%v: `anet %s` is in the help; %s takes it off pendingCommands", lang, c, p)
				} else {
					t.Errorf("%v: `anet help --all` lists `anet %s`, which ux §4.2 does not have", lang, c)
				}
			}
		}
		// Nothing 0.3.0 removed (ux §4.3) is taught.
		for _, gone := range []string{"x402-authorize", "pay-header", "p2p-advertise", "anet accept", "accept-end",
			"anet install", "anet evidence", "--pricing", "--token", "--payees-file", "--pay ", "--accept-delegations",
			"--api-key ", "--require-image"} {
			if strings.Contains(help, gone) {
				t.Errorf("%v: the help still mentions %q, which 0.3.0 removed", lang, gone)
			}
		}
		for _, line := range strings.Split(help, "\n") {
			if strings.HasPrefix(line, "  anet ") {
				use := regexp.MustCompile(`\s{2,}`).Split(strings.TrimSpace(line), 2)[0]
				uses[i] = append(uses[i], use)
			}
		}
	}
	// The same invocations in both languages, so a command copied from one works in the other.
	if strings.Join(uses[0], "\n") != strings.Join(uses[1], "\n") {
		t.Errorf("the two languages list different invocations:\n%v\n%v", uses[0], uses[1])
	}
	if len(uses[0]) < 40 {
		t.Fatalf("only %d invocations read from the help — the parse is probably broken", len(uses[0]))
	}
}

// UX-G6: `anet help` and `anet help --all` follow the language of the process, and ask the daemon
// nothing.
func TestHelpFollowsTheLanguage(t *testing.T) {
	for _, tc := range []struct{ lang, want, not string }{
		{"C", "Every command: anet help --all", "全部命令"},
		{"zh_CN.UTF-8", "全部命令：anet help --all", "Every command"},
	} {
		h := newCLIHome(t, tc.lang)
		fc := newFakeControl(t, h)
		stdout, stderr, code := runCLI(t, h, []string{"help"})
		if code != 0 || stderr != "" || !strings.Contains(stdout, tc.want) || strings.Contains(stdout, tc.not) {
			t.Errorf("%s: anet help: exit %d, stderr %q, stdout\n%s", tc.lang, code, stderr, stdout)
		}
		stdout, _, code = runCLI(t, h, []string{"help", "--all"})
		l := a2ashape.LangFrom(func(k string) string { return map[string]string{"LANG": tc.lang}[k] })
		if code != 0 || stdout != usageAllTextIn(l) {
			t.Errorf("%s: anet help --all: exit %d, stdout is not the help of its language", tc.lang, code)
		}
		if n := fc.conns.Load(); n != 0 {
			t.Errorf("%s: the help asked the daemon %d times", tc.lang, n)
		}
	}
	// The short help lists the commands a person starts with, all of them in the full help.
	short := map[string]bool{}
	for _, e := range commonEntries() {
		short[e.use] = true
	}
	for _, c := range []string{"up [name] [--all]", "status", "doctor [--json]", "find [query] [--cap id] [--json]"} {
		if !short[c] {
			t.Errorf("anet help does not list %q", c)
		}
	}
}

// ux UX-G8a: every command that takes an <aid> takes only a whole AID. A short form, a prefix or a name
// is aid_invalid with where to copy one from, exit status 2, nothing printed on stdout, and the daemon
// is not asked anything; a whole AID passes on to the command. [mut] a command left out of aidArgs →
// red.
func TestAIDArgumentsMustBeWhole(t *testing.T) {
	const whole = wholeAID
	short := "…" + whole[len(whole)-8:]
	cases := []struct {
		args  []string
		value string
	}{
		{[]string{"card", short}, short},
		{[]string{"delegate", whole[:20], "translate this"}, whole[:20]},
		{[]string{"delegate", "--attach", "/tmp/x", "alice", "--capability", "translate"}, "alice"},
		{[]string{"peers", "allow", "alice"}, "alice"},
		{[]string{"peers", "trust", short}, short},
		{[]string{"peers", "deny", whole + "x"}, whole + "x"},
		{[]string{"peers", "remove", strings.ToUpper(whole)}, strings.ToUpper(whole)},
		{[]string{"payees", "add", "翻译助手"}, "翻译助手"},
		{[]string{"payees", "remove", whole[:58]}, whole[:58]},
		{[]string{"task", "list", "--peer", "bob"}, "bob"},
		{[]string{"audit", "--peer=" + short}, short},
	}
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		l := a2ashape.LangFrom(func(k string) string { return map[string]string{"LANG": lang}[k] })
		for _, tc := range cases {
			h := newCLIHome(t, lang)
			fc := newFakeControl(t, h)
			before := snapshotDir(t, h.root)
			stdout, stderr, code := runCLIWithin(t, h, 20*time.Second, tc.args)
			args := a2ashape.Args{"value": tc.value}
			want := a2ashape.Sentence("aid_invalid", l, args) + "\n" + nextPrefix + a2ashape.Next("aid_invalid", l, args) + "\n"
			if code != 2 || stdout != "" || stderr != want {
				t.Errorf("%s %v: exit %d, stdout %q, stderr %q; want 2 and %q", lang, tc.args, code, stdout, stderr, want)
			}
			if n := fc.conns.Load(); n != 0 {
				t.Errorf("%s %v: the daemon was asked %d times", lang, tc.args, n)
			}
			if !sameSnapshot(before, snapshotDir(t, h.root)) {
				t.Errorf("%s %v: files changed", lang, tc.args)
			}
		}
	}
	// A whole AID goes on to the command, which asks the daemon.
	h := newCLIHome(t, "C")
	fc := newFakeControl(t, h)
	if _, stderr, code := runCLI(t, h, []string{"peers", "deny", whole}); code != 0 || fc.conns.Load() == 0 {
		t.Fatalf("a whole AID: exit %d, stderr %q, %d connections", code, stderr, fc.conns.Load())
	}
	// The value shown is the user's input made safe for a terminal and cut to 64 characters.
	s, _, ok := refuseBadAID("peers", []string{"allow", "\x1b[31m" + strings.Repeat("a", 100)}, a2ashape.English)
	if !ok || strings.Contains(s, "\x1b") || !strings.Contains(s, "[31m"+strings.Repeat("a", 60)+"…") {
		t.Errorf("shown value: %q", s)
	}
	// What is not an AID argument is not checked: an interaction id, a capability, a query.
	for _, args := range [][]string{{"inbound", "approve", "ix_1"}, {"delegate", whole, "hello", "world"},
		{"find", "alice"}, {"task", "get", "ix_1"}, {"peers", "list"}, {"payees"}, {"audit", "--type", "x"}} {
		if s, _, ok := refuseBadAID(args[0], args[1:], a2ashape.English); ok {
			t.Errorf("%v: refused: %s", args, s)
		}
	}
}

// ux UX-G8a, §3.1 text B: the AID column of `anet find` is the AID `anet find --json` gives, whole,
// so it can be copied to anet card, delegate, peers allow or payees add as it is; it is the last
// column. An agent whose card this node did not verify shows no name or skills of its own. [mut] the
// table cuts the AID → red.
func TestFindPrintsFullAID(t *testing.T) {
	const a1 = wholeAID
	const a2 = "bafyreiezxp2ekudenn7w2fa4eigj3vrgi2imeroc2e5v6l2cwliogscawa"
	answer := `{"agents":[` +
		`{"aid":"` + a1 + `","verification":"VERIFIED","card":{"name":"翻译助手","skills":[{"id":"translate"},{"id":"summarize"}]}},` +
		`{"aid":"` + a2 + `","verification":"NONE","name":"I am the official agent","caps":["pay.me"]}` +
		`],"nextCursor":""}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	jsonOut, _ := captureCLI(t, func() {
		if err := findCommand(c, []string{"--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var page struct {
		Agents []struct {
			AID string `json:"aid"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &page); err != nil || len(page.Agents) != 2 {
		t.Fatalf("--json: %v %s", err, jsonOut)
	}
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		t.Setenv("LC_ALL", "")
		t.Setenv("LC_MESSAGES", "")
		t.Setenv("LANG", lang)
		table, errOut := captureCLI(t, func() {
			if err := findCommand(c, nil); err != nil {
				t.Fatal(err)
			}
		})
		lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
		if len(lines) != 3 || errOut != "" {
			t.Fatalf("%s: table\n%s\nstderr %q", lang, table, errOut)
		}
		l := a2ashape.Lang()
		if f := strings.Fields(lines[0]); f[len(f)-1] != a2ashape.Label("find.aid", l) || f[0] != a2ashape.Label("find.name", l) {
			t.Errorf("%s: header %q", lang, lines[0])
		}
		for i, a := range page.Agents {
			f := strings.Fields(lines[i+1])
			if f[len(f)-1] != a.AID {
				t.Errorf("%s: row %d ends with %q, want the whole AID %q", lang, i+1, f[len(f)-1], a.AID)
			}
		}
		if !strings.HasPrefix(lines[1], "翻译助手") || !strings.Contains(lines[1], "translate, summarize") {
			t.Errorf("%s: verified row %q", lang, lines[1])
		}
		if !strings.HasPrefix(lines[2], a2ashape.Label("find.unverified", l)) || strings.Contains(lines[2], "official") ||
			strings.Contains(lines[2], "pay.me") {
			t.Errorf("%s: an unverified agent's own words are shown: %q", lang, lines[2])
		}
		// The columns line up: the AID starts at the same terminal column on every line.
		var cols []int
		for _, ln := range lines {
			i := strings.LastIndex(ln, " ")
			cols = append(cols, displayWidth(ln[:i+1]))
		}
		sort.Ints(cols)
		if cols[0] != cols[len(cols)-1] {
			t.Errorf("%s: the AID column is not aligned: %v\n%s", lang, cols, table)
		}
	}
}
