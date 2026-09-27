package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTTY is a terminal whose user types answer.
type fakeTTY struct {
	r      io.Reader
	prompt strings.Builder
}

func (f *fakeTTY) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *fakeTTY) Write(p []byte) (int, error) { return f.prompt.Write(p) }
func (f *fakeTTY) Close() error                { return nil }

// withTTY makes confirmOnTTY see a terminal answering answer, or no terminal
// when answer is "".
func withTTY(t *testing.T, answer string) *fakeTTY {
	t.Helper()
	prev := openTTY
	t.Cleanup(func() { openTTY = prev })
	if answer == "" {
		openTTY = func() (io.ReadWriteCloser, error) { return nil, errors.New("no tty") }
		return nil
	}
	tty := &fakeTTY{r: strings.NewReader(answer + "\n")}
	openTTY = func() (io.ReadWriteCloser, error) { return tty, nil }
	return tty
}

// recordingDaemon is a control plane that records the paths it was asked.
// It holds one delegation, ix_1, for approval.
func recordingDaemon(t *testing.T) (*client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","pending":[{"interaction_id":"ix_1","requester":"bafyreipeer000000000","bytes":12}],` +
			`"payments":{"auto_max":0,"agent_max":0}}`))
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// Granting commands — peers allow and trust, inbound approve, loosening the
// policy, changing a spending limit — need a confirmation typed on a
// terminal (A2A-DESIGN §5.3, §8.6). With no terminal nothing reaches the
// daemon at all; without "yes" nothing that changes anything does; the other
// commands need none.
func TestGrantingCommandsNeedATerminal(t *testing.T) {
	grants := [][]string{
		{"peers", "allow", "bafyreipeer000000000"},
		{"peers", "trust", "bafyreipeer000000000"},
		{"inbound", "approve", "ix_1"},
		{"inbound", "policy", "open"},
		{"inbound", "policy", "approve"},
		{"payments", "limits", "--auto-max", "5"},
		{"payments", "limits", "--daily-max", "0"},
	}
	// What a prompt may read before asking.
	readOnly := map[string]bool{"/inbound/pending": true, "/find": true, "/payments/limits": true}
	for _, args := range grants {
		c, sent := recordingDaemon(t)
		withTTY(t, "")
		err := runClientArgs(c, args)
		if !errors.Is(err, errNoTTY) {
			t.Errorf("%v without a terminal: %v, want the TTY refusal", args, err)
		}
		if got := sent(); len(got) != 0 {
			t.Errorf("%v without a terminal reached the daemon at %v", args, got)
		}
		c, sent = recordingDaemon(t)
		withTTY(t, "no")
		if err := runClientArgs(c, args); err == nil {
			t.Errorf("%v answered no: accepted", args)
		}
		for _, p := range sent() {
			if !readOnly[p] {
				t.Errorf("%v answered no reached the daemon at %s", args, p)
			}
		}
		c, sent = recordingDaemon(t)
		tty := withTTY(t, "yes")
		if err := runClientArgs(c, args); err != nil {
			t.Errorf("%v confirmed: %v", args, err)
		}
		if got := sent(); len(got) < 2 && args[0] == "payments" || len(got) == 0 ||
			!strings.HasPrefix(got[len(got)-1], "/"+args[0]+"/") {
			t.Errorf("%v confirmed: daemon asked %v", args, got)
		}
		if !strings.Contains(tty.prompt.String(), "Type yes") {
			t.Errorf("%v: no prompt on the terminal", args)
		}
	}
	for _, args := range [][]string{
		{"peers", "list"}, {"peers", "deny", "bafyreipeer000000000"}, {"peers", "remove", "bafyreipeer000000000"},
		{"inbound", "pending"}, {"inbound", "list"}, {"inbound", "reject", "ix_1"}, {"inbound", "policy", "closed"},
		{"inbound", "policy"}, {"payments", "limits"},
	} {
		c, sent := recordingDaemon(t)
		withTTY(t, "")
		if err := runClientArgs(c, args); err != nil || len(sent()) != 1 {
			t.Errorf("%v: %v, daemon asked %v", args, err, sent())
		}
	}
}

