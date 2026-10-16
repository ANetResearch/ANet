package daemon

// LEC AV-4, UP-2, UP-3 (EXE-6): each agent's configuration in the sandbox, its command line in a direct
// and in a sandboxed run, and the agents and configurations the sandbox does not run, which fail closed.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// The AV-4 table as the trials left it: which agents run in the sandbox, the model service each uses
// without an api_base, the services whose routes carry the API it speaks, and the {why} of the refusal
// when api_base names another (UP-2). An agent the sandbox does not run says why.
func TestAgentSandboxTable(t *testing.T) {
	type row struct {
		supported bool
		upstream  string
		combo     string
		speaks    []string
	}
	want := map[string]row{
		agentClaude:   {true, egressUpstreamAnthropic, agentComboAnthropicOnly, []string{egressUpstreamAnthropic}},
		agentCodex:    {true, egressUpstreamOpenAI, agentComboOpenAIOnly, []string{egressUpstreamOpenAI}},
		agentOpenCode: {true, "", agentComboNoChat, []string{egressUpstreamCustom, egressUpstreamOpenAI}},
		agentHermes:   {true, "", agentComboNoChat, []string{egressUpstreamCustom, egressUpstreamOpenAI}},
		agentOpenClaw: {true, "", agentComboNoChat, []string{egressUpstreamCustom, egressUpstreamOpenAI}},
		agentCursor:   {false, "", "", nil},
	}
	reg := agentRegistry()
	if len(reg) != len(want) {
		t.Fatalf("%d agents registered, the table has %d", len(reg), len(want))
	}
	for _, a := range reg {
		w, ok := want[a.id]
		if !ok {
			t.Errorf("%s is not in the AV-4 table", a.id)
			continue
		}
		if (a.sandbox != nil) != w.supported {
			t.Errorf("%s: supported in the sandbox %v, want %v", a.id, a.sandbox != nil, w.supported)
			continue
		}
		if a.sandbox == nil {
			if a.sandboxNote == "" {
				t.Errorf("%s is not run in the sandbox and does not say why", a.id)
			}
			continue
		}
		sb := a.sandbox
		if sb.upstream != w.upstream || sb.combo != w.combo || !slices.Equal(sb.speaks, w.speaks) || sb.api == "" {
			t.Errorf("%s: upstream %q combo %q speaks %v api %q", a.id, sb.upstream, sb.combo, sb.speaks, sb.api)
		}
		if sb.upstream != "" && !slices.Contains(sb.speaks, sb.upstream) {
			t.Errorf("%s: its default upstream %s carries no API it speaks", a.id, sb.upstream)
		}
		if a.id != agentClaude && sb.prepare == nil {
			t.Errorf("%s has no configuration for the route", a.id)
		}
	}
}

// liveRoute is the injection route's address in the sandbox, as the agents' configuration must name it.
func liveRoute(name string) string { return "http://127.0.0.1:3128/anet-upstream/" + name }

