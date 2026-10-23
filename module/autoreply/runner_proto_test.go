package autoreply

// runner_proto_test.go — the limits of the runner's protocol and the configuration of auto_reply.runner,
// model_proxy and admitted_budget, on every platform.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

func TestRunnerJobCheck(t *testing.T) {
	ok := runnerJob{Agent: agentHermes, Conversation: "hi", Peer: "abcdEFGH0123-_x"}
	if err := ok.check(); err != nil {
		t.Fatalf("a valid job: %v", err)
	}
	many := make([]runnerFile, runnerMaxFiles+1)
	for i := range many {
		many[i] = runnerFile{Name: "f" + strings.Repeat("x", i+1), Data: []byte("x")}
	}
	for name, mutate := range map[string]func(*runnerJob){
		"no agent":              func(j *runnerJob) { j.Agent = "" },
		"model with a newline":  func(j *runnerJob) { j.Model = "m\nx" },
		"conversation over":     func(j *runnerJob) { j.Conversation = strings.Repeat("x", runnerMaxConversation+1) },
		"instructions over":     func(j *runnerJob) { j.Instructions = strings.Repeat("x", runnerMaxInstructions+1) },
		"too many files":        func(j *runnerJob) { j.Files = many },
		"files over":            func(j *runnerJob) { j.Files = []runnerFile{{Name: "a.png", Data: make([]byte, runnerMaxFileBytes+1)}} },
		"file name with a path": func(j *runnerJob) { j.Files = []runnerFile{{Name: "../evil.png", Data: []byte("x")}} },
		"hidden file name":      func(j *runnerJob) { j.Files = []runnerFile{{Name: ".bashrc", Data: []byte("x")}} },
		"duplicate file names": func(j *runnerJob) {
			j.Files = []runnerFile{{Name: "a", Data: []byte("x")}, {Name: "a", Data: []byte("y")}}
		},
		"variable of the daemon": func(j *runnerJob) { j.Env = map[string]string{"ANET_DATA_DIR": "/d"} },
		"variable outside ANET_": func(j *runnerJob) { j.Env = map[string]string{"LD_PRELOAD": "/evil.so"} },
		"prefix alone":           func(j *runnerJob) { j.Env = map[string]string{"ANET_TURN_": "x"} },
		"lower-case name":        func(j *runnerJob) { j.Env = map[string]string{"ANET_TURN_url": "x"} },
		"value with a newline":   func(j *runnerJob) { j.Env = map[string]string{"ANET_TURN_URL": "x\nLD_PRELOAD=y"} },
		"value over": func(j *runnerJob) {
			j.Env = map[string]string{"ANET_TURN_URL": strings.Repeat("x", runnerMaxEnvValue+1)}
		},
		"negative timeout":         func(j *runnerJob) { j.TimeoutS = -1 },
		"timeout over":             func(j *runnerJob) { j.TimeoutS = int(runnerMaxTimeout.Seconds()) + 1 },
		"peer with a path":         func(j *runnerJob) { j.Peer = "../../etc" },
		"peer of the runs":         func(j *runnerJob) { j.Peer = runnerRunsDirName() },
		"empty peer":               func(j *runnerJob) { j.Peer = "" },
		"peer starting with a dot": func(j *runnerJob) { j.Peer = ".hidden" },
	} {
		j := ok
		mutate(&j)
		if err := j.check(); !errors.Is(err, errRunnerJob) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

// runnerRunsDirName is the per-run parent's name, which no peer may take (runner_serve.go).
func runnerRunsDirName() string { return "_runs" }

// A line over its limit is refused before it is decoded, and a member the line's type does not have is
// refused too (a newer node, an older runner).
func TestRunnerLineLimits(t *testing.T) {
	var j runnerJob
	long := `{"agent":"hermes","conversation":"` + strings.Repeat("x", 200) + `","peer":"p"}` + "\n"
	if err := readRunnerLine(bufio.NewReaderSize(strings.NewReader(long), 16), 100, &j); !errors.Is(err, errRunnerLineTooLong) {
		t.Errorf("a line over the limit: %v", err)
	}
	if err := readRunnerLine(bufio.NewReader(strings.NewReader(long)), 1000, &j); err != nil || len(j.Conversation) != 200 {
		t.Errorf("a line within the limit: %v", err)
	}
	if err := readRunnerLine(bufio.NewReader(strings.NewReader(`{"agent":"hermes","argv":["x"]}`+"\n")), 1000, &j); err == nil {
		t.Error("a member the job does not have was accepted")
	}
	var b bytes.Buffer
	_ = writeRunnerLine(&b, runnerLine{Progress: "started"})
	_ = writeRunnerLine(&b, runnerLine{Exit: runnerExitOK, Reply: "r", Files: []runnerFile{{Name: "a.txt", Data: []byte{0, 1, 2}}}})
	br := bufio.NewReader(&b)
	var l1, l2 runnerLine
	if readRunnerLine(br, runnerLineMax, &l1) != nil || readRunnerLine(br, runnerLineMax, &l2) != nil || l1.Progress != "started" ||
		l2.Exit != runnerExitOK || !bytes.Equal(l2.Files[0].Data, []byte{0, 1, 2}) {
		t.Errorf("round trip: %+v %+v", l1, l2)
	}
}

// The files of a runner's reply land in the outbox under safe names, within the outbox's caps.
func TestWriteRunnerFiles(t *testing.T) {
	ob := t.TempDir()
	if err := writeRunnerFiles(ob, []runnerFile{{Name: "../../evil.txt", Data: []byte("a")}, {Name: "ok.png", Data: []byte("b")}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"evil.txt", "ok.png"} {
		if _, err := os.Stat(filepath.Join(ob, n)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(ob)), "evil.txt")); err == nil {
		t.Error("a file was written outside the outbox")
	}
	many := make([]runnerFile, maxOutboxFiles+1)
	for i := range many {
		many[i] = runnerFile{Name: "f" + strings.Repeat("y", i+1), Data: []byte("x")}
	}
	if err := writeRunnerFiles(t.TempDir(), many); err == nil {
		t.Error("more files than the outbox takes were written")
	}
	if err := writeRunnerFiles(t.TempDir(), []runnerFile{{Name: "a", Data: []byte("x")}, {Name: "a", Data: []byte("y")}}); err == nil {
		t.Error("two files of the same name were written")
	}
}

// The conversation of a job names its images by file name only, and the images ride as files.
func TestRunnerPrompt(t *testing.T) {
	prompt, files := runnerPrompt([]chatTurn{{Role: "user", Text: "look", Images: []chatImage{{Mime: "image/png", Data: []byte("p")}}},
		{Role: "assistant", Text: "seen"}})
	if !strings.Contains(prompt, "Requester attachment: turn0-img0.png (image/png)") || strings.Contains(prompt, string(filepath.Separator)+"turn0") {
		t.Errorf("prompt %q", prompt)
	}
	if len(files) != 1 || files[0].Name != "turn0-img0.png" {
		t.Errorf("files %+v", files)
	}
}

func TestCheckRunnerConfig(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "k")
	good := AutoReplyConfig{Backend: "exec", Agent: agentHermes,
		Runner: &AutoReplyRunnerConfig{Socket: "/run/anet-runner/r.sock", ExpectedUser: "0"},
		ModelProxy: &AutoReplyModelProxyConfig{Listen: "127.0.0.1:47811", Upstreams: []ModelProxyUpstream{
			{Name: "plan", APIBase: "https://ark.example/api/plan/v3", APIKeyFile: keyFile},
			{Name: "local", APIBase: "http://127.0.0.1:11434/v1"}}},
		AdmittedBudget: &AutoReplyAdmittedBudget{RunsPerDay: Budget(10), RunsPerPeerPerDay: Budget(2), RequestsPerRun: Budget(50)}}
	if runtime.GOOS == "windows" {
		if r := checkRunnerConfig(&good); r == nil || r.Code() != "runner_unsupported" {
			t.Fatalf("a runner on Windows: %v", r)
		}
		return
	}
	if r := checkRunnerConfig(&good); r != nil {
		t.Fatalf("a valid block: %v", r)
	}
	for name, c := range map[string]struct {
		mutate func(*AutoReplyConfig)
		code   string
	}{
		"relative socket":       {func(c *AutoReplyConfig) { c.Runner.Socket = "r.sock" }, "config_invalid"},
		"unknown user":          {func(c *AutoReplyConfig) { c.Runner.ExpectedUser = "no-such-user-anet-test" }, "config_invalid"},
		"openai backend":        {func(c *AutoReplyConfig) { c.Backend = "openai" }, "config_invalid"},
		"public listen":         {func(c *AutoReplyConfig) { c.ModelProxy.Listen = "0.0.0.0:47811" }, "config_invalid"},
		"listen by name":        {func(c *AutoReplyConfig) { c.ModelProxy.Listen = "localhost:47811" }, "config_invalid"},
		"reserved name":         {func(c *AutoReplyConfig) { c.ModelProxy.Upstreams[0].Name = "openai" }, "config_invalid"},
		"duplicate name":        {func(c *AutoReplyConfig) { c.ModelProxy.Upstreams[1].Name = "plan" }, "config_invalid"},
		"name with a slash":     {func(c *AutoReplyConfig) { c.ModelProxy.Upstreams[0].Name = "a/b" }, "config_invalid"},
		"plain http elsewhere":  {func(c *AutoReplyConfig) { c.ModelProxy.Upstreams[0].APIBase = "http://ark.example/v3" }, "api_base_insecure"},
		"anthropic api":         {func(c *AutoReplyConfig) { c.ModelProxy.Upstreams[0].APIBase = "https://api.anthropic.com" }, "config_invalid"},
		"relative key file":     {func(c *AutoReplyConfig) { c.ModelProxy.Upstreams[0].APIKeyFile = "k" }, "config_invalid"},
		"per peer over daily":   {func(c *AutoReplyConfig) { c.AdmittedBudget.RunsPerPeerPerDay = Budget(11) }, "budget_invalid"},
		"requests not a number": {func(c *AutoReplyConfig) { c.AdmittedBudget.RequestsPerRun = &BudgetValue{raw: json.RawMessage(`"x"`)} }, "budget_invalid"},
	} {
		raw, _ := json.Marshal(good)
		var cfg AutoReplyConfig
		_ = json.Unmarshal(raw, &cfg)
		c.mutate(&cfg)
		r := checkRunnerConfig(&cfg)
		if r == nil || r.Code() != c.code {
			t.Errorf("%s: want %s, got %v", name, c.code, r)
		}
	}
}

// The blocks merge member by member like every other auto-reply setting, and a misspelled member is
// named in the refusal.
func TestMergeRunnerBlocks(t *testing.T) {
	cur := &AutoReplyConfig{Backend: "exec", Agent: agentHermes}
	next, ref := mergeAutoReply(cur, map[string]json.RawMessage{
		"model_proxy": json.RawMessage(`{"listen":"127.0.0.1:0","upstreams":[{"name":"plan","api_base":"https://ark.example/v3"}]}`)})
	if ref != nil || next.ModelProxy == nil || next.ModelProxy.Upstreams[0].Name != "plan" {
		t.Fatalf("merge: %+v %v", next, ref)
	}
	_, ref = mergeAutoReply(cur, map[string]json.RawMessage{"runner": json.RawMessage(`{"sockett":"/x"}`)})
	if ref == nil || ref.Code() != "config_invalid" || !strings.Contains(a2ashape.Sentence(ref.Key, a2ashape.English, ref.Args), "auto_reply.runner.sockett") {
		t.Fatalf("a misspelled member: %v", ref)
	}
}
