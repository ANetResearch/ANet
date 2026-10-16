package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// egressDaemon is a control plane whose auto-reply allows current, and that records what it is sent.
func egressDaemon(t *testing.T, current []string) (*client, func() (paths []string, bodies []string)) {
	t.Helper()
	var mu sync.Mutex
	var paths, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, strings.TrimSpace(string(b)))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/status" {
			cur, _ := json.Marshal(current)
			_, _ = w.Write([]byte(`{"auto_reply":{"backend":"exec","agent":"claude","untrusted":"sandbox","sandbox":{"egress_allow":` + string(cur) + `}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"updated"}`))
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}, func() ([]string, []string) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...), append([]string(nil), bodies...)
	}
}

// runEgress runs `anet autoreply set <args>` as typed, and returns its error and what it said on stderr.
func runEgress(t *testing.T, c *client, args ...string) (error, string) {
	t.Helper()
	var stderr bytes.Buffer
	prevErr, prevTTYErr, prevLine := egressStderr, ttyStderr, commandLine
	egressStderr, ttyStderr = &stderr, &stderr
	commandLine = func() []string { return append([]string{"autoreply", "set"}, args...) }
	defer func() { egressStderr, ttyStderr, commandLine = prevErr, prevTTYErr, prevLine }()
	if err := checkFlags("autoreply", append([]string{"set"}, args...)); err != nil {
		t.Fatalf("flags: %v", err)
	}
	err := runAutoReply(c, append([]string{"set"}, args...))
	return err, stderr.String()
}

// CN-8, UX-C12: adding a host to the allowlist is confirmed on a terminal. Without one the command says
// tty_required with the command to run there, exits 1 and changes nothing (the daemon is only read);
// with "yes" the list is sent, and the prompt names the hosts added; without "yes" nothing is sent.
func TestEgressAllowNeedsATerminalToAdd(t *testing.T) {
	c, sent := egressDaemon(t, []string{"pypi.org"})
	withTTY(t, "")
	err, out := runEgress(t, c, "--egress-allow", "pypi.org,files.pythonhosted.org")
	args := a2ashape.Args{"action": "egress", "command": "anet autoreply set --egress-allow pypi.org,files.pythonhosted.org"}
	want := a2ashape.Sentence("tty_required", a2ashape.Lang(), args) + "\n" + nextPrefix +
		a2ashape.Next("tty_required", a2ashape.Lang(), args) + "\n"
	if !errors.Is(err, errExit1) || out != want {
		t.Errorf("without a terminal: %v\n%q\nwant\n%q", err, out, want)
	}
	if paths, _ := sent(); strings.Join(paths, " ") != "/status" {
		t.Errorf("without a terminal the daemon was asked %v", paths)
	}

	c, sent = egressDaemon(t, []string{"pypi.org"})
	withTTY(t, "no")
	if err, _ := runEgress(t, c, "--egress-allow", "pypi.org,files.pythonhosted.org"); err == nil {
		t.Error("declined: accepted")
	}
	if paths, _ := sent(); strings.Join(paths, " ") != "/status" {
		t.Errorf("declined: the daemon was asked %v", paths)
	}

	c, sent = egressDaemon(t, []string{"pypi.org"})
	tty := withTTY(t, "yes")
	if err, out := runEgress(t, c, "--egress-allow", "PyPI.org, files.pythonhosted.org"); err != nil {
		t.Fatalf("confirmed: %v %s", err, out)
	}
	paths, bodies := sent()
	if strings.Join(paths, " ") != "/status /autoreply" || bodies[1] != `{"sandbox":{"egress_allow":["pypi.org","files.pythonhosted.org"]}}` {
		t.Errorf("confirmed: %v %v", paths, bodies)
	}
	if want := a2ashape.Sentence("egress_confirm", a2ashape.Lang(), a2ashape.Args{"hosts": "files.pythonhosted.org"}); !strings.Contains(tty.prompt.String(), want) {
		t.Errorf("prompt %q lacks %q", tty.prompt.String(), want)
	}
}