// AV-4: the configuration each agent is given in the run's home names the injection route, the pinned
// model and the placeholder key, and parses as the agent's format does, whatever the model id holds. The
// files are 0600 in 0700 directories, and nothing of the operator's key is in them.
func TestAgentSandboxConfigFiles(t *testing.T) {
	const model = "m\"o\\d\nel ✓ [x]"
	for _, tc := range []struct {
		agent, upstream, base string
		file                  string
		env                   []string
	}{
		{agentCodex, egressUpstreamOpenAI, liveRoute("openai") + "/v1", ".codex/config.toml", nil},
		{agentOpenCode, egressUpstreamCustom, liveRoute("custom"), ".config/opencode/opencode.json", openCodeFlags},
		{agentOpenCode, egressUpstreamOpenAI, liveRoute("openai") + "/v1", ".config/opencode/opencode.json", openCodeFlags},
		{agentHermes, egressUpstreamCustom, liveRoute("custom"), ".hermes/config.yaml", nil},
		{agentHermes, egressUpstreamOpenAI, liveRoute("openai") + "/v1", ".hermes/config.yaml", nil},
		{agentOpenClaw, egressUpstreamCustom, liveRoute("custom"), ".openclaw/openclaw.json", nil},
		{agentClaude, egressUpstreamAnthropic, "", "", []string{"IS_SANDBOX=1"}},
	} {
		t.Run(tc.agent+"-"+tc.upstream, func(t *testing.T) {
			spec, err := lookupAgent(tc.agent)
			if err != nil || spec.sandbox == nil || spec.sandbox.prepare == nil {
				t.Fatalf("%s: %v", tc.agent, err)
			}
			up := &egressUpstream{Name: tc.upstream, Key: testOperatorKey, Model: model}
			hostHome := filepath.Join(t.TempDir(), "home")
			if err := os.Mkdir(hostHome, 0o700); err != nil {
				t.Fatal(err)
			}
			env, err := spec.sandbox.prepare(up, hostHome, "/home/someone", egressRouteBase(up))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(env, tc.env) {
				t.Errorf("environment %v, want %v", env, tc.env)
			}
			if tc.agent == agentOpenCode {
				// opencode fetches nothing at start-up and reads no project configuration from the work
				// dir, where a peer's earlier turn may have left one.
				for _, f := range []string{"OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_DISABLE_MODELS_FETCH=1",
					"OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_PURE=1"} {
					if !slices.Contains(env, f) {
						t.Errorf("opencode's environment lacks %s", f)
					}
				}
			}
			var files []string
			_ = filepath.WalkDir(hostHome, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				fi, _ := d.Info()
				switch {
				case d.IsDir() && fi.Mode().Perm() != 0o700, !d.IsDir() && fi.Mode().Perm() != 0o600:
					t.Errorf("%s has mode %v", p, fi.Mode().Perm())
				}
				if !d.IsDir() {
					rel, _ := filepath.Rel(hostHome, p)
					files = append(files, rel)
				}
				return nil
			})
			if tc.file == "" {
				if len(files) != 0 {
					t.Errorf("files written: %v", files)
				}
				return
			}
			if len(files) != 1 || files[0] != tc.file {
				t.Fatalf("files %v, want %s", files, tc.file)
			}
			raw, err := os.ReadFile(filepath.Join(hostHome, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), testOperatorKey) {
				t.Errorf("the operator's key is in %s", tc.file)
			}
			gotBase, gotModel, gotKey := readAgentConfig(t, tc.agent, raw, model)
			if gotBase != tc.base || gotModel != model || gotKey != egressPlaceholderKey {
				t.Errorf("%s: base %q model %q key %q; want %q, the pinned model, the placeholder", tc.file, gotBase, gotModel, gotKey, tc.base)
			}
			// The run's home is fresh: a file already there is not written through.
			if _, err := spec.sandbox.prepare(up, hostHome, "/home/someone", egressRouteBase(up)); err == nil {
				t.Error("a second configuration overwrote the first")
			}
		})
	}
}

// readAgentConfig reads the model service address, pinned model and key out of the configuration file an
// agent's prepare wrote (raw), parsed as the agent parses it, and checks the rest of what the file must
// say. model is the pinned model; gotModel is "" unless every place the file names it agrees.
func readAgentConfig(t *testing.T, agent string, raw []byte, model string) (gotBase, gotModel, gotKey string) {
	t.Helper()
	switch agent {
	case agentCodex:
		vals := tomlValues(t, string(raw))
		gotBase, gotModel, gotKey = vals["base_url"], vals["model"], egressPlaceholderKey
		for k, v := range map[string]string{"model_provider": "anet", "name": "anet", "wire_api": "responses",
			"env_key": "OPENAI_API_KEY", "supports_websockets": "false", "web_search": "disabled",
			"check_for_update_on_startup": "false"} {
			if vals[k] != v {
				t.Errorf("codex %s = %q, want %q", k, vals[k], v)
			}
		}
		if !strings.Contains(string(raw), "\n[model_providers.anet]\n") {
			t.Errorf("codex provider table missing:\n%s", raw)
		}
	case agentOpenCode:
		var c struct {
			Provider map[string]struct {
				NPM     string                           `json:"npm"`
				Options struct{ BaseURL, APIKey string } `json:"options"`
				Models  map[string]any                   `json:"models"`
			} `json:"provider"`
			Model      string            `json:"model"`
			Permission map[string]string `json:"permission"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		p := c.Provider["anet"]
		gotBase, gotKey = p.Options.BaseURL, p.Options.APIKey
		if _, ok := p.Models[model]; ok && c.Model == "anet/"+model {
			gotModel = model
		}
		if p.NPM != "@ai-sdk/openai-compatible" || c.Permission["bash"] != "allow" || c.Permission["edit"] != "allow" {
			t.Errorf("opencode configuration %s", raw)
		}
	case agentHermes:
		vals := yamlJSONValues(t, string(raw))
		gotBase, gotModel, gotKey = vals["base_url"], vals["default"], vals["api_key"]
		if !strings.HasPrefix(string(raw), "model:\n  provider: custom\n") {
			t.Errorf("hermes configuration %s", raw)
		}
	case agentOpenClaw:
		var c struct {
			Models struct {
				Providers map[string]struct {
					BaseURL string                `json:"baseUrl"`
					APIKey  string                `json:"apiKey"`
					API     string                `json:"api"`
					Models  []struct{ ID string } `json:"models"`
				} `json:"providers"`
			} `json:"models"`
			Agents struct {
				Defaults struct {
					Model struct{ Primary string } `json:"model"`
				} `json:"defaults"`
			} `json:"agents"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		p := c.Models.Providers["anet"]
		gotBase, gotKey = p.BaseURL, p.APIKey
		if len(p.Models) == 1 && p.Models[0].ID == model && c.Agents.Defaults.Model.Primary == "anet/"+model {
			gotModel = model
		}
		if p.API != "openai-completions" {
			t.Errorf("openclaw api %q", p.API)
		}
	}
	return gotBase, gotModel, gotKey
}

// tomlValues reads `key = value` lines (strings decoded) of the TOML prepareCodex writes.
func tomlValues(t *testing.T, s string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ln := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(ln, " = ")
		if !ok {
			continue
		}
		if strings.HasPrefix(v, `"`) {
			u, err := strconv.Unquote(v)
			if err != nil {
				t.Fatalf("%s: %v", ln, err)
			}
			v = u
		}
		out[k] = v
	}
	return out
}

