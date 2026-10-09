package daemon

// LEC AV-1 to AV-3, AV-9 and SB-4 (EXE-5): what the other party wrote, the operator's key and the local
// tokens are on no command line anet starts; the conversation reaches every agent whole, on stdin or in
// a file, at any size the conversation bound allows; a sandboxed agent's environment is built from
// nothing and holds no secret.

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// Canaries: each stands for one thing that must never be on a command line.
const (
	canaryGoal   = "CANARY-GOAL-4e1d"   // the requester's goal
	canaryTurn   = "CANARY-TURN-b07c"   // a message of the conversation
	canaryKey    = "CANARY-KEY-6a2f"    // auto_reply.api_key
	canaryIX     = "CANARY-IX-93c5"     // an interaction id the other node chose
	canaryEnvKey = "CANARY-ENVKEY-0d88" // a model key in the daemon's own environment
	canaryProxy  = "CANARY-PROXYPW-71e" // the password in the daemon's proxy setting
)

// cmdlineStub is an agent that records its own command line as the kernel shows it to every local user
// (/proc/<pid>/cmdline, else its arguments), then reports what it was given on stdin and in the
// conversation file its message names. codex reads its reply from -o.
func cmdlineStub(t *testing.T, cmdlineLog string) string {
	t.Helper()
	return writeStub(t, `#!/bin/sh
if [ -r /proc/$$/cmdline ]; then tr '\0' ' ' < /proc/$$/cmdline >> "`+cmdlineLog+`"; else printf '%s ' "$0" "$@" >> "`+cmdlineLog+`"; fi
printf '\n' >> "`+cmdlineLog+`"
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
report() {
  printf 'STDIN: '
  cat
  printf '\n'
  f=$(printf '%s' "$*" | sed -n 's/.*Read the file \(.*conversation\.md\): .*/\1/p')
  if [ -n "$f" ]; then printf 'FILE: '; cat "$f"; printf '\n'; fi
}
if [ -n "$out" ]; then report "$@" > "$out"; else report "$@"; fi
`)
}

// The cmdline canary (AV-1, SI-60, the unit half of J9): a full exec turn of every agent, through the
// replier as the auto-reply loop runs it, with a canary in the goal, in a message, in the API key, in
// the interaction id, in the daemon's model key and in its proxy password. The agent's command line
// carries none of them; the goal and the message reach the agent, on stdin or in its conversation file.
func TestCmdlineCanary(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", canaryEnvKey)
	t.Setenv("HTTPS_PROXY", "http://user:"+canaryProxy+"@proxy.example:3128")
	for _, id := range SupportedExecAgents() {
		t.Run(id, func(t *testing.T) {
			logf := filepath.Join(t.TempDir(), "cmdline.log")
			layout := NewLayout(t.TempDir())
			_ = layout.EnsureRoot()
			r := &execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: id, Command: cmdlineStub(t, logf), APIKey: canaryKey},
				layout: layout, dataDir: layout.Root, aid: "EaidCmdlineCanary0001"}
			rc := replyContext{Role: "inbound", Goal: canaryGoal, InteractionID: canaryIX + "/../x"}
			reply, err := r.Reply(context.Background(), rc, []chatTurn{{Role: "user", Text: canaryTurn}})
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			cmdlines, err := os.ReadFile(logf)
			if err != nil || len(cmdlines) == 0 {
				t.Fatalf("%s: no command line recorded: %v", id, err)
			}
			for _, c := range []string{canaryGoal, canaryTurn, canaryKey, canaryIX, canaryEnvKey, canaryProxy} {
				if strings.Contains(string(cmdlines), c) {
					t.Errorf("%s: %s is on the command line: %s", id, c, cmdlines)
				}
			}
			for _, c := range []string{canaryGoal, canaryTurn} {
				if !strings.Contains(reply, c) {
					t.Errorf("%s: %s never reached the agent: %q", id, c, reply)
				}
			}
		})
	}
}

