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
func recordingDaemon(t *testing.T) (*client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","pending":[]}`))
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "t", timeout: 5 * time.Second}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// Granting commands — peers allow and trust, inbound approve, loosening the
// policy — need a confirmation typed on a terminal (A2A-DESIGN §5.3). With
// no terminal, or without "yes", nothing reaches the daemon; the other
// commands need none.
func TestGrantingCommandsNeedATerminal(t *testing.T) {
	grants := [][]string{
		{"peers", "allow", "bafyreipeer000000000"},
		{"peers", "trust", "bafyreipeer000000000"},
		{"inbound", "approve", "ix_1"},
		{"inbound", "policy", "open"},
		{"inbound", "policy", "approve"},
	}
	for _, args := range grants {
		c, sent := recordingDaemon(t)
		withTTY(t, "")
		err := runClientArgs(c, args)
		if !errors.Is(err, errNoTTY) {
			t.Errorf("%v without a terminal: %v, want the TTY refusal", args, err)
		}
		for _, p := range sent() {
			if p != "/inbound/pending" {
				t.Errorf("%v without a terminal reached the daemon at %s", args, p)
			}
		}
		c, sent = recordingDaemon(t)
		withTTY(t, "no")
		if err := runClientArgs(c, args); err == nil {
			t.Errorf("%v answered no: accepted", args)
		}
		for _, p := range sent() {
			if p != "/inbound/pending" {
				t.Errorf("%v answered no reached the daemon at %s", args, p)
			}
		}
		c, sent = recordingDaemon(t)
		tty := withTTY(t, "yes")
		if err := runClientArgs(c, args); err != nil {
			t.Errorf("%v confirmed: %v", args, err)
		}
		if got := sent(); len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "/"+args[0]+"/") {
			t.Errorf("%v confirmed: daemon asked %v", args, got)
		}
		if !strings.Contains(tty.prompt.String(), "Type yes") {
			t.Errorf("%v: no prompt on the terminal", args)
		}
	}
	for _, args := range [][]string{
		{"peers", "list"}, {"peers", "deny", "bafyreipeer000000000"}, {"peers", "remove", "bafyreipeer000000000"},
		{"inbound", "pending"}, {"inbound", "reject", "ix_1"}, {"inbound", "policy", "closed"}, {"inbound", "policy"},
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
	}
	panic("runClientArgs: " + args[0])
}
