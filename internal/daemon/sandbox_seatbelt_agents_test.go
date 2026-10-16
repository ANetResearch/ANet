//go:build linux || darwin

package daemon

// The agents that read their model service's address from a file of their own, in the macOS sandbox (LEC
// EG-3 as revised 2026-10-10, AV-4): the file names the keyed route. Driven, like the rest of
// sandbox_seatbelt_test.go, through the stand-in for sandbox-exec, so it runs on Linux and on a Mac.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// darwinConfigStub is an agent that copies the configuration it finds in its $HOME, and its environment,
// into ./seen of its working directory, and answers.
const darwinConfigStub = `#!/bin/sh
mkdir -p seen
for f in .codex/config.toml .config/opencode/opencode.json .hermes/config.yaml .openclaw/openclaw.json; do
  if [ -f "$HOME/$f" ]; then cp "$HOME/$f" "seen/$(basename "$f")"; ls -l "$HOME/$f" > seen/mode; fi
done
env > seen/env
echo STUB-OK
`

// EG-3 (2026-10-10), AV-4, MS-3: on macOS codex, opencode, hermes and openclaw are pointed at their model
// service by a file in the run's home, and the endpoint there takes nothing without the run's key — so
// the file names the keyed route, http://127.0.0.1:<P>/anet-upstream/<S>/<name>, the same key and port as
// the agent's environment. A request on that address reaches the model service with the operator's key;
// the same address without the key is refused. The file is 0600 and goes with the run directory; the key
// is on no command line, in no profile, log or evidence.
func TestDarwinAgentConfigsCarryTheKeyedRoute(t *testing.T) {
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	f := newFakeModelService(t)
	sbx := standInSandboxExec(t, "run")
	for _, tc := range []struct {
		agent, upstream, file string
		call                  string // the model call the agent makes, relative to the configured address
	}{
		{agentCodex, egressUpstreamOpenAI, "config.toml", "/responses"},
		{agentOpenCode, egressUpstreamCustom, "opencode.json", "/chat/completions"},
		{agentOpenCode, egressUpstreamOpenAI, "opencode.json", "/chat/completions"},
		{agentHermes, egressUpstreamCustom, "config.yaml", "/chat/completions"},
		{agentOpenClaw, egressUpstreamCustom, "openclaw.json", "/chat/completions"},
	} {
		t.Run(tc.agent+"-"+tc.upstream, func(t *testing.T) {
			f.reset()
			up := testUpstream(t, tc.upstream, f)
			spec, err := lookupAgent(tc.agent)
			if err != nil || spec.sandbox == nil {
				t.Fatal(err)
			}
			plan := &sandboxPlan{SandboxExec: sbx, AID: "EaidDarwinAgentCfg01", Egress: up,
				AgentHome: spec.sandbox.homeFor(up), NeverBind: []string{filepath.Join(t.TempDir(), "never")}}
			work := t.TempDir()
			l, err := seatbeltCommand(plan, work, []string{writeStub(t, darwinConfigStub)}, []string{"PATH=/usr/bin:/bin", "LANG=C"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			closed := false
			defer func() {
				if !closed {
					l.Close()
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, l.Path, l.Args...)
			cmd.Env, cmd.ExtraFiles, cmd.Dir = l.Env, l.Files, work
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil || strings.TrimSpace(stdout.String()) != "STUB-OK" {
				t.Fatalf("the run: %v %q\n%s", err, stdout.String(), stderr.String())
			}

			envRaw, _ := os.ReadFile(filepath.Join(work, "seen", "env"))
			env := envOf(strings.Split(strings.TrimSpace(string(envRaw)), "\n"))
			key, addr, ok := strings.Cut(strings.TrimPrefix(env["HTTPS_PROXY"], "http://"+egressKeyUser+":"), "@")
			if !ok || len(key) != 2*egressKeyBytes {
				t.Fatalf("the run's key and endpoint: HTTPS_PROXY=%q", env["HTTPS_PROXY"])
			}
			raw, err := os.ReadFile(filepath.Join(work, "seen", tc.file))
			if err != nil {
				t.Fatalf("the agent found no %s in its home: %v", tc.file, err)
			}
			if mode, _ := os.ReadFile(filepath.Join(work, "seen", "mode")); !strings.HasPrefix(string(mode), "-rw-------") {
				t.Errorf("the configuration's mode: %s", mode)
			}
			base, model, placeholder := readAgentConfig(t, tc.agent, raw, up.Model)
			want := "http://" + addr + egressRoutePrefix + key + "/" + up.Name
			if up.Name == egressUpstreamOpenAI {
				want += "/v1"
			}
			if base != want || model != up.Model || placeholder != egressPlaceholderKey {
				t.Fatalf("%s names %q (model %q, key %q), want the keyed route %q", tc.file, base, model, placeholder, want)
			}
			if strings.Contains(string(raw), testOperatorKey) {
				t.Errorf("%s holds the operator's key", tc.file)
			}

			// The configured address is the run's endpoint: with the key the call reaches the model
			// service with the operator's key; the same address without the key is refused.
			body := `{"model":"x","messages":[{"role":"user","content":"hi"}]}`
			if tc.call == "/responses" {
				body = `{"model":"x","input":"hi"}`
			}
			post := func(u string) (int, string) {
				req, _ := http.NewRequest("POST", u, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+egressPlaceholderKey)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, string(b)
			}
			if code, b := post(base + tc.call); code != 200 || len(f.requests()) != 1 ||
				f.requests()[0].Header.Get("Authorization") != "Bearer "+testOperatorKey {
				t.Errorf("a call on the configured address: %d %s; the service saw %d requests", code, b, len(f.requests()))
			}
			if code, _ := post(strings.Replace(base, egressRoutePrefix+key+"/", egressRoutePrefix, 1) + tc.call); code != http.StatusNotFound {
				t.Errorf("the same address without the key: %d", code)
			}
			if len(f.requests()) != 1 {
				t.Errorf("the service saw %d requests", len(f.requests()))
			}

			profile, _ := os.ReadFile(filepath.Join(filepath.Dir(sbx), "profile.copy"))
			ev, _ := json.Marshal(plan.egressSummary())
			for where, text := range map[string]string{"command line": strings.Join(append([]string{l.Path}, l.Args...), " "),
				"profile": string(profile), "log": logs.String(), "evidence": string(ev)} {
				if strings.Contains(text, key) {
					t.Errorf("the %s holds the run's key", where)
				}
			}
			home := env["HOME"]
			l.Close()
			closed = true
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Errorf("the run's home, with the configuration, is left after the run: %v", err)
			}
			ev, _ = json.Marshal(plan.egressSummary())
			if strings.Contains(string(ev), key) || !strings.Contains(string(ev), `"model_requests":1`) {
				t.Errorf("the run's evidence %s", ev)
			}
		})
	}
}

// j8Answer is what the scripted model has the agent write into its work dir (J8's task), and j8Done the
// reply it ends with.
const (
	j8Answer = "391"
	j8Done   = "J8-DONE"
)

// scriptedRequest is one request the scripted model service received.
type scriptedRequest struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

// scriptedAnthropic is a model service speaking the Anthropic Messages API as a scripted model: asked
// with the agent's tools, it first calls the Bash tool to write j8Answer into answer.txt of the agent's
// working directory, and once the tool's result is back it answers j8Done; a request without the Bash
// tool (a side request of the agent's own) is answered "ok". It answers in the stream format when the
// request asks for it, and records each request.
type scriptedAnthropic struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []scriptedRequest
}

func newScriptedAnthropic(t testing.TB) *scriptedAnthropic {
	s := &scriptedAnthropic{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *scriptedAnthropic) requests() []scriptedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scriptedRequest(nil), s.reqs...)
}