// yamlJSONValues reads the `  key: "json string"` lines of the YAML prepareHermes writes.
func yamlJSONValues(t *testing.T, s string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ln := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), ": ")
		if !ok || !strings.HasPrefix(v, `"`) {
			continue
		}
		var u string
		if err := json.Unmarshal([]byte(v), &u); err != nil {
			t.Fatalf("%s: %v", ln, err)
		}
		out[k] = u
	}
	return out
}

// The agents' command lines, direct and sandboxed (AV-2, AV-4): claude may use its tools only in the
// sandbox; codex runs outside a git repository, with workspace-write directly and without its own
// sandbox inside anet's, the model and the instructions as settings and the conversation on stdin;
// opencode and openclaw name the pinned model on the route's provider in the sandbox; hermes runs one-shot.
func TestAgentCommandLines(t *testing.T) {
	sb := &sandboxPlan{}
	const ins = "INSTRUCTIONS \"quoted\"\nsecond line"
	base := execInvokeOpts{Instructions: ins, Conversation: "CONVERSATION", Model: "m1", WorkDir: "/w"}
	sandboxed := base
	sandboxed.Sandbox = sb

	if got := strings.Join(claudeArgs(base), " "); !strings.Contains(got, "--permission-mode dontAsk") || strings.Contains(got, "bypass") {
		t.Errorf("claude direct: %s", got)
	}
	if got := strings.Join(claudeArgs(sandboxed), " "); !strings.Contains(got, "--permission-mode bypassPermissions") {
		t.Errorf("claude sandboxed: %s", got)
	}

	for _, tc := range []struct {
		o    execInvokeOpts
		mode string
	}{{base, "workspace-write"}, {sandboxed, "danger-full-access"}} {
		args := codexArgs(tc.o, "/w/.out")
		got := strings.Join(args, "\x00")
		for _, want := range []string{"exec\x00--skip-git-repo-check\x00--ephemeral\x00-s\x00" + tc.mode + "\x00-o\x00/w/.out",
			"\x00-C\x00/w\x00", "\x00-m\x00m1\x00"} {
			if !strings.Contains(got, want) {
				t.Errorf("codex %s: %q lacks %q", tc.mode, args, want)
			}
		}
		if args[len(args)-1] != "-" || slices.Contains(args, "--full-auto") || strings.Contains(got, "CONVERSATION") {
			t.Errorf("codex %s: %q", tc.mode, args)
		}
		i := slices.Index(args, "-c")
		if i < 0 || !strings.HasPrefix(args[i+1], "developer_instructions=") {
			t.Fatalf("codex %s: no developer instructions: %q", tc.mode, args)
		}
		if v, err := strconv.Unquote(strings.TrimPrefix(args[i+1], "developer_instructions=")); err != nil || v != ins {
			t.Errorf("codex instructions %q read back as %q (%v)", args[i+1], v, err)
		}
	}
	if args := codexArgs(execInvokeOpts{}, "/o"); slices.Contains(args, "-c") || slices.Contains(args, "-m") || slices.Contains(args, "-C") {
		t.Errorf("codex without instructions, model or work dir: %q", args)
	}

	if got := openCodeArgs(base); !slices.Equal(got, []string{"run", "--model", "m1", "--", ins}) {
		t.Errorf("opencode direct: %q", got)
	}
	if got := openCodeArgs(sandboxed); !slices.Equal(got, []string{"run", "--model", "anet/m1", "--", ins}) {
		t.Errorf("opencode sandboxed: %q", got)
	}
	if got := openClawArgs(base, "main", "/w/in/conversation.md"); !slices.Equal(got,
		[]string{"agent", "--local", "--json", "--agent", "main", "--message-file", "/w/in/conversation.md", "--model", "m1"}) {
		t.Errorf("openclaw direct: %q", got)
	}
	if got := openClawArgs(sandboxed, "main", "/f"); !slices.Equal(got,
		[]string{"agent", "exec", "--json", "--message-file", "/f", "--cwd", "/w", "--model", "anet/m1"}) {
		t.Errorf("openclaw sandboxed: %q", got)
	}
	if got := hermesArgs(base, "/f/conversation.md"); len(got) != 3 || !strings.HasPrefix(got[0], "--oneshot="+ins) ||
		!strings.Contains(got[0], "Read the file /f/conversation.md") || got[1] != "-m" || got[2] != "m1" {
		t.Errorf("hermes: %q", got)
	}
}

