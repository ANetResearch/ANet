package autoreply

// testnode_test.go is the stand-in the module's own tests use for the node they ran inside before the
// module moved out of internal/daemon: a data directory, the module constructed on it, and the sensitive
// paths the kernel would declare. It has the methods those tests called on the daemon, so that they read
// as they did.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/anethome"
	"github.com/ANetResearch/ANet/module"
)

type testLayout struct{ Root string }

func (l testLayout) ControlTokenPath() string { return filepath.Join(l.Root, "control_token.txt") }

// testNode is a data directory with the module on it.
type testNode struct {
	layout  testLayout
	modules []module.Module // other modules of the node, which may declare module.LocalPaths
	mod     *autoReplyModule
}

type testSensitive struct{ n *testNode }

// SensitivePaths is the kernel's fixed list (hostgrants.go) with the local paths of the node's modules.
func (s testSensitive) SensitivePaths() []string {
	paths := []string{s.n.layout.Root, anethome.Home(), filepath.Join("/tmp", fmt.Sprintf("anet-%d", os.Getuid())),
		filepath.Join("/run/user", fmt.Sprint(os.Getuid())), s.n.layout.ControlTokenPath(),
		filepath.Join(anethome.A2ADir(s.n.layout.Root), anethome.A2ATokenFile)}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		paths = append(paths, x)
	}
	if sock := anethome.DefaultC1Socket(); sock != "" {
		paths = append(paths, sock)
	}
	for _, m := range s.n.modules {
		if lp, ok := m.(module.LocalPaths); ok {
			paths = append(paths, lp.LocalPaths()...)
		}
	}
	sort.Strings(paths)
	return resolvedAll(paths)
}

// newBareDaemon is a node with no hub and the module not started (nothing runs until a test starts it).
func newBareDaemon(t *testing.T) *testNode {
	t.Helper()
	root := t.TempDir()
	n := &testNode{layout: testLayout{Root: root}}
	n.mod = &autoReplyModule{env: arEnv{DataDir: root, StateDir: anethome.ModuleDir(root, "autoreply"), AID: "Etestnodeaid00000001"}}
	n.mod.sens = testSensitive{n}
	return n
}

func (n *testNode) ar() *autoReplyModule { return n.mod }

func (n *testNode) runSandboxed(ctx context.Context, req sandboxRequest) (sandboxResult, error) {
	return n.mod.runSandboxed(ctx, req)
}
func (n *testNode) newSandboxPlan() (*sandboxPlan, error) { return n.mod.newSandboxPlan() }
func (n *testNode) sandboxNeverBind() []string            { return n.mod.sandboxNeverBind() }
func (n *testNode) newExecOutbox(ix string, sandboxed bool) (string, func(), error) {
	return n.mod.newExecOutbox(ix, sandboxed)
}
func (n *testNode) sweepSandboxLeftovers() { n.mod.sweepSandboxLeftovers() }
func (n *testNode) reserveRun(cfg AutoReplyConfig, peer string) (*budgetTicket, budgetVerdict, time.Duration) {
	return n.mod.reserveRun(cfg, peer)
}
func (n *testNode) budgetRunsOnChain(since time.Time) []budgetRun {
	return n.mod.budgetRunsOnChain(since)
}

// openSandboxTask is a no-op here: no maintenance runs on a bare node that could remove the work directory
// of an interaction that is not open (the daemon's tests recorded the task for that).
func openSandboxTask(t testing.TB, n *testNode, ix string) { t.Helper() }

// newControlToken writes a control token the way the kernel does and returns it.
func newControlToken(l testLayout) (string, error) {
	tok := "t0k3n" + fmt.Sprint(time.Now().UnixNano())
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(l.ControlTokenPath(), []byte(tok+"\n"), 0o600)
}

// writeStub writes an executable script and returns its path.
func writeStub(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stub.sh")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func quietLog(b testing.TB) {
	b.Helper()
	prev := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(prev) })
}

// liveRoute is the injection route's address in the sandbox, as the agents' configuration must name it.
func liveRoute(name string) string { return "http://127.0.0.1:3128/anet-upstream/" + name }

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

// fakeOpenAI is a minimal OpenAI-compatible /chat/completions endpoint that records the last request
// body and returns a fixed reply. calls counts invocations (to assert contract violations SKIP the API).
type fakeOpenAI struct {
	calls    atomic.Int64
	lastBody atomic.Value // string
	reply    string
	fail     bool
}

func (f *fakeOpenAI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		f.calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		f.lastBody.Store(string(b))
		if f.fail {
			http.Error(w, "model exploded", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": f.reply}}},
		})
	})
}

// NewLayout is the layout of a data directory.
func NewLayout(root string) testLayout { return testLayout{Root: root} }

func (l testLayout) EnsureRoot() error { return os.MkdirAll(l.Root, 0o700) }

// AID is the node's identity, which names its work directories.
func (n *testNode) AID() string { return n.mod.env.AID }

func legacyRuntimeDir() string { return filepath.Join("/tmp", fmt.Sprintf("anet-%d", os.Getuid())) }

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
