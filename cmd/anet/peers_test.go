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
// It holds one delegation, ix_1, for approval, one quote on a task this
// node delegated (/thread), and a hub it settles on (/payments/status).
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
			`"auto_max":0,"agent_max":0,"agent_daily_max":0,"explicit_max":10,"daily_max":50,"spent_24h":3,` +
			`"hub":"https://hub.example","hub_aid":"bafyreihub00000000000",` +
			`"thread":{"role":"outbound","messages":[{"metadata":{"x402.payment.required":{"x402Version":2,"accepts":[` +
			`{"scheme":"anet-credit","network":"hub:bafyreihub","amount":"5","asset":"credit","payTo":"bafyreipeer000000000"}]}}}]}}`))
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
	withPaymentsCompiled(t)
	grants := [][]string{
		{"peers", "allow", "bafyreipeer000000000"},
		{"peers", "trust", "bafyreipeer000000000"},
		{"inbound", "approve", "ix_1"},
		{"inbound", "policy", "open"},
		{"inbound", "policy", "approve"},
		{"payments", "set", "auto_max=5"},
		{"payments", "set", "daily_max=0"},
		{"pay", "ix_2"},
		{"payees", "add", "bafyreipeer000000000"},
		{"redeem", "5"},
		{"redeem", "5", "--ref", "invoice 7"},
	}
	// What a prompt may read before asking.
	readOnly := map[string]bool{"/inbound/pending": true, "/find": true, "/payments/status": true, "/thread": true}
	// The request a confirmed command ends with.
	final := map[string]string{"peers": "/peers/", "inbound": "/inbound/", "payments": "/payments/limits",
		"pay": "/tasks/pay-manual", "payees": "/payees/add", "redeem": "/redeem"}
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
		if got := sent(); len(got) < 2 && (args[0] == "payments" || args[0] == "pay" || args[0] == "payees" ||
			args[0] == "redeem") || len(got) == 0 ||
			!strings.HasPrefix(got[len(got)-1], final[args[0]]) {
			t.Errorf("%v confirmed: daemon asked %v", args, got)
		}
		if !strings.Contains(tty.prompt.String(), "Type yes") {
			t.Errorf("%v: no prompt on the terminal", args)
		}
	}
	for _, args := range [][]string{
		{"peers", "list"}, {"peers", "deny", "bafyreipeer000000000"}, {"peers", "remove", "bafyreipeer000000000"},
		{"inbound", "pending"}, {"inbound", "list"}, {"inbound", "reject", "ix_1"}, {"inbound", "policy", "closed"},
		{"inbound", "policy"}, {"payments"}, {"payments", "show"}, {"pay", "ix_2", "--reject"},
		{"payees"}, {"payees", "list"}, {"payees", "remove", "bafyreipeer000000000"},
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
	case "pay":
		return runPay(c, args[1:])
	case "payees":
		return runPayees(c, args[1:])
	case "redeem":
		return runRedeem(c, args[1:])
	}
	panic("runClientArgs: " + args[0])
}

