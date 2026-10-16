//go:build linux

package autoreply

// LEC AV-4 on Linux (EXE-6): the agent's configuration reaches the run's home before the agent starts, a
// Python agent's environment is visible in the sandbox, and, on the real sandbox, each agent finds its
// configuration for the injection route in its $HOME.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// command runs the plan's AgentHome in the run's home before anything starts: what it writes is in the
// home directory bwrap binds at $HOME, and the variables it returns are in the agent's environment. A
// configuration that cannot be written makes the sandbox unavailable and leaves no run directory.
func TestSandboxCommandWritesTheAgentConfiguration(t *testing.T) {
	agent := writeStub(t, "#!/bin/sh\necho hi\n")
	home := filepath.Join(t.TempDir(), "home")
	var gotHost, gotHome, gotRoute string
	up := &egressUpstream{Name: egressUpstreamCustom}
	plan := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: "EaidAgentHome0000001", Home: home, Egress: up,
		AgentHome: func(hostHome, inside, route string) ([]string, error) {
			gotHost, gotHome, gotRoute = hostHome, inside, route
			return []string{"AGENT_CONFIGURED=1"}, writeHomeFile(hostHome, filepath.Join(".agent", "config"), []byte("route"))
		}}
	l, err := plan.command(t.TempDir(), []string{agent}, []string{"PATH=/usr/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if gotHome != home {
		t.Errorf("the agent's home is %q inside, want %q", gotHome, home)
	}
	// On Linux the route is the sandbox's own 127.0.0.1:3128, which carries no key.
	if gotRoute != liveRoute(egressUpstreamCustom) {
		t.Errorf("the agent's configuration is for the route %q, want %q", gotRoute, liveRoute(egressUpstreamCustom))
	}
	bound := ""
	for i := 0; i+2 < len(l.Args); i++ {
		if l.Args[i] == "--bind" && l.Args[i+2] == home {
			bound = l.Args[i+1]
		}
	}
	if bound == "" || bound != gotHost {
		t.Fatalf("the configuration went to %q, the sandbox's $HOME is %q", gotHost, bound)
	}
	if b, err := os.ReadFile(filepath.Join(bound, ".agent", "config")); err != nil || string(b) != "route" {
		t.Errorf("the configuration in the bound home: %q %v", b, err)
	}
	raw, _ := io.ReadAll(l.Files[0])
	var spec sandboxInitSpec
	if err := decodeSandboxSpec(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(spec.Env, "AGENT_CONFIGURED=1") {
		t.Errorf("the agent's environment lacks what its configuration needs: %v", spec.Env)
	}

	failing := &sandboxPlan{Bwrap: "/usr/bin/bwrap", AID: "EaidAgentHomeFails01", Home: home,
		AgentHome: func(string, string, string) ([]string, error) { return nil, errors.New("disk full") }}
	if _, err := failing.command(t.TempDir(), []string{agent}, nil, nil); !errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("a configuration that cannot be written: %v", err)
	}
	ents, _ := os.ReadDir(sandboxRunRoot())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "eFails01-") {
			t.Errorf("run directory %s left behind", e.Name())
		}
	}
}

