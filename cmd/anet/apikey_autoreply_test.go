//go:build !no_autoreply

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// --api-key-stdin: the key read from stdin (surrounding white space dropped) is sent in the body and
// nowhere else, with only the members set named; nothing prints the key.
// "-" after the flag, the usual name of standard input, reads it the same way.
func TestAPIKeyStdinSendsTheKeyInTheBody(t *testing.T) {
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
	// So are the 0.2 flags whose values are now fixed: a 0.2-era script hears that, not "unknown flag".
	for _, flag := range []string{"--require-image", "--require-image=false", "--max-auto-replies", "--max-auto-replies=5",
		"--poll-interval", "--poll-interval=10"} {
		h := newCLIHome(t, "C")
		rc := newRecordingControl(t, h)
		name, _, _ := strings.Cut(strings.TrimPrefix(flag, "--"), "=")
		if _, stderr, code := runCLIStdin(t, h, "", []string{"autoreply", "set", flag}); code != 2 || len(rc.bodies()) != 0 ||
			!strings.Contains(stderr, name) || strings.Contains(stderr, "unknown flag") {
			t.Fatalf("%s: exit %d, %d requests: %s", flag, code, len(rc.bodies()), stderr)
		}
	}
}

// An empty key is api_key_empty, a key over 4096 bytes config_invalid: exit 1, the sentence, nothing
// sent.
func TestAPIKeyStdinRefusesEmptyAndOversizedInput(t *testing.T) {
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