// openclaw prints {"payloads":[{"text":…}],"meta":…} with --json (2026.9); the reply is the payloads'
// text. The single text members of older versions, and output that is not JSON, still read as before.
func TestOpenClawReply(t *testing.T) {
	for in, want := range map[string]string{
		`{"payloads":[{"text":" FILE-OK ","mediaUrl":null}],"meta":{"durationMs":1}}`: "FILE-OK",
		`{"payloads":[{"text":"one"},{"text":""},{"text":"two"}]}`:                    "one\n\ntwo",
		`{"payloads":[],"reply":"older"}`:                                             "older",
		`{"result":"r"}`:                                                              "r",
		`plain text`:                                                                  "plain text",
		`{"meta":{}}`:                                                                 `{"meta":{}}`,
		`[1,2]`:                                                                       `[1,2]`,
	} {
		if got := openClawReply(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// openClawReply reads what the agent printed: it never panics, returns something for any non-blank
// output, and returns the payloads' text when the output has them.
func FuzzOpenClawReply(f *testing.F) {
	for _, s := range []string{`{"payloads":[{"text":"a"}]}`, `{"reply":"x"}`, `x`, `{"payloads":[{"text":5}]}`, `{"payloads":null}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		raw = strings.TrimSpace(raw)
		got := openClawReply(raw)
		if raw != "" && got == "" {
			var v map[string]any
			if json.Unmarshal([]byte(raw), &v) != nil {
				t.Fatalf("non-JSON output %q read as nothing", raw)
			}
		}
		var p struct {
			Payloads []struct{ Text string } `json:"payloads"`
		}
		if json.Unmarshal([]byte(raw), &p) == nil {
			for _, x := range p.Payloads {
				if t2 := strings.TrimSpace(x.Text); t2 != "" && !strings.Contains(got, t2) {
					t.Fatalf("payload %q missing from %q", t2, got)
				}
			}
		}
	})
}

// tomlString is a TOML basic string that reads back as the same text (TOML's escapes are a subset of Go's
// string literal escapes, so strconv.Unquote reads it as TOML does).
func FuzzTOMLString(f *testing.F) {
	for _, s := range []string{"", "plain", "q\"b\\n\nt\tr\r\x00\x1f\x7f", "✓ ünï", " "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			s = strings.ToValidUTF8(s, "�")
		}
		q := tomlString(s)
		if strings.ContainsAny(q[1:len(q)-1], "\n\r") {
			t.Fatalf("%q spans lines", q)
		}
		got, err := strconv.Unquote(q)
		if err != nil || got != s {
			t.Fatalf("%q reads back as %q (%v)", s, got, err)
		}
	})
}

// LEC AV-4 (EXE-6): an agent the sandbox does not run, and a configuration whose model service does not
// carry the agent's API, fail closed at run time. The inbound task is answered status{rejected,
// anet.reason=sandbox_unavailable}, the local detail is agent_unsupported, and the agent never starts.
func TestUnsupportedAgentsFailClosedInTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  AutoReplyConfig
	}{
		{"cursor", AutoReplyConfig{Agent: agentCursor, APIKey: "k"}},
		{"codex-custom", AutoReplyConfig{Agent: agentCodex, APIKey: "k", Model: "qwen3"}},
		{"claude-custom", AutoReplyConfig{Agent: agentClaude, APIKey: "k", Model: "qwen3"}},
		{"opencode-anthropic", AutoReplyConfig{Agent: agentOpenCode, APIKey: "k", Model: "m"}},
		{"hermes-anthropic", AutoReplyConfig{Agent: agentHermes, APIKey: "k", Model: "m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := execStub(t)
			cfg := tc.cfg
			cfg.Backend, cfg.Untrusted = "exec", UntrustedSandbox
			f := newAutoReplyFixture(t, cfg, nil)
			// The fixture sets a loopback api_base; each case sets the one it is about.
			switch tc.name {
			case "cursor":
				f.cfg.APIBase = ""
			case "codex-custom", "claude-custom":
				// a local model service: codex speaks only the Responses API, which the custom route does not
				// carry; claude only the Anthropic API, which only api.anthropic.com's route carries
				f.cfg.APIBase = "http://127.0.0.1:11434/v1"
			default:
				f.cfg.APIBase = "https://api.anthropic.com"
			}
			if _, err := resolveEgressUpstream(f.cfg); err == nil {
				t.Fatalf("%+v resolves", f.cfg)
			} else if pe := (*sandboxProbeError)(nil); !errors.As(err, &pe) || pe.Code != "agent_unsupported" {
				t.Fatalf("%+v: %v", f.cfg, err)
			}
			id, err := f.req.Delegate(f.ctx, f.prov.AID(), "run something", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.tick(t)
			if n := runs(); n != 0 {
				t.Fatalf("the agent ran %d times", n)
			}
			if st := stateOf(t, f.prov, id); st != interactions.StateRejected {
				t.Fatalf("inbound task: %s", st)
			}
			if meta := awaitState(t, f.req, id, interactions.StateRejected); meta["anet.reason"] != reasonSandboxUnavailble {
				t.Fatalf("requester told %v", meta)
			}
			_, recs := f.prov.ledger.Evidence(EvidenceQuery{EventType: EvAutoReplyInvoked})
			ev := string(mustJSON(t, recs))
			if len(recs) != 1 || !strings.Contains(ev, `"exit":"sandbox_unavailable"`) || !strings.Contains(ev, `"unavailable":"agent_unsupported"`) {
				t.Fatalf("invocation evidence %s", ev)
			}
		})
	}
}

// openCodeArgsStub parses its arguments as opencode run does (what follows "--" joins the message): --model takes a
// value, any other argument starting with "-" before "--" is an option it does not know (an error), the
// other words and everything after "--" are the message. It writes the message beside itself.
const openCodeArgsStub = `#!/bin/sh
here=$(dirname "$0")
[ "$1" = run ] || { echo "not a run" >&2; exit 2; }
shift
msg=""
while [ $# -gt 0 ]; do
  case "$1" in
    --) shift; for a in "$@"; do msg="$msg${msg:+ }$a"; done; break ;;
    --model) shift 2 ;;
    -*) echo "unknown option $1" >&2; exit 2 ;;
    *) msg="$msg${msg:+ }$1"; shift ;;
  esac
done
printf '%s' "$msg" > "$here/message"
echo STUB-OK
`

// hermesArgsStub parses its arguments as hermes does (argparse): -z/--oneshot and -m/--model take a value,
// given as the next argument or after "="; a next argument starting with "-" is not a value but an
// option, so -z is left without one (an error). It writes the one-shot message beside itself.
const hermesArgsStub = `#!/bin/sh
here=$(dirname "$0")
msg=""
while [ $# -gt 0 ]; do
  case "$1" in
    --oneshot=*) msg=${1#--oneshot=}; shift ;;
    -z|--oneshot)
      case "$2" in -*|"") echo "argument -z/--oneshot: expected one argument" >&2; exit 2 ;; esac
      msg=$2; shift 2 ;;
    -m|--model) shift 2 ;;
    *) echo "unrecognized arguments: $1" >&2; exit 2 ;;
  esac
