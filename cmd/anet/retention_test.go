package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)

// retentionCtl is a control plane holding content_retention cur, whose dry run answers would; it
// records the /retention bodies it is sent.
func retentionCtl(t *testing.T, cur string, would int) (*client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/status":
			_, _ = w.Write([]byte(`{"privacy":{"content_retention":"` + cur + `","public_retention":"7d"}}`))
		case "/retention":
			mu.Lock()
			bodies = append(bodies, strings.TrimSpace(string(b)))
			mu.Unlock()
			var req map[string]any
			_ = json.Unmarshal(b, &req)
			if req["dry_run"] == true {
				_ = json.NewEncoder(w).Encode(map[string]any{"value": req["value"], "would_delete": would})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": req["value"], "deleted": would,
				"message": "set to " + req["value"].(string)})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// runRetentionCmd runs `anet retention <args>` against layout, with the daemon c (nil: not running),
// and returns its error, stdout and stderr.
func runRetentionCmd(t *testing.T, layout daemon.Layout, c *client, args ...string) (error, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	prevOut, prevErr, prevD, prevLine := retentionStdout, retentionStderr, retentionDaemon, commandLine
	retentionStdout, retentionStderr = &out, &errOut
	retentionDaemon = func(daemon.Layout) *client { return c }
	commandLine = func() []string { return append([]string{"retention"}, args...) }
	defer func() {
		retentionStdout, retentionStderr, retentionDaemon, commandLine = prevOut, prevErr, prevD, prevLine
	}()
	if err := checkFlags("retention", args); err != nil {
		t.Fatalf("flags: %v", err)
	}
	err := runRetention(layout, args)
	return err, out.String(), errOut.String()
}