// AV-3: a conversation over the 128 KiB a single argument may hold (MAX_ARG_STRLEN) reaches every
// agent whole, and the turn succeeds; it used to stop the auto-reply loop with E2BIG.
func TestA200KiBConversationReachesEveryAgent(t *testing.T) {
	var turns []chatTurn
	for i := 0; i < 4; i++ {
		turns = append(turns, chatTurn{Role: "user", Text: strings.Repeat(string(rune('a'+i)), 50<<10)})
	}
	for _, id := range SupportedExecAgents() {
		t.Run(id, func(t *testing.T) {
			layout := NewLayout(t.TempDir())
			_ = layout.EnsureRoot()
			stub := cmdlineStub(t, filepath.Join(t.TempDir(), "cmdline.log"))
			r := &execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: id, Command: stub, APITimeoutSeconds: 60},
				layout: layout, dataDir: layout.Root, aid: "EaidLargeConversation1"}
			reply, err := r.Reply(context.Background(), replyContext{Role: "inbound", Goal: "g", InteractionID: "ix-large"}, turns)
			if err != nil {
				t.Fatalf("%s: a 200 KiB conversation failed: %v", id, err)
			}
			for i := 0; i < 4; i++ {
				if !strings.Contains(reply, strings.Repeat(string(rune('a'+i)), 50<<10)) {
					t.Errorf("%s: message %d did not reach the agent whole", id, i)
				}
			}
		})
	}
}