// A Python agent (hermes, installed with pip into a virtual environment) runs with that environment's
// packages: the environment's root, with its pyvenv.cfg and lib/, is listed for the sandbox, as is the
// prefix of a Python installed outside the system directories, both by the link uv makes for the minor
// version (which the venv's python names) and by the patch release it points to; the parent of some
// other bin directory (a "#!" naming ~/bin/python) is not.
func TestAgentRuntimeShowsAPythonEnvironment(t *testing.T) {
	root := t.TempDir()
	venv := filepath.Join(root, "venvs", "hermes")
	pyhome := filepath.Join(root, "pythons", "cpython-3.12.15")
	pylink := filepath.Join(root, "pythons", "cpython-3.12")
	plain := filepath.Join(root, "plainhome")
	for _, d := range []string{filepath.Join(venv, "bin"), filepath.Join(venv, "lib", "python3.12", "site-packages"),
		filepath.Join(pyhome, "bin"), filepath.Join(pyhome, "lib", "python3.12"), filepath.Join(plain, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	interp := filepath.Join(pyhome, "bin", "python3.12")
	if err := os.WriteFile(interp, []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = "+filepath.Dir(interp)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(pyhome), pylink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(pylink, "bin", "python3.12"), filepath.Join(venv, "bin", "python")); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(venv, "bin", "hermes")
	if err := os.WriteFile(script, []byte("#!"+filepath.Join(venv, "bin", "python")+"\nimport sys\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, dirs, err := agentRuntime(script)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{venv, pylink, pyhome} {
		if !slices.Contains(dirs, want) {
			t.Errorf("runtime dirs %v lack %s", dirs, want)
		}
	}
	// A "#!" naming a python in a bin directory that is neither a venv nor an installation.
	if err := os.WriteFile(filepath.Join(plain, "bin", "python"), []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "tool")
	if err := os.WriteFile(other, []byte("#!"+filepath.Join(plain, "bin", "python")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, dirs, err = agentRuntime(other)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(dirs, plain) {
		t.Errorf("the parent of a plain bin directory is shown: %v", dirs)
	}
	// Not a Python at all: nothing beyond the interpreter's directory.
	if got := pythonRuntime(filepath.Join(venv, "bin", "node"), filepath.Join(venv, "bin", "node")); got != nil {
		t.Errorf("a node interpreter lists %v", got)
	}
}

// configStub is an agent that copies whatever agent configuration it finds in its $HOME, and its
// environment, into ./seen of its working directory, and answers as each agent does (codex in its -o
// file, openclaw as JSON).
const configStub = `#!/bin/sh
mkdir -p seen
for f in .codex/config.toml .config/opencode/opencode.json .hermes/config.yaml .openclaw/openclaw.json; do
  if [ -f "$HOME/$f" ]; then cp "$HOME/$f" "seen/$(basename "$f")"; fi
done
env > seen/env
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ "$1" = "agent" ]; then echo '{"payloads":[{"text":"STUB-OK"}]}'; elif [ -n "$out" ]; then echo STUB-OK > "$out"; else echo STUB-OK; fi
`

// AV-4 on the real sandbox: a sandboxed turn of each agent the sandbox runs finds, in its $HOME, the
// configuration that points it at the run's injection route with the pinned model, and the variables
// that go with it. Skipped where bwrap is missing or cannot create namespaces (the CI sandbox job and the
// lab hosts treat a skip as a failure).
func TestSandboxIntegrationConfiguresEachAgent(t *testing.T) {
	pr := realBwrapProbe(t, func(f bwrapFeatures) bwrapFeatures { return f })
	if pr.Err != nil {
		t.Fatalf("the sandbox failed its self-check: %v", pr.Err)
	}
	f := newFakeModelService(t)
	_ = testUpstream(t, egressUpstreamAnthropic, f)
	_ = testUpstream(t, egressUpstreamOpenAI, f)
	stub := writeStub(t, configStub)
	for _, tc := range []struct {
		agent, apiBase, model string
		file                  string // what it finds in $HOME
		want                  []string
		env                   []string
	}{
		{agentClaude, "", "", "", nil, []string{"ANTHROPIC_BASE_URL=" + liveRoute("anthropic"), "IS_SANDBOX=1"}},
		{agentCodex, "", "", "config.toml", []string{`base_url = "` + liveRoute("openai") + `/v1"`,
			`model = "` + egressCheapestModel[egressUpstreamOpenAI] + `"`, `web_search = "disabled"`}, nil},
		{agentOpenCode, f.srv.URL + "/v1", "local-m", "opencode.json", []string{`"baseURL": "` + liveRoute("custom") + `"`,
			`"model": "anet/local-m"`}, openCodeFlags},
		{agentHermes, f.srv.URL + "/v1", "local-m", "config.yaml", []string{`base_url: "` + liveRoute("custom") + `"`,
			`default: "local-m"`}, nil},
		{agentOpenClaw, f.srv.URL + "/v1", "local-m", "openclaw.json", []string{`"baseUrl": "` + liveRoute("custom") + `"`,
			`"primary": "anet/local-m"`}, nil},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			d := newBareDaemon(t)
			ix := "ix-config-" + tc.agent
			openSandboxTask(t, d, ix)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := d.runSandboxed(ctx, sandboxRequest{
				Cfg: AutoReplyConfig{Backend: "exec", Agent: tc.agent, Command: stub, APIKey: testOperatorKey,
					APIBase: tc.apiBase, Model: tc.model, APITimeoutSeconds: 50},
				RC:    replyContext{Role: "inbound", InteractionID: ix, Goal: "g"},
				Turns: []chatTurn{{Role: "user", Text: "hi"}},
			})
			if err != nil || !res.Sandboxed || !strings.Contains(res.Reply, "STUB-OK") {
				t.Fatalf("sandboxed turn: %q %v", res.Reply, err)
			}
			work, _ := execSandboxInteractionDir(d.layout.Root, d.AID(), ix)
			seen := filepath.Join(work, "seen")
			ents, _ := os.ReadDir(seen)
			var names []string
			for _, e := range ents {
				if e.Name() != "env" {
					names = append(names, e.Name())
				}
			}
			if tc.file == "" && len(names) != 0 || tc.file != "" && (len(names) != 1 || names[0] != tc.file) {
				t.Fatalf("the agent found %v in its home, want %q", names, tc.file)
			}
			if tc.file != "" {
				b, _ := os.ReadFile(filepath.Join(seen, tc.file))
				for _, w := range tc.want {
					if !strings.Contains(string(b), w) {
						t.Errorf("%s lacks %s:\n%s", tc.file, w, b)
					}
				}
				if strings.Contains(string(b), testOperatorKey) {
					t.Errorf("%s holds the operator's key", tc.file)
				}
			}
			env, _ := os.ReadFile(filepath.Join(seen, "env"))
			for _, w := range tc.env {
				if !strings.Contains("\n"+string(env), "\n"+w+"\n") {
					t.Errorf("the agent's environment lacks %s", w)
				}
			}
			if strings.Contains(string(env), testOperatorKey) {
				t.Error("the agent's environment holds the operator's key")
			}
		})
	}
	if ents, _ := os.ReadDir(sandboxRunRoot()); len(ents) != 0 {
		t.Errorf("run directories left behind: %d", len(ents))
	}
}
