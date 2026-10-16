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

	"github.com/ANetResearch/ANet/internal/a2ashape"
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
	prev, prevErr := openTTY, ttyStderr
	ttyStderr = io.Discard
	t.Cleanup(func() { openTTY, ttyStderr = prev, prevErr })
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
			`"auto_max":0,"agent_max":0,"agent_daily_max":0,"explicit_max":10,"daily_max":50,"spent_today":3,"left_today":47,` +
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
	readOnly := map[string]bool{"/inbound/pending": true, "/agents/card": true, "/payments/status": true, "/thread": true,
		"/status": true, "/tasks/get": true}
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
		if !strings.HasSuffix(tty.prompt.String(), "\n"+a2ashape.Sentence("confirm.yes", a2ashape.Lang(), nil)+" ") {
			t.Errorf("%v: the prompt on the terminal does not end with confirm.yes: %q", args, tty.prompt.String())
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

// runClientArgs dispatches the commands this file tests, the way runClient
// does, against a given client.
func runClientArgs(c *client, args []string) error {
	switch args[0] {
	case "peers":
		return runPeers(c, args[1:])
	case "inbound":
		return runInbound(c, args[1:])
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

// The prompt names what is being granted, in the M group's lines (ux
// UX-R12): the held task and its sender by full AID, each limit as current →
// new, the quote with its payee and the manual limit, the redemption with
// the hub and the manual limit, the payee with what it may then be paid.
// Approving an id the daemon does not hold ends without asking and without
// calling approve.
func TestConfirmationsDescribeWhatTheyGrant(t *testing.T) {
	lang := a2ashape.Lang()
	prompt := func(args []string) string {
		t.Helper()
		c, _ := recordingDaemon(t)
		tty := withTTY(t, "yes")
		if err := runClientArgs(c, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return tty.prompt.String()
	}
	yes := "\n" + a2ashape.Sentence("confirm.yes", lang, nil) + " "
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"inbound", "approve", "ix_1"},
			a2ashape.Sentence("approve_confirm", lang, a2ashape.Args{"task": "ix_1", "peer": "bafyreipeer000000000"})},
		{[]string{"inbound", "policy", "open"}, a2ashape.Sentence("inbound.confirm_open", lang, nil)},
		{[]string{"inbound", "policy", "approve"}, a2ashape.Sentence("inbound.confirm_approve", lang, nil)},
		{[]string{"peers", "allow", "bafyreipeer000000000"},
			a2ashape.Sentence("allow_confirm", lang, a2ashape.Args{"peer": "bafyreipeer000000000"})},
		{[]string{"peers", "trust", "bafyreipeer000000000"},
			a2ashape.Sentence("trust_confirm.no_agent", lang, a2ashape.Args{"peer": "bafyreipeer000000000"})},
		{[]string{"payments", "set", "agent_max=7"}, a2ashape.Sentence("limits_confirm", lang, nil) + "\n  " +
			a2ashape.Sentence("limits_confirm.line", lang, a2ashape.Args{"key": "agent_max", "from": "0", "to": 7})},
		{[]string{"pay", "ix_2"}, a2ashape.Sentence("pay.prompt_quote", lang, a2ashape.Args{"amount": "5",
			"peer": "bafyreipeer000000000"}) + "\n" + a2ashape.Sentence("pay.prompt_no_price", lang, nil) + "\n" +
			a2ashape.Sentence("pay.prompt_limits", lang, a2ashape.Args{"single": 10, "left": 47})},
		{[]string{"redeem", "7", "--ref", "invoice 7"}, a2ashape.Sentence("redeem_confirm", lang,
			a2ashape.Args{"amount": 7, "hub": "https://hub.example", "single": 10, "left": 47})},
		{[]string{"payees", "add", "bafyreipeer000000000"}, a2ashape.Sentence("payee_confirm", lang,
			a2ashape.Args{"peer": "bafyreipeer000000000", "auto_max": 0, "agent_max": 0, "daily": 0})},
	} {
		if got := prompt(tc.args); got != tc.want+yes {
			t.Errorf("%v: the terminal showed\n%q\nwant\n%q", tc.args, got, tc.want+yes)
		}
	}
	c, sent := recordingDaemon(t)
	tty := withTTY(t, "yes")
	if err := runClientArgs(c, []string{"inbound", "approve", "ix_unknown"}); err == nil ||
		!strings.Contains(err.Error(), "no held delegation") {
		t.Errorf("approve of an unknown id: %v", err)
	}
	if strings.Contains(strings.Join(sent(), ","), "/inbound/approve") || tty.prompt.Len() != 0 {
		t.Errorf("unknown id: asked %q, daemon %v", tty.prompt.String(), sent())
	}
	c, _ = recordingDaemon(t)
	withTTY(t, "yes")
	if err := runClientArgs(c, []string{"payments", "set", "agent_max=-1"}); !isUsage(err) {
		t.Errorf("a negative limit: %v, want a usage error", err)
	}
}

// A redemption with no hub to name, or a hub whose identity is not known,
// ends before the question: there is no payee to show and nothing the
// daemon could sign to.
func TestRedeemWithoutAKnownHubDoesNotAsk(t *testing.T) {
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

// MOD-3: `peers trust` asks how a trusted peer's tasks will run on this node: directly, in the sandbox, or
// — without auto-reply (no auto_reply block, or the module is not available) — not at all.
func TestTrustConfirmFollowsAutoReply(t *testing.T) {
	lang := a2ashape.Lang()
	peer := "bafyreipeer000000000"
	for _, tc := range []struct{ status, key string }{
		{`{}`, "trust_confirm.no_agent"},
		{`{"auto_reply":{"backend":"exec","trusted":"direct"}}`, "trust_confirm.direct"},
		{`{"auto_reply":{"backend":"exec","trusted":"sandbox"}}`, "trust_confirm.sandbox"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tc.status))
		}))
		c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
		want := a2ashape.Sentence(tc.key, lang, a2ashape.Args{"peer": peer})
		if got := peerPrompt(c, "trust", peer); got != want {
			t.Errorf("status %s: prompt %q, want %q", tc.status, got, want)
		}
		srv.Close()
	}
}