// The conversation file of a file-input agent is 0600, in the turn's input directory under the work
// dir, and gone when the turn ends (AV-2).
func TestTheConversationFileIsPrivateAndRemoved(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	stub := writeStub(t, `#!/bin/sh
f=$(printf '%s' "$*" | sed -n 's/.*Read the file \(.*conversation\.md\): .*/\1/p')
stat -c '%a %n' "$f" > "`+seen+`"
echo ok
`)
	layout := NewLayout(t.TempDir())
	_ = layout.EnsureRoot()
	r := &execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: agentHermes, Command: stub},
		layout: layout, dataDir: layout.Root, aid: "EaidConversationFile01"}
	if _, err := r.Reply(context.Background(), replyContext{Role: "inbound", Goal: "g", InteractionID: "ix-file"},
		[]chatTurn{{Role: "user", Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	mode, path, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	work, _ := execInteractionDir(layout.Root, "EaidConversationFile01", "ix-file")
	if mode != "600" || filepath.Base(path) != conversationFileName || filepath.Dir(filepath.Dir(path)) != work ||
		!strings.HasPrefix(filepath.Base(filepath.Dir(path)), ".anet-in-") {
		t.Fatalf("conversation file %q mode %s, want 0600 in a .anet-in- directory of %s", path, mode, work)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("the input directory survived the turn: %v", err)
	}
}

// AV-9: a message over 64 KiB is cut at a character boundary and marked; while the goal and the turns
// are over 256 KiB together, the earliest turns go and the first kept says so; the newest stays.
func TestBoundConversation(t *testing.T) {
	small := []chatTurn{{Role: "user", Text: "a"}, {Role: "assistant", Text: "b"}}
	if g, got := boundConversation("goal", small); g != "goal" || len(got) != 2 || got[0].Text != "a" || got[1].Text != "b" {
		t.Fatalf("a small conversation changed: %q %+v", g, got)
	}
	big := strings.Repeat("界", 30<<10) // 90 KiB, 3 bytes a character
	g, got := boundConversation(big, []chatTurn{{Role: "user", Text: big}})
	for _, s := range []string{g, got[0].Text} {
		if len(s) > conversationMaxMessage || !strings.HasSuffix(s, conversationCut) || !utf8.ValidString(s) {
			t.Fatalf("a 90 KiB message: %d bytes, marked %v, valid %v", len(s), strings.HasSuffix(s, conversationCut), utf8.ValidString(s))
		}
	}
	var turns []chatTurn
	for i := 0; i < 10; i++ {
		turns = append(turns, chatTurn{Role: "user", Text: strings.Repeat(string(rune('a'+i)), 60<<10)})
	}
	goal := strings.Repeat("g", 10<<10)
	g, got = boundConversation(goal, turns)
	total := len(g)
	for _, tt := range got {
		total += len(tt.Text)
	}
	if total > conversationMaxBytes {
		t.Fatalf("bounded conversation is %d bytes", total)
	}
	if !strings.HasPrefix(got[0].Text, conversationOmitted+"\n") || got[len(got)-1].Text != turns[9].Text || g != goal {
		t.Fatalf("dropping: first %q…, last kept %v, goal kept %v", got[0].Text[:40], got[len(got)-1].Text == turns[9].Text, g == goal)
	}
	if len(turns[0].Text) != 60<<10 || strings.HasPrefix(turns[0].Text, conversationOmitted) {
		t.Fatal("boundConversation changed the caller's turns")
	}
	// The line saying turns were dropped counts toward both bounds: four 64 KiB messages (the 256 KiB
	// limit exactly, once the earliest is dropped) lose a second one to make room for it, and the first
	// kept, already at 64 KiB, is cut again rather than grown past it.
	turns = nil
	for i := 0; i < 5; i++ {
		turns = append(turns, chatTurn{Role: "user", Text: strings.Repeat(string(rune('k'+i)), 70<<10)})
	}
	_, got = boundConversation("", turns)
	total = 0
	for _, tt := range got {
		total += len(tt.Text)
		if len(tt.Text) > conversationMaxMessage {
			t.Fatalf("a kept message is %d bytes", len(tt.Text))
		}
	}
	if total > conversationMaxBytes || !strings.HasPrefix(got[0].Text, conversationOmitted+"\n") ||
		!strings.HasSuffix(got[0].Text, conversationCut) || got[len(got)-1].Text != cutMessage(turns[4].Text) {
		t.Fatalf("dropping with the marker counted: %d turns, %d bytes", len(got), total)
	}
	// The same when the first kept message is short: the total after dropping the earliest is 10 bytes
	// under 256 KiB, which the line would take over, so the next turns go too.
	turns = []chatTurn{{Role: "user", Text: strings.Repeat("A", 64<<10)}, {Role: "user", Text: "0123456789"}}
	for i := 0; i < 3; i++ {
		turns = append(turns, chatTurn{Role: "user", Text: strings.Repeat(string(rune('C'+i)), 64<<10)})
	}
	turns = append(turns, chatTurn{Role: "user", Text: strings.Repeat("F", 64<<10-20)})
	_, got = boundConversation("", turns)
	total = 0
	for _, tt := range got {
		total += len(tt.Text)
	}
	if total > conversationMaxBytes || !strings.HasPrefix(got[0].Text, conversationOmitted+"\n") {
		t.Fatalf("dropping before a short message: %d turns, %d bytes", len(got), total)
	}
	// One huge newest message alone is kept, cut.
	_, got = boundConversation("", []chatTurn{{Role: "user", Text: strings.Repeat("x", 1<<20)}})
	if len(got) != 1 || len(got[0].Text) > conversationMaxMessage {
		t.Fatalf("a single 1 MiB message: %d turns, %d bytes", len(got), len(got[0].Text))
	}
}

// AV-9 on the exec backend: what the agent is given stays within 256 KiB of conversation, the earliest
// turns dropped and the drop said, the newest kept.
func TestExecReplierBoundsTheConversation(t *testing.T) {
	var turns []chatTurn
	for i := 0; i < 8; i++ {
		turns = append(turns, chatTurn{Role: "user", Text: strings.Repeat(string(rune('a'+i)), 60<<10)})
	}
	layout := NewLayout(t.TempDir())
	_ = layout.EnsureRoot()
	stub := writeStub(t, "#!/bin/sh\ncat\n")
	r := &execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: agentClaude, Command: stub},
		layout: layout, dataDir: layout.Root, aid: "EaidBoundedConversation"}
	got, err := r.Reply(context.Background(), replyContext{Role: "inbound", Goal: "g", InteractionID: "ix-bound"}, turns)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > conversationMaxBytes+4<<10 || !strings.Contains(got, conversationOmitted) ||
		!strings.Contains(got, turns[7].Text) || strings.Contains(got, turns[0].Text) {
		t.Fatalf("the agent was given %d bytes (marker %v)", len(got), strings.Contains(got, conversationOmitted))
	}
}