// Removing hosts, or clearing the list, loosens nothing and needs no terminal.
func TestEgressAllowRemovesWithoutATerminal(t *testing.T) {
	for _, tc := range []struct{ value, body string }{
		{"pypi.org", `{"sandbox":{"egress_allow":["pypi.org"]}}`},
		{"", `{"sandbox":{"egress_allow":[]}}`},
		{" , ", `{"sandbox":{"egress_allow":[]}}`},
	} {
		c, sent := egressDaemon(t, []string{"pypi.org", "files.pythonhosted.org"})
		withTTY(t, "")
		if err, out := runEgress(t, c, "--egress-allow", tc.value); err != nil {
			t.Errorf("%q: %v %s", tc.value, err, out)
		}
		if _, bodies := sent(); len(bodies) == 0 || bodies[len(bodies)-1] != tc.body {
			t.Errorf("%q: sent %v", tc.value, bodies)
		}
	}
}

// CN-1 before anything else: a list the daemon would refuse is refused here with its sentence, before the
// daemon is asked anything or anyone is asked to confirm; the flag without a value is a usage error.
func TestEgressAllowIsCheckedFirst(t *testing.T) {
	for _, tc := range []struct{ value, key string }{
		{"10.0.0.1", "egress_allow_invalid"},
		{"pypi.org,*.github.com", "egress_allow_invalid"},
		{"pypi.org:443", "egress_allow_invalid"},
		{"api.anthropic.com", "egress_allow_model_host"},
	} {
		c, sent := egressDaemon(t, nil)
		tty := withTTY(t, "yes")
		err, out := runEgress(t, c, "--egress-allow", tc.value)
		_, ref := daemon.CheckEgressAllow(strings.Split(tc.value, ","), "")
		if !errors.Is(err, errExit1) || ref == nil || ref.Key != tc.key || out != a2ashape.Sentence(ref.Key, a2ashape.Lang(), ref.Args)+"\n" {
			t.Errorf("%s: %v %q", tc.value, err, out)
		}
		if paths, _ := sent(); len(paths) != 0 || tty.prompt.Len() != 0 {
			t.Errorf("%s: asked %v, prompt %q", tc.value, paths, tty.prompt.String())
		}
	}
	c, _ := egressDaemon(t, nil)
	var ue usageError
	if err, _ := runEgress(t, c, "--egress-allow"); !errors.As(err, &ue) {
		t.Errorf("no value: %v", err)
	}
}

// The flag goes with a whole setup too: the allowlist rides in the same request.
func TestEgressAllowWithASetup(t *testing.T) {
	c, sent := egressDaemon(t, nil)
	withTTY(t, "yes")
	if err, out := runEgress(t, c, "--backend", "exec", "--agent", "claude", "--egress-allow", "pypi.org"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	_, bodies := sent()
	var body map[string]any
	if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &body); err != nil || body["backend"] != "exec" || body["agent"] != "claude" ||
		strings.TrimSpace(mustJSON(body["sandbox"])) != `{"egress_allow":["pypi.org"]}` {
		t.Errorf("sent %v", bodies)
	}
}

// tty_required's {command} is the command typed, quoted where a shell needs it, without a key's value.
func TestTypedCommand(t *testing.T) {
	for in, want := range map[string]string{
		"autoreply set --egress-allow pypi.org":             "anet autoreply set --egress-allow pypi.org",
		"autoreply set --egress-allow a.org,b.org":          "anet autoreply set --egress-allow a.org,b.org",
		"autoreply set --api-key sk-1 --egress-allow x.org": "anet autoreply set --api-key … --egress-allow x.org",
		"autoreply set --api-key=sk-1 --egress-allow x.org": "anet autoreply set --api-key=… --egress-allow x.org",
	} {
		if got := typedCommand(strings.Fields(in)); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	if got := typedCommand([]string{"autoreply", "set", "--egress-allow", "a.org, it's"}); got != `anet autoreply set --egress-allow 'a.org, it'\''s'` {
		t.Errorf("quoting: %s", got)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
