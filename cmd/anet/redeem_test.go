package main

import (
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

// redeemDaemon is a control plane with a hub it settles on
// (/payments/status {"hub": true}) that records each request's path and
// body.
func redeemDaemon(t *testing.T, hubAID string) (*client, func() []string, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	var redeem map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/redeem" {
			_ = json.Unmarshal(b, &redeem)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"explicit_max":10,"daily_max":50,"spent_24h":3,"hub":"https://hub.example","hub_aid":"` +
			hubAID + `","verified":true}`))
	}))
	t.Cleanup(srv.Close)
	c := &client{base: srv.URL, token: "t", timeout: 5 * time.Second}
	return c, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), paths...)
		}, func() map[string]any {
			mu.Lock()
			defer mu.Unlock()
			return redeem
		}
}

// The hub the operator is shown is the hub paid: the prompt is
// redeem_confirm with the hub the node settles on (ux §1.4 M1, UX-R12), and
// /redeem carries that hub's AID, read in the same answer, as pay_to, which
// the daemon signs to and to nothing else (it refuses when the node settles
// on another hub by then).
// Without a terminal nothing reaches the daemon; without "yes", nothing
// is redeemed.
func TestRedeemSendsThePayeeItShowed(t *testing.T) {
	// The flow of a build with the payment module, in any build.
	prev := paymentsCompiled
	t.Cleanup(func() { paymentsCompiled = prev })
	paymentsCompiled = func() bool { return true }
	const hub = "bafyreihub00000000000"
	c, paths, body := redeemDaemon(t, hub)
	withTTY(t, "")
	if err := runRedeem(c, []string{"7"}); !errors.Is(err, errNoTTY) || len(paths()) != 0 {
		t.Fatalf("no terminal: %v, daemon asked %v", err, paths())
	}
	withTTY(t, "no")
	if err := runRedeem(c, []string{"7"}); err == nil || strings.Join(paths(), ",") != "/payments/status" {
		t.Fatalf("not confirmed: %v, daemon asked %v", err, paths())
	}

	c, paths, body = redeemDaemon(t, hub)
	tty := withTTY(t, "yes")
	if err := runRedeem(c, []string{"7", "--ref", "invoice 7"}); err != nil {
		t.Fatal(err)
	}
	lang := a2ashape.Lang()
	want := a2ashape.Sentence("redeem_confirm", lang, a2ashape.Args{"amount": 7, "hub": "https://hub.example",
		"single": 10, "left": 47}) + "\n" + a2ashape.Sentence("confirm.yes", lang, nil) + " "
	if tty.prompt.String() != want {
		t.Errorf("prompt %q, want %q", tty.prompt.String(), want)
	}
	if got := strings.Join(paths(), ","); got != "/payments/status,/redeem" {
		t.Fatalf("daemon asked %s", got)
	}
	if b := body(); b["pay_to"] != hub || b["amount"] != 7.0 || b["reference"] != "invoice 7" {
		t.Fatalf("/redeem body %v, want pay_to %s", b, hub)
	}
}

// A build without the payment module says so before it asks anything: no
// terminal is opened and the daemon is not asked. [mut] the check after
// ttyConfirm → the operator is asked to confirm a redemption that cannot
// happen, and this is red.
func TestRedeemWithoutThePaymentModuleDoesNotAsk(t *testing.T) {
	prev := paymentsCompiled
	t.Cleanup(func() { paymentsCompiled = prev })
	paymentsCompiled = func() bool { return false }
	c, paths, _ := redeemDaemon(t, "bafyreihub00000000000")
	prevTTY := openTTY
	t.Cleanup(func() { openTTY = prevTTY })
	opened := false
	openTTY = func() (io.ReadWriteCloser, error) { opened = true; return nil, errors.New("asked") }
	err := runRedeem(c, []string{"7"})
	if !errors.Is(err, errNoPaymentModule) || !strings.Contains(err.Error(), "no payment module") {
		t.Fatalf("redeem without the payment module: %v", err)
	}
	if opened || len(paths()) != 0 {
		t.Fatalf("terminal opened %v, daemon asked %v", opened, paths())
	}
}