// AV-9 applies to the openai backend too: what it posts stays within 256 KiB of conversation.
func TestOpenAIBackendBoundsTheConversation(t *testing.T) {
	api := &fakeOpenAI{reply: "ok"}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	r := &openAIReplier{cfg: AutoReplyConfig{APIBase: srv.URL + "/v1", Model: "m"}}
	var turns []chatTurn
	for i := 0; i < 8; i++ {
		turns = append(turns, chatTurn{Role: "user", Text: strings.Repeat("y", 60<<10)})
	}
	if _, err := r.Reply(context.Background(), replyContext{}, turns); err != nil {
		t.Fatal(err)
	}
	body, _ := api.lastBody.Load().(string)
	if len(body) > conversationMaxBytes+8<<10 || !strings.Contains(body, conversationOmitted) {
		t.Fatalf("the openai backend posted %d bytes (marker %v)", len(body), strings.Contains(body, conversationOmitted))
	}
}

// SB-4: a sandboxed agent's environment is built from nothing — PATH (existing directories only),
// locale, terminal, time zone, NO_COLOR, HOME, USER, LOGNAME, TMPDIR=/tmp, ANET_OUTBOX and the egress
// variables of its run — and holds none of the daemon's proxy settings (with their passwords, M-SB-4) or
// keys, nor auto_reply.api_key, nor ANET_EXEC_API_KEY.
func TestSandboxExecEnvIsBuiltFromNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	environ := []string{
		"PATH=/usr/bin:" + missing + ":relative:/usr/bin", "LANG=C.UTF-8", "LC_CTYPE=C", "TERM=xterm", "TZ=UTC",
		"NO_COLOR=1", "HOME=/home/u", "USER=u", "LOGNAME=u", "SHELL=/bin/bash", "LANGUAGE=en",
		"HTTPS_PROXY=http://user:" + canaryProxy + "@proxy.example:3128", "http_proxy=http://user:" + canaryProxy + "@p:1",
		"ALL_PROXY=socks5://user:" + canaryProxy + "@p:2", "NO_PROXY=internal.example",
		"ANTHROPIC_API_KEY=" + canaryEnvKey, "OPENAI_API_KEY=" + canaryEnvKey, "ANET_EXEC_API_KEY=" + canaryEnvKey,
		"SSL_CERT_FILE=/home/u/ca.pem", "XDG_RUNTIME_DIR=/run/user/1000", "ANET_DATA_DIR=/home/u/.anet", "TMPDIR=/home/u/tmp",
	}
	up := &egressUpstream{Name: egressUpstreamAnthropic, Key: canaryKey, Model: "m"}
	env := execEnvFrom(environ, AutoReplyConfig{Agent: agentClaude, APIKey: canaryKey}, &sandboxPlan{Egress: up}, "/w/ob")
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	want := map[string]string{
		"PATH": "/usr/bin", "LANG": "C.UTF-8", "LC_CTYPE": "C", "TERM": "xterm", "TZ": "UTC", "NO_COLOR": "1",
		"HOME": "/home/u", "USER": "u", "LOGNAME": "u", "TMPDIR": "/tmp", "ANET_OUTBOX": "/w/ob",
	}
	for _, kv := range sandboxEgressEnv(up) {
		k, v, _ := strings.Cut(kv, "=")
		want[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("the sandbox env carries %s, which SB-4 does not list", k)
		}
	}
	all := strings.Join(env, "\n")
	for _, c := range []string{canaryProxy, canaryEnvKey, canaryKey, "ANET_EXEC_API_KEY"} {
		if strings.Contains(all, c) {
			t.Errorf("the sandbox env carries %s", c)
		}
	}
	// The direct env keeps the operator's environment for its own agent, and never ANET_EXEC_API_KEY.
	direct := strings.Join(execEnvFrom(environ, AutoReplyConfig{Agent: agentHermes, APIKey: canaryKey}, nil, ""), "\n")
	if strings.Contains(direct, "ANET_EXEC_API_KEY") {
		t.Error("a direct env still carries ANET_EXEC_API_KEY")
	}
}

