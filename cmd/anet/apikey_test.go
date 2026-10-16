package main

// LEC AV-8 (EXE-5): `anet autoreply set --api-key K` is refused before anything else happens — exit 2,
// api_key_on_argv, the key not repeated, the daemon not contacted, nothing written; --api-key-stdin
// reads the key from stdin and sends only what set names (the daemon merges it).

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

const cliKeyCanary = "sk-CANARY-5d0e9b"

// UX-T11 for api_key_on_argv (M-AV-4): every way of putting the key on the command line exits 2 with
// the sentence alone on stderr, without the key, before the daemon is contacted or a file changes.
func TestAPIKeyOnTheCommandLineIsRefusedBeforeTheDaemon(t *testing.T) {
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		for _, args := range [][]string{
			{"autoreply", "set", "--api-key", cliKeyCanary},
			{"autoreply", "set", "--backend", "exec", "--agent", "claude", "--api-key=" + cliKeyCanary},
			{"autoreply", "set", "--api-key="},
			{"autoreply", "set", "--api-key"},
			{"auto-reply", "set", "--api-key", cliKeyCanary},
			{"autoreply", "set", "--api-key-stdin=" + cliKeyCanary},
			{"autoreply", "set", "--api-key-stdin", cliKeyCanary},
			{"autoreply", "show", "--api-key", cliKeyCanary},
		} {
			t.Run(lang+"/"+strings.Join(args, "_"), func(t *testing.T) {
				h := newCLIHome(t, lang)
				fc := newFakeControl(t, h)
				before := snapshotDir(t, h.root)
				stdout, stderr, code := runCLI(t, h, args)
				l := a2ashape.LangFrom(func(k string) string { return map[string]string{"LANG": lang}[k] })
				want := a2ashape.Sentence("api_key_on_argv", l, nil)
				if code != 2 || stdout != "" || stderr != want+"\n" {
					t.Errorf("exit %d, stdout %q, stderr %q; want 2 and %q", code, stdout, stderr, want)
				}
				if strings.Contains(stdout+stderr, cliKeyCanary) {
					t.Error("the output repeats the key")
				}
				if !strings.Contains(want, "anet autoreply set --api-key-stdin") {
					t.Errorf("the sentence does not name the way to give a key: %q", want)
				}
				if n := fc.conns.Load(); n != 0 {
					t.Errorf("the CLI contacted the control plane %d times", n)
				}
				if !sameSnapshot(before, snapshotDir(t, h.root)) {
					t.Error("the CLI changed files")
				}
			})
		}
	}
}

// What refuseAPIKeyOnArgv counts as a key on the command line, and what it does not.
func TestRefuseAPIKeyOnArgvMatching(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		rest []string
		want bool
	}{
		{"autoreply", []string{"set", "--api-key", "k"}, true},
		{"autoreply", []string{"set", "--api-key=k"}, true},
		{"autoreply", []string{"set", "--api-key"}, true},
		{"autoreply", []string{"set", "--api-key-stdin=k"}, true},
		{"autoreply", []string{"set", "--api-key-stdin", "k"}, true},
		{"autoreply", []string{"set", "--api-key-stdin"}, false},
		{"autoreply", []string{"set", "--api-key-stdin", "--agent", "claude"}, false},
		{"autoreply", []string{"set", "--api-key-stdin="}, false},
		{"autoreply", []string{"set", "--api-key-stdin", "-"}, false},
		{"autoreply", []string{"set", "--api-key-stdin=-"}, false},
		{"autoreply", []string{"set", "--api-key-stdin", "-", "--api-key", "k"}, true},
		{"autoreply", []string{"set", "--agent", "claude", "--", "--api-key", "k"}, false},
		{"autoreply", []string{"set", "--api-base", "https://x", "--model", "m"}, false},
		{"delegate", []string{"aid", "--api-key", "k"}, false},
	} {
		s, got := refuseAPIKeyOnArgv(c.cmd, c.rest, a2ashape.English)
		if got != c.want || (got && s != a2ashape.Sentence("api_key_on_argv", a2ashape.English, nil)) {
			t.Errorf("%s %v: %v %q, want %v", c.cmd, c.rest, got, s, c.want)
		}
	}
}

// --api-key-stdin: the key read from stdin (surrounding white space dropped) is sent in the body and
// nowhere else, with only the members set named; nothing prints the key.
// "-" after the flag, the usual name of standard input, reads it the same way.
func TestAPIKeyStdinSendsTheKeyInTheBody(t *testing.T) {
	needModule(t, "autoreply")
	for _, flag := range [][]string{{"--api-key-stdin"}, {"--api-key-stdin", "-"}, {"--api-key-stdin=-"}} {
		h := newCLIHome(t, "C")
		rc := newRecordingControl(t, h)
		stdout, stderr, code := runCLIStdin(t, h, "  "+cliKeyCanary+"\n\n", append([]string{"autoreply", "set"}, flag...))
		if code != 0 {
			t.Fatalf("%v: exit %d: %s %s", flag, code, stdout, stderr)
		}
		if strings.Contains(stdout+stderr, cliKeyCanary) {
			t.Errorf("%v: the output repeats the key: %q %q", flag, stdout, stderr)
		}
		bodies := rc.bodies()
		if len(bodies) != 1 {
			t.Fatalf("%v: %d requests", flag, len(bodies))
		}
		var body map[string]any
		if err := json.Unmarshal(bodies[0], &body); err != nil || len(body) != 1 || body["api_key"] != cliKeyCanary {
			t.Fatalf("%v: body %s, want only the trimmed key", flag, bodies[0])
		}
	}
}