done
printf '%s' "$msg" > "$here/message"
echo STUB-OK
`

// hermesArgparse is hermes's top-level parser as far as the one-shot run goes (-z/--oneshot,
// -m/--model and the options around them), run on Python's own argparse: it prints the one-shot message
// as JSON, or exits 2 as hermes does when its arguments do not parse.
const hermesArgparse = `
import argparse, json, sys
p = argparse.ArgumentParser(prog="hermes")
p.add_argument("-z", "--oneshot", metavar="PROMPT", default=None)
p.add_argument("-m", "--model", default=None)
p.add_argument("--provider", default=None)
p.add_argument("-t", "--toolsets", default=None)
p.add_argument("-s", "--skills", action="append", default=None)
p.add_argument("-r", "--resume", default=None)
p.add_argument("-c", "--continue", dest="continue_last", nargs="?", const=True, default=None)
p.add_argument("--usage-file", default=None)
p.add_argument("--yolo", action="store_true")
p.add_argument("-v", "--version", action="store_true")
ns = p.parse_args(sys.argv[1:])
print(json.dumps(ns.oneshot))
`

// LEC AV-2, AV-4 (EXE-6 record §5 item 9): an operator's system_prompt that starts with "-" stays the
// message of opencode and hermes, direct and in the sandbox, on every platform (the command line is the
// same for both sandboxes). opencode is given it after "--", hermes as --oneshot=MESSAGE; each stand-in
// parses its arguments as the CLI does and receives the instructions whole, and Python's argparse — what
// hermes parses with — reads the message out of hermes's command line unchanged.
func TestASystemPromptStartingWithADashStaysAMessage(t *testing.T) {
	prompts := []string{"-v", "--model evil-model", "-m evil be brief", "-zx", "--oneshot=x", "--", "-",
		"- a list item first\n- then another", "--help me", "-c", "-s skill"}
	run := func(t *testing.T, agent, stub string, o execInvokeOpts) string {
		t.Helper()
		bin := writeStub(t, stub)
		o.AgentID, o.Command, o.WorkDir, o.Env = agent, bin, t.TempDir(), []string{"PATH=/usr/bin:/bin"}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		reply, err := InvokeAgent(ctx, o)
		if err != nil || reply != "STUB-OK" {
			t.Fatalf("%s with instructions %q: %q %v", agent, o.Instructions, reply, err)
		}
		b, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "message"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, ins := range prompts {
		for _, sandboxed := range []bool{false, true} {
			o := execInvokeOpts{Instructions: ins, Conversation: "CONVERSATION", Model: "m1"}
			if sandboxed {
				o.Sandbox = &sandboxPlan{}
				// The command line in the sandbox is the one built here; the stand-in runs it directly.
				args := openCodeArgs(o)
				if i := slices.Index(args, "--"); i < 0 || !slices.Equal(args[i+1:], []string{ins}) {
					t.Errorf("opencode sandboxed, instructions %q: %q", ins, args)
				}
				args = hermesArgs(o, "/w/.anet-in-x/conversation.md")
				if args[0] != hermesOneShot+fileInputMessage(ins, "/w/.anet-in-x/conversation.md") {
					t.Errorf("hermes sandboxed, instructions %q: %q", ins, args)
				}
				continue
			}
			if got := run(t, agentOpenCode, openCodeArgsStub, o); got != ins {
				t.Errorf("opencode's message is %q, want the instructions %q", got, ins)
			}
			if got := run(t, agentHermes, hermesArgsStub, o); !strings.HasPrefix(got, ins+"\n\n") ||
				!strings.Contains(got, "/"+conversationFileName) {
				t.Errorf("hermes's message is %q, want the instructions %q and the conversation file", got, ins)
			}
		}
	}

	// hermes's own parser: Python's argparse reads the message out of the command line hermesArgs builds.
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("the argparse half needs python3 on PATH: %v", err)
	}
	for _, ins := range prompts {
		args := hermesArgs(execInvokeOpts{Instructions: ins, Model: "m1"}, "/w/c.md")
		out, err := exec.Command(py, append([]string{"-I", "-c", hermesArgparse}, args...)...).CombinedOutput()
		var got string
		if err != nil || json.Unmarshal(out, &got) != nil || got != fileInputMessage(ins, "/w/c.md") {
			t.Errorf("argparse read hermes %q as %q (%v)", args, out, err)
		}
	}
	// The spelling before this fix, -z MESSAGE, does not parse for such a message: the reason for it.
	out, err := exec.Command(py, "-I", "-c", hermesArgparse, "-z", "-m evil be brief", "-m", "m1").CombinedOutput()
	if err == nil {
		t.Errorf("argparse took -z with a message starting with -m: %s", out)
	}
}