// The prompt names what is being granted: the held delegation's sender and
// size, each limit as current → new. Approving an id the daemon does not
// hold ends without asking and without calling approve.
func TestConfirmationsDescribeWhatTheyGrant(t *testing.T) {
	withPaymentsCompiled(t)
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
	if err := runClientArgs(c, []string{"payments", "set", "agent_max=7"}); err != nil {
		t.Fatal(err)
	}
	if p := tty.prompt.String(); !strings.Contains(p, "agent_max") || !strings.Contains(p, "0 → 7") {
		t.Errorf("limits prompt: %q", p)
	}
	if err := runClientArgs(c, []string{"payments", "set", "agent_max=-1"}); err == nil {
		t.Error("a negative limit was accepted")
	}
	// anet pay names the amount, the payee and the task.
	c, _ = recordingDaemon(t)
	tty = withTTY(t, "yes")
	if err := runClientArgs(c, []string{"pay", "ix_2"}); err != nil {
		t.Fatal(err)
	}
	if p := tty.prompt.String(); !strings.Contains(p, "ix_2") || !strings.Contains(p, "5 credit to bafyreipeer000000000") {
		t.Errorf("pay prompt: %q", p)
	}
	// anet redeem names the amount, the payee (the hub's AID) and the
	// limits it falls under.
	c, _ = recordingDaemon(t)
	tty = withTTY(t, "yes")
	if err := runClientArgs(c, []string{"redeem", "7", "--ref", "invoice 7"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Redeem 7 credits", "bafyreihub00000000000", "https://hub.example", "invoice 7",
		"explicit_max 10", "daily_max 50", "3 signed"} {
		if !strings.Contains(tty.prompt.String(), want) {
			t.Errorf("redeem prompt lacks %q: %q", want, tty.prompt.String())
		}
	}
	// anet payees add names the payee and what it may then be paid.
	c, _ = recordingDaemon(t)
	tty = withTTY(t, "yes")
	if err := runClientArgs(c, []string{"payees", "add", "bafyreipeer000000000"}); err != nil {
		t.Fatal(err)
	}
	if p := tty.prompt.String(); !strings.Contains(p, "pay bafyreipeer000000000") || !strings.Contains(p, "agent_max") ||
		!strings.Contains(p, "up to 10 each") {
		t.Errorf("payees add prompt: %q", p)
	}
}

// A redemption with no hub to name, or a hub whose identity is not known,
// ends before the question: there is no payee to show and nothing the
// daemon could sign to.
func TestRedeemWithoutAKnownHubDoesNotAsk(t *testing.T) {
	withPaymentsCompiled(t)
	for name, body := range map[string]string{
		"no hub":           `{"hub":"","hub_aid":"","explicit_max":10,"daily_max":50}`,
		"hub not answered": `{"hub":"https://hub.example","hub_aid":"","hub_error":"timeout","explicit_max":10,"daily_max":50}`,
	} {
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			_, _ = w.Write([]byte(body))
		}))
		c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
		tty := withTTY(t, "yes")
		err := runClientArgs(c, []string{"redeem", "5"})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "nothing was signed") {
			t.Errorf("%s: %v", name, err)
		}
		if strings.Join(paths, ",") != "/payments/status" || tty.prompt.Len() != 0 {
			t.Errorf("%s: daemon asked %v, prompt %q", name, paths, tty.prompt.String())
		}
	}
	if err := runClientArgs(&client{base: "http://127.0.0.1:1"}, []string{"redeem", "0"}); err == nil {
		t.Error("redeem 0 was accepted")
	}
}

// With the payee list turned off every payee is already allowed: `anet
// payees add` says so and ends before the question, instead of asking and
// then being refused.
func TestPayeesAddWithTheListOffDoesNotAsk(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"auto_max":0,"agent_max":0,"agent_daily_max":0,"explicit_max":10,"daily_max":50,"payees_file":""}`))
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	tty := withTTY(t, "yes")
	err := runClientArgs(c, []string{"payees", "add", "bafyreipeer000000000"})
	if err == nil || !strings.Contains(err.Error(), "payee list is off") {
		t.Fatalf("payees add with the list off: %v", err)
	}
	if strings.Join(paths, ",") != "/payments/status" || tty.prompt.Len() != 0 {
		t.Fatalf("daemon asked %v, prompt %q", paths, tty.prompt.String())
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

// withPaymentsCompiled makes the CLI behave as a build with the payment
// module for one test. These tests are about the terminal confirmation and
// the prompt, which a -tags no_x402 build never reaches: there `anet
// redeem` ends before asking (redeem_nox402_test.go).
func withPaymentsCompiled(t *testing.T) {
	t.Helper()
	prev := paymentsCompiled
	t.Cleanup(func() { paymentsCompiled = prev })
	paymentsCompiled = func() bool { return true }
}