// `set` sends only the settings it names: the daemon merges them into what is configured (LEC §2.3.4).
// --agent names the exec backend; --api-base alone names none.
func TestAutoReplySetSendsOnlyWhatItNames(t *testing.T) {
	needModule(t, "autoreply")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--model", "haiku"}, `{"model":"haiku"}`},
		{[]string{"--agent", "claude", "--untrusted", "sandbox"}, `{"agent":"claude","backend":"exec","untrusted":"sandbox"}`},
		{[]string{"--api-base", "https://models.example/v1"}, `{"api_base":"https://models.example/v1"}`},
		{[]string{"--backend", "openai", "--api-base", "http://127.0.0.1:1/v1", "--model", "m"},
			`{"api_base":"http://127.0.0.1:1/v1","backend":"openai","model":"m"}`},
		// A string is cleared with "".
		{[]string{"--system-prompt", ""}, `{"system_prompt":""}`},
	} {
		h := newCLIHome(t, "C")
		rc := newRecordingControl(t, h)
		if _, stderr, code := runCLIStdin(t, h, "", append([]string{"autoreply", "set"}, c.args...)); code != 0 {
			t.Fatalf("%v: exit %d %s", c.args, code, stderr)
		}
		b := rc.bodies()
		if len(b) != 1 || string(bytes.TrimSpace(b[0])) != c.want {
			t.Errorf("%v: sent %q, want %s", c.args, b, c.want)
		}
	}
	// --require-image is removed (ux §4.3): cli_removed, exit 2, and nothing is sent.
	for _, flag := range []string{"--require-image", "--require-image=false"} {
		h := newCLIHome(t, "C")
		rc := newRecordingControl(t, h)
		if _, stderr, code := runCLIStdin(t, h, "", []string{"autoreply", "set", flag}); code != 2 || len(rc.bodies()) != 0 ||
			!strings.Contains(stderr, "require-image") {
			t.Fatalf("%s: exit %d, %d requests: %s", flag, code, len(rc.bodies()), stderr)
		}
	}
}

// An empty key is api_key_empty, a key over 4096 bytes config_invalid: exit 1, the sentence, nothing
// sent.
func TestAPIKeyStdinRefusesEmptyAndOversizedInput(t *testing.T) {
	needModule(t, "autoreply")
	for _, c := range []struct {
		in   string
		code string
	}{
		{"", "api_key_empty"},
		{" \n\t\n", "api_key_empty"},
		{strings.Repeat("k", 4097), "config_invalid"},
	} {
		h := newCLIHome(t, "C")
		rc := newRecordingControl(t, h)
		stdout, stderr, code := runCLIStdin(t, h, c.in, []string{"autoreply", "set", "--api-key-stdin"})
		want := a2ashape.Sentence(c.code, a2ashape.English, nil)
		if c.code == "config_invalid" {
			want = "The setting api_key is not valid: "
		}
		if code != 1 || stdout != "" || !strings.HasPrefix(stderr, want) || strings.Contains(stderr, "kkkk") {
			t.Errorf("%d bytes: exit %d, stdout %q, stderr %q; want 1 and %q", len(c.in), code, stdout, stderr, want)
		}
		if n := len(rc.bodies()); n != 0 {
			t.Errorf("%d bytes: %d requests sent", len(c.in), n)
		}
	}
	// 4096 bytes exactly is a key.
	h := newCLIHome(t, "C")
	rc := newRecordingControl(t, h)
	if _, stderr, code := runCLIStdin(t, h, strings.Repeat("k", 4096), []string{"autoreply", "set", "--api-key-stdin"}); code != 0 {
		t.Fatalf("a 4096-byte key: exit %d %s", code, stderr)
	}
	if len(rc.bodies()) != 1 {
		t.Fatal("a 4096-byte key was not sent")
	}
}

// recordingControl is a control plane that answers 200 {} and keeps the request bodies.
type recordingControl struct {
	mu   sync.Mutex
	got  [][]byte
	hits atomic.Int64
}

func (rc *recordingControl) bodies() [][]byte {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([][]byte(nil), rc.got...)
}

func newRecordingControl(t *testing.T, h *cliHome) *recordingControl {
	t.Helper()
	rc := &recordingControl{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/autoreply" {
			rc.mu.Lock()
			rc.got = append(rc.got, b)
			rc.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	cfg := `{"control_addr": "` + ln.Addr().String() + `"}`
	if err := os.WriteFile(filepath.Join(h.data, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.data, "control_token.txt"), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return rc
}

// runCLIStdin is runCLI with stdin from in (a pipe, not a terminal).
func runCLIStdin(t *testing.T, h *cliHome, in string, args []string) (stdout, stderr string, code int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = h.env()
	cmd.Dir = h.root
	cmd.Stdin = strings.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("anet %s did not end", strings.Join(args, " "))
	}
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}
