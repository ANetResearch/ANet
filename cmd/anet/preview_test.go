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
)

// previewDaemon is a control plane holding stream_preview cur that records the /preview bodies it is sent.
func previewDaemon(t *testing.T, cur string) (*client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/preview" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		bodies = append(bodies, strings.TrimSpace(string(b)))
		mu.Unlock()
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		if m, ok := req["mode"].(string); ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": m, "from": cur})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"mode": cur})
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func runPreviewCmd(t *testing.T, c *client, args ...string) (error, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	prevOut, prevErr, prevLine := cliStdout, cliStderr, commandLine
	cliStdout, cliStderr = &out, &errOut
	commandLine = func() []string { return append([]string{"preview"}, args...) }
	defer func() { cliStdout, cliStderr, commandLine = prevOut, prevErr, prevLine }()
	if err := checkFlags("preview", args); err != nil {
		t.Fatalf("flags: %v", err)
	}
	err := runPreview(c, args)
	return err, out.String(), errOut.String()
}

// Without an argument the command says what the setting is, in the table's sentence; nothing is changed.
func TestPreviewShowsTheSetting(t *testing.T) {
	for _, mode := range []string{"off", "on", "with_reasoning"} {
		c, bodies := previewDaemon(t, mode)
		err, out, _ := runPreviewCmd(t, c)
		want := a2ashape.Sentence("preview."+mode, a2ashape.Lang(), nil) + "\n"
		if err != nil || out != want {
			t.Errorf("%s: %v %q, want %q", mode, err, out, want)
		}
		if b := bodies(); len(b) != 1 || b[0] != "{}" {
			t.Errorf("%s: sent %q", mode, b)
		}
	}
	c, _ := previewDaemon(t, "on")
	if _, out, _ := runPreviewCmd(t, c, "--json"); !strings.Contains(out, `"mode":"on"`) {
		t.Errorf("--json: %q", out)
	}
}

// on and off need no terminal; with_reasoning needs one, and without it nothing is sent
// (tty_required), and a "no" changes nothing (confirm.declined).
func TestPreviewReasoningNeedsATerminal(t *testing.T) {
	withTTY(t, "")
	c, bodies := previewDaemon(t, "off")
	for _, m := range []string{"on", "off"} {
		if err, _, _ := runPreviewCmd(t, c, m); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if b := bodies(); len(b) != 2 || b[0] != `{"mode":"on"}` || b[1] != `{"mode":"off"}` {
		t.Fatalf("sent %q", b)
	}
	var tty bytes.Buffer
	prev := ttyStderr
	ttyStderr = &tty
	defer func() { ttyStderr = prev }()
	err, out, _ := runPreviewCmd(t, c, "with_reasoning")
	if !errors.Is(err, errExit1) || out != "" || len(bodies()) != 2 {
		t.Fatalf("without a terminal: %v %q, %d requests", err, out, len(bodies()))
	}
	if !strings.Contains(tty.String(), "preview with_reasoning") && !strings.Contains(tty.String(), "with_reasoning") {
		t.Errorf("tty_required does not name the command: %q", tty.String())
	}

	withTTY(t, "no")
	if err, _, _ := runPreviewCmd(t, c, "with_reasoning"); !errors.Is(err, errExit1) || len(bodies()) != 2 {
		t.Fatalf("declined: %v, %d requests", err, len(bodies()))
	}
	tt := withTTY(t, "yes")
	err, out, _ = runPreviewCmd(t, c, "with_reasoning")
	if err != nil || len(bodies()) != 3 || bodies()[2] != `{"mode":"with_reasoning"}` {
		t.Fatalf("confirmed: %v, %q", err, bodies())
	}
	if !strings.Contains(tt.prompt.String(), a2ashape.Sentence("preview.confirm_reasoning", a2ashape.Lang(), nil)) {
		t.Errorf("the confirmation did not say what it does: %q", tt.prompt.String())
	}
	if !strings.Contains(out, a2ashape.Sentence("preview.with_reasoning", a2ashape.Lang(), nil)) {
		t.Errorf("output %q", out)
	}
}

// A value that is no setting, or too many arguments, is a usage error, and nothing is sent.
func TestPreviewUsage(t *testing.T) {
	c, bodies := previewDaemon(t, "off")
	for _, args := range [][]string{{"loud"}, {"on", "off"}, {"ON"}} {
		var ue usageError
		if err, _, _ := runPreviewCmd(t, c, args...); !errors.As(err, &ue) {
			t.Errorf("%v: %v", args, err)
		}
	}
	if len(bodies()) != 0 {
		t.Fatalf("sent %q", bodies())
	}
}