// `anet accept on` and `hub-register --accept-delegations true` are refused
// with the three policies and the allow-list command, and nothing reaches
// the daemon; `accept off` asks the daemon for closed.
func TestAcceptOnIsRefusedByTheCLI(t *testing.T) {
	for _, args := range [][]string{{"accept", "on"}, {"accept"}, {"hub-register", "http://hub", "--accept-delegations", "true"}} {
		c, sent := recordingDaemon(t)
		err := runClientArgs(c, args)
		if err == nil || !strings.Contains(err.Error(), "anet peers allow") || !strings.Contains(err.Error(), "approve") {
			t.Errorf("%v: %v", args, err)
		}
		if len(sent()) != 0 {
			t.Errorf("%v reached the daemon: %v", args, sent())
		}
	}
	c, sent := recordingDaemon(t)
	if err := runClientArgs(c, []string{"accept", "off"}); err != nil || strings.Join(sent(), ",") != "/accept" {
		t.Fatalf("accept off: %v, %v", err, sent())
	}
}

// runClientArgs dispatches the commands this file tests, the way runClient
// does, against a given client.
func runClientArgs(c *client, args []string) error {
	switch args[0] {
	case "peers":
		return runPeers(c, args[1:])
	case "inbound":
		return runInbound(c, args[1:])
	case "accept":
		return runAccept(c, args[1:])
	case "hub-register":
		return runHubRegister(c, args[1:])
	case "payments":
		return runPayments(c, args[1:])
	}
	panic("runClientArgs: " + args[0])
}

// The prompt names what is being granted: the held delegation's sender and
// size, each limit as current → new. Approving an id the daemon does not
// hold ends without asking and without calling approve.
func TestConfirmationsDescribeWhatTheyGrant(t *testing.T) {
	c, _ := recordingDaemon(t)
	tty := withTTY(t, "yes")
	if err := runClientArgs(c, []string{"inbound", "approve", "ix_1"}); err != nil {
		t.Fatal(err)
	}
	if p := tty.prompt.String(); !strings.Contains(p, "bafyreipeer000000000") || !strings.Contains(p, "12 bytes") {
		t.Errorf("approve prompt: %q", p)
	}
	c, sent := recordingDaemon(t)
	tty = withTTY(t, "yes")
	if err := runClientArgs(c, []string{"inbound", "approve", "ix_unknown"}); err == nil ||
		!strings.Contains(err.Error(), "no held delegation") {
		t.Errorf("approve of an unknown id: %v", err)
	}
	if strings.Contains(strings.Join(sent(), ","), "/inbound/approve") || tty.prompt.Len() != 0 {
		t.Errorf("unknown id: asked %q, daemon %v", tty.prompt.String(), sent())
	}
	c, _ = recordingDaemon(t)
	tty = withTTY(t, "yes")
	if err := runClientArgs(c, []string{"payments", "limits", "--agent-max", "7"}); err != nil {
		t.Fatal(err)
	}
	if p := tty.prompt.String(); !strings.Contains(p, "agent_max") || !strings.Contains(p, "0 → 7") {
		t.Errorf("limits prompt: %q", p)
	}
	if err := runClientArgs(c, []string{"payments", "limits", "--agent-max", "-1"}); err == nil {
		t.Error("a negative limit was accepted")
	}
}

// Text from elsewhere cannot rewrite a prompt: control and bidi characters
// are dropped and length is bounded.
func TestPrintableStripsTerminalControl(t *testing.T) {
	got := printable("evil\x1b[2J\rname\u202etxt\n", 100)
	if got != "evil[2Jnametxt" {
		t.Fatalf("printable = %q", got)
	}
	if got := printable(strings.Repeat("a", 10), 4); got != "aaaa…" {
		t.Fatalf("cut = %q", got)
	}
}