func (s *scriptedAnthropic) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.reqs = append(s.reqs, scriptedRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/count_tokens"):
		_, _ = io.WriteString(w, `{"input_tokens":10}`)
		return
	case r.Method == "GET":
		_, _ = io.WriteString(w, `{"data":[],"has_more":false,"first_id":null,"last_id":null}`)
		return
	case !strings.HasSuffix(r.URL.Path, "/v1/messages"):
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"not found"}}`)
		return
	}
	model, _ := body["model"].(string)
	var block map[string]any
	stop := "end_turn"
	switch {
	case scriptedHasToolResult(body):
		block = map[string]any{"type": "text", "text": j8Done}
	case scriptedHasTool(body, "Bash"):
		block = map[string]any{"type": "tool_use", "id": "toolu_j8", "name": "Bash",
			"input": map[string]any{"command": "printf " + j8Answer + " > answer.txt", "description": "Write the answer"}}
		stop = "tool_use"
	default:
		block = map[string]any{"type": "text", "text": "ok"}
	}
	usage := map[string]any{"input_tokens": 10, "output_tokens": 5}
	if stream, _ := body["stream"].(bool); !stream {
		b, _ := json.Marshal(map[string]any{"id": "msg_j8", "type": "message", "role": "assistant", "model": model,
			"content": []any{block}, "stop_reason": stop, "stop_sequence": nil, "usage": usage})
		_, _ = w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	event := func(name string, data map[string]any) {
		data["type"] = name
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	event("message_start", map[string]any{"message": map[string]any{"id": "msg_j8", "type": "message", "role": "assistant",
		"model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 1}}})
	if block["type"] == "tool_use" {
		input, _ := json.Marshal(block["input"])
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "tool_use",
			"id": block["id"], "name": block["name"], "input": map[string]any{}}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "input_json_delta",
			"partial_json": string(input)}})
	} else {
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": block["text"]}})
	}
	event("content_block_stop", map[string]any{"index": 0})
	event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 5}})
	event("message_stop", map[string]any{})
}

// scriptedHasTool reports a request offering the tool named name.
func scriptedHasTool(body map[string]any, name string) bool {
	tools, _ := body["tools"].([]any)
	for _, tl := range tools {
		if m, ok := tl.(map[string]any); ok && m["name"] == name {
			return true
		}
	}
	return false
}

// scriptedHasToolResult reports a request whose messages carry a tool's result.
func scriptedHasToolResult(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		content, _ := mm["content"].([]any)
		for _, c := range content {
			if cm, ok := c.(map[string]any); ok && cm["type"] == "tool_result" {
				return true
			}
		}
	}
	return false
}

// AV-4, MS-2: a Python agent (hermes, pip-installed into a virtual environment whose python is a link
// into a separately installed Python) gets a profile that lets it read and execute both the environment
// and the installation's prefix — by their real paths, which are what Seatbelt matches — so it starts
// with its packages and its standard library; nothing above them is opened.
func TestSeatbeltLaunchShowsAPythonEnvironment(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	venv := filepath.Join(root, "venvs", "hermes")
	pyhome := filepath.Join(root, "pythons", "cpython-3.12.15")
	for _, d := range []string{filepath.Join(venv, "bin"), filepath.Join(venv, "lib", "python3.12", "site-packages"),
		filepath.Join(pyhome, "bin"), filepath.Join(pyhome, "lib", "python3.12")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pyhome, "bin", "python3.12"), []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = "+filepath.Join(pyhome, "bin")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "pythons", "cpython-3.12"), filepath.Join(venv, "bin", "python")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("cpython-3.12.15/bin/python3.12", filepath.Join(root, "pythons", "cpython-3.12")); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(venv, "bin", "hermes")
	if err := os.WriteFile(script, []byte("#!"+filepath.Join(venv, "bin", "python")+"\nimport sys\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakeModelService(t)
	up := testUpstream(t, egressUpstreamCustom, f)
	plan := &sandboxPlan{SandboxExec: "/usr/bin/sandbox-exec", AID: "EaidSeatbeltPython01", Egress: up,
		NeverBind: []string{filepath.Join(t.TempDir(), "never")}}
	l, err := seatbeltCommand(plan, t.TempDir(), []string{script}, []string{"PATH=/usr/bin:/bin"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	runs := plan.liveRunDirs()
	if len(runs) != 1 {
		t.Fatalf("run directories %v", runs)
	}
	run, _ := filepath.EvalSymlinks(runs[0])
	raw, err := os.ReadFile(filepath.Join(run, seatbeltProfileName))
	if err != nil {
		t.Fatal(err)
	}
	forms := mustParseSBPL(t, string(raw))
	allowed := func(op string) []string {
		var out []string
		for _, fm := range forms {
			if fm.list[0].atom == "allow" && fm.list[1].atom == op {
				for _, x := range fm.list[2:] {
					out = append(out, x.String())
				}
			}
		}
		return out
	}
	for _, op := range []string{"file-read*", "process-exec"} {
		got := allowed(op)
		for _, want := range []string{venv, pyhome} {
			if !slices.Contains(got, "(subpath "+strconv.Quote(want)+")") {
				t.Errorf("%s does not open %s: %v", op, want, got)
			}
		}
		for _, above := range []string{root, filepath.Join(root, "venvs"), filepath.Join(root, "pythons")} {
			if slices.Contains(got, "(subpath "+strconv.Quote(above)+")") {
				t.Errorf("%s opens %s", op, above)
			}
		}
	}
}