// AV-1: an interaction's work dir is named by a digest of its id, which another node may have chosen:
// the id (and its slashes, dots and length) is nowhere in the path that bwrap's --bind and the agent's
// --workspace carry. Distinct ids get distinct dirs; the self-test keeps its name.
func TestWorkDirNamesCarryNoPeerText(t *testing.T) {
	ix := canaryIX + "/../../" + strings.Repeat("z", 4096)
	dir, err := execInteractionDir(t.TempDir(), "EaidWorkDirDigest0001", ix)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dir, canaryIX) || strings.Contains(dir, "zzz") || !pathWithin(dir, execWorkRoot()) {
		t.Fatalf("work dir %q carries the interaction id or left the work root", dir)
	}
	if ixDirName(ix) == ixDirName(ix+"x") || ixDirName(ix) != ixDirName(ix) || len(ixDirName(ix)) != len(ixDirName("a")) {
		t.Fatal("ixDirName is not a fixed-length digest")
	}
	if ixDirName("") != "selftest" {
		t.Fatalf("the self-test dir is %q", ixDirName(""))
	}
}

// AV-2: a sandboxed run's instructions say what the agent can reach — models only through this node,
// and the hosts of the egress allowlist, "none" when there are none — so that it tells the other party
// instead of failing in silence; a direct run's say nothing of a sandbox. The instructions are fixed text
// (they go on argv): the role is the one this node set, and nothing of the goal is in them.
func TestASandboxedRunIsToldWhatItCanReach(t *testing.T) {
	rc := replyContext{Role: "inbound", Goal: canaryGoal}
	sandboxed := (&execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: agentClaude}, sandbox: &sandboxPlan{}}).instructions(rc)
	direct := (&execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: agentClaude}}).instructions(rc)
	if !strings.HasSuffix(sandboxed, execSandboxNotice(nil)) || !strings.Contains(sandboxed, "reach on the network are: none.") {
		t.Fatalf("the sandboxed instructions do not end with the sandbox notice:\n%s", sandboxed)
	}
	if strings.Contains(direct, "## Sandbox") || sandboxed != direct+execSandboxNotice(nil) {
		t.Fatalf("the direct instructions differ from the sandboxed ones by more than the notice:\n%s", direct)
	}
	if strings.Contains(sandboxed, canaryGoal) {
		t.Fatal("the instructions hold the goal")
	}
	if n := execSandboxNotice([]string{"pypi.org", "files.example"}); !strings.Contains(n, "are: pypi.org, files.example.") {
		t.Fatalf("the notice does not name the allowed hosts: %q", n)
	}
	// The hosts named are the operator's allowlist (auto_reply.sandbox.egress_allow), in its order.
	allow := AutoReplyConfig{Backend: "exec", Agent: agentClaude,
		Sandbox: &AutoReplySandboxConfig{EgressAllow: []string{"pypi.org", "files.pythonhosted.org"}}}
	if got := (&execReplier{cfg: allow, sandbox: &sandboxPlan{}}).instructions(rc); !strings.HasSuffix(got,
		execSandboxNotice([]string{"pypi.org", "files.pythonhosted.org"})) || !strings.Contains(got, "are: pypi.org, files.pythonhosted.org.") {
		t.Fatalf("the sandboxed instructions do not name the allowlist:\n%s", got)
	}
	if got := (&execReplier{cfg: allow}).instructions(rc); strings.Contains(got, "## Sandbox") {
		t.Fatalf("an unsandboxed run is told about the sandbox:\n%s", got)
	}
	// The operator's system prompt replaces the default and is followed by the same fixed sections.
	own := (&execReplier{cfg: AutoReplyConfig{Backend: "exec", Agent: agentClaude, SystemPrompt: "Be brief."},
		sandbox: &sandboxPlan{}}).instructions(rc)
	if !strings.HasPrefix(own, "Be brief.\n\n## Task") || strings.Contains(own, autoReplyDefaultExecPrompt) {
		t.Fatalf("the operator's system prompt is not the head of the instructions:\n%s", own)
	}
}