// stoppedNode is a data directory with no daemon running: a config.json whose content_retention is
// value (left out when ""), and a store holding a non-public task that ended endedDaysAgo days ago.
func stoppedNode(t *testing.T, value string, endedDaysAgo ...int) daemon.Layout {
	t.Helper()
	l := daemon.NewLayout(t.TempDir())
	body := `{"control_addr":"127.0.0.1:0","name":"kept as it is"`
	if value != "" {
		body += `,"content_retention":` + value
	}
	if err := os.WriteFile(l.ConfigPath(), []byte(body+"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := interactions.Open(l.InteractionsDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i, days := range endedDaysAgo {
		id := "ix_" + string(rune('a'+i))
		s.SetClock(func() int64 { return now - int64(days)*24*3600*1000 })
		if err := s.Create(interactions.New{ID: id, Role: interactions.RoleOutbound, PeerAID: "did:anet:peer", Goal: "g"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetState(id, interactions.StateCompleted, interactions.CausePeerEnded); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return l
}

func fileRetention(t *testing.T, l daemon.Layout) string {
	t.Helper()
	_, text, _, err := daemon.ContentRetentionInFile(l)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// TestRetentionSetNeedsTTY (FS §5.2 step 2, mutation 34): without a terminal `anet retention set` says
// tty_required with the command to run there, exits 1 and changes nothing — the running daemon is
// not asked anything, a stopped node's config.json is not written — however harmless the value.
func TestRetentionSetNeedsTTY(t *testing.T) {
	withTTY(t, "")
	for _, value := range []string{"7d", "90d", "forever"} {
		args := a2ashape.Args{"action": "retention", "command": "anet retention set " + value}
		want := a2ashape.Sentence("tty_required", a2ashape.Lang(), args) + "\n" + nextPrefix +
			a2ashape.Next("tty_required", a2ashape.Lang(), args) + "\n"

		c, sent := retentionCtl(t, "30d", 3)
		err, out, errOut := runRetentionCmd(t, daemon.NewLayout(t.TempDir()), c, "set", value)
		if !errors.Is(err, errExit1) || out != "" || errOut != want {
			t.Errorf("%s, daemon running: %v %q %q", value, err, out, errOut)
		}
		if b := sent(); len(b) != 0 {
			t.Errorf("%s: without a terminal the daemon was sent %v", value, b)
		}

		l := stoppedNode(t, `"30d"`, 40)
		err, _, errOut = runRetentionCmd(t, l, nil, "set", value)
		if !errors.Is(err, errExit1) || errOut != want || fileRetention(t, l) != "30d" {
			t.Errorf("%s, daemon stopped: %v %q, config says %q", value, err, errOut, fileRetention(t, l))
		}
	}
}

// TestRetentionShortenAsksWithCount (FS §5.2 step 4, mutation 35): a change that deletes tasks at once
// says how many on the terminal and needs "yes"; anything else changes nothing and says so. A change
// that deletes nothing at once (lengthening, forever) asks nothing.
func TestRetentionShortenAsksWithCount(t *testing.T) {
	lang := a2ashape.Lang()
	prompt := a2ashape.Sentence("retention.confirm", lang, a2ashape.Args{"retention": "7d", "n": 12})

	c, sent := retentionCtl(t, "30d", 12)
	tty := withTTY(t, "no")
	err, out, errOut := runRetentionCmd(t, daemon.NewLayout(t.TempDir()), c, "set", "7d")
	if !errors.Is(err, errExit1) || out != "" || errOut != a2ashape.Sentence("confirm.declined", lang, nil)+"\n" {
		t.Errorf("declined: %v %q %q", err, out, errOut)
	}
	if !strings.Contains(tty.prompt.String(), prompt) {
		t.Errorf("the prompt %q does not say %q", tty.prompt.String(), prompt)
	}
	if b := sent(); len(b) != 1 || b[0] != `{"dry_run":true,"value":"7d"}` {
		t.Errorf("declined: the daemon was sent %v", b)
	}

	c, sent = retentionCtl(t, "30d", 12)
	tty = withTTY(t, "yes")
	err, out, _ = runRetentionCmd(t, daemon.NewLayout(t.TempDir()), c, "set", "7d")
	if err != nil || out != "set to 7d\n" || !strings.Contains(tty.prompt.String(), prompt) {
		t.Errorf("confirmed: %v %q, prompt %q", err, out, tty.prompt.String())
	}
	if b := sent(); len(b) != 2 || b[1] != `{"value":"7d"}` {
		t.Errorf("confirmed: the daemon was sent %v", b)
	}

	for _, value := range []string{"90d", "forever"} {
		c, sent = retentionCtl(t, "30d", 0)
		tty = withTTY(t, "no")
		if err, out, _ := runRetentionCmd(t, daemon.NewLayout(t.TempDir()), c, "set", value); err != nil || out == "" {
			t.Errorf("%s: %v %q", value, err, out)
		}
		if tty.prompt.Len() != 0 {
			t.Errorf("%s: asked %q", value, tty.prompt.String())
		}
		if b := sent(); len(b) != 2 {
			t.Errorf("%s: the daemon was sent %v", value, b)
		}
	}

	// With the daemon stopped the count reads the store: one task of 20 days goes at 7 days.
	l := stoppedNode(t, `"30d"`, 20, 2)
	tty = withTTY(t, "no")
	if err, _, _ := runRetentionCmd(t, l, nil, "set", "7d"); !errors.Is(err, errExit1) {
		t.Fatalf("declined, stopped: %v", err)
	}
	if want := a2ashape.Sentence("retention.confirm", lang, a2ashape.Args{"retention": "7d", "n": 1}); !strings.Contains(tty.prompt.String(), want) {
		t.Errorf("stopped: the prompt %q does not say %q", tty.prompt.String(), want)
	}
	if fileRetention(t, l) != "30d" {
		t.Error("declined, stopped: config.json changed")
	}
}

// TestRetentionOfflineRepair (FS §5.2 step 5): with the daemon stopped the change is written to
// config.json, every other key as it was; this is also how a value the daemon refused to start with is
// repaired. `anet retention` reports such a value with retention_invalid.
func TestRetentionOfflineRepair(t *testing.T) {
	lang := a2ashape.Lang()
	l := stoppedNode(t, `"30"`, 40, 2)
	if err := daemon.CheckConfigForStart(l); err == nil {
		t.Fatal("the daemon would start with content_retention 30")
	}
	invalid := a2ashape.Sentence("retention_invalid", lang, nil) + "\n" + nextPrefix + a2ashape.Next("retention_invalid", lang, nil) + "\n"
	if err, out, errOut := runRetentionCmd(t, l, nil); !errors.Is(err, errExit1) || out != "" || errOut != invalid {
		t.Fatalf("show: %v %q %q", err, out, errOut)
	}
	tty := withTTY(t, "yes")
	err, out, _ := runRetentionCmd(t, l, nil, "set", "30d")
	if err != nil || out != a2ashape.Sentence("retention.set", lang, a2ashape.Args{"retention": "30d"})+"\n" {
		t.Fatalf("repair: %v %q", err, out)
	}
	// The value that was there counts as keeping everything: the 40-day task is counted.
	if want := a2ashape.Sentence("retention.confirm", lang, a2ashape.Args{"retention": "30d", "n": 1}); !strings.Contains(tty.prompt.String(), want) {
		t.Errorf("repair prompt %q", tty.prompt.String())
	}
	if err := daemon.CheckConfigForStart(l); err != nil {
		t.Fatalf("after the repair: %v", err)
	}
	raw, _ := os.ReadFile(l.ConfigPath())
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m["content_retention"] != "30d" || m["name"] != "kept as it is" {
		t.Fatalf("config after the repair: %s", raw)
	}
	// An invalid value is refused before anything is asked or written.
	withTTY(t, "yes")
	if err, _, errOut := runRetentionCmd(t, l, nil, "set", "0d"); !errors.Is(err, errExit1) || errOut != invalid || fileRetention(t, l) != "30d" {
		t.Fatalf("set 0d: %v %q", err, errOut)
	}
}

// `anet retention` says the setting in force, with the usage line, or as JSON: the running daemon's,
// else config.json's (30 days without the key).
func TestRetentionShow(t *testing.T) {
	lang := a2ashape.Lang()
	want := func(r string) string {
		return a2ashape.Sentence("retention.summary", lang, a2ashape.Args{"retention": r}) + "\nusage: " + retentionUsage + "\n"
	}
	if err, out, _ := runRetentionCmd(t, stoppedNode(t, ""), nil); err != nil || out != want("30d") {
		t.Errorf("no key: %v %q", err, out)
	}
	if err, out, _ := runRetentionCmd(t, stoppedNode(t, `"forever"`), nil, "--json"); err != nil ||
		out != `{"content_retention":"forever","public_retention":"7d"}`+"\n" {
		t.Errorf("--json: %v %q", err, out)
	}
	if err, out, _ := runRetentionCmd(t, stoppedNode(t, `"3d"`), nil, "--json"); err != nil ||
		out != `{"content_retention":"3d","public_retention":"3d"}`+"\n" {
		t.Errorf("--json 3d: %v %q", err, out)
	}
	c, _ := retentionCtl(t, "90d", 0)
	if err, out, _ := runRetentionCmd(t, stoppedNode(t, `"30d"`), c); err != nil || out != want("90d") {
		t.Errorf("running: %v %q", err, out)
	}
	var ue usageError
	for _, args := range [][]string{{"set"}, {"set", "7d", "8d"}, {"get"}, {"set", "7d", "--json"}} {
		if err, _, _ := runRetentionCmd(t, stoppedNode(t, ""), nil, args...); !errors.As(err, &ue) {
			t.Errorf("%v: %v, want the usage line", args, err)
		}
	}
}
