package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// delegateThrough posts one goal delegation at the control API exactly as the CLI does, and returns the
// decoded answer. Going through hDelegate rather than Delegate is the point: the defect was that the
// daemon consumed the hub's answer and the control plane replied without it.
func delegateThrough(t *testing.T, d *Daemon, providerAID, goal string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"provider": providerAID, "goal": goal})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/delegate", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	d.hDelegate(rec, r)
	if rec.Code != 200 {
		t.Fatalf("delegate = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("delegate answer is not JSON: %v (%s)", err, rec.Body.String())
	}
	return out
}

// The hub says the provider has stopped collecting its mail; `anet delegate` has to say so too.
//
// The hub returns recipient_quiet + warning on /relay/send. The daemon parsed both, logged them to its
// own file and answered the control plane with the interaction id alone — so the one party who never
// learned that the provider had been silent for days was the person who typed the command and then
// waited for an answer. The task is still queued: quiet is not dead, and a single poll by the provider
// collects everything waiting, so the mark is an addition to the answer and never a refusal.
func TestDelegateCarriesTheHubsQuietWarning(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Bob", nil, GuestDefaultMessages, ""); err != nil {
		t.Fatal(err)
	}

	// A provider that has just registered is not quiet, and the answer must not claim it is.
	fresh := delegateThrough(t, req, prov.AID(), "hello")
	if _, marked := fresh["recipient_quiet"]; marked {
		t.Fatalf("a provider that just registered must not be reported quiet: %v", fresh)
	}

	// Three days without collecting mail — the real hub's SetLastSeenForTest equivalent.
	backdateLastSeen(srv.URL, prov.AID(), 72*time.Hour)

	out := delegateThrough(t, req, prov.AID(), "still there?")
	if id, _ := out["interaction_id"].(string); id == "" {
		t.Fatalf("the delegation must still be queued: %v", out)
	}
	if quiet, _ := out["recipient_quiet"].(bool); !quiet {
		t.Fatalf("the hub reported the recipient quiet and the answer dropped it: %v", out)
	}
	warn, _ := out["warning"].(string)
	if !strings.Contains(warn, "has not collected its mail") {
		t.Fatalf("the warning must say what is wrong: %q", warn)
	}
	if !strings.Contains(warn, "3 days") {
		t.Fatalf("the warning must say how long the recipient has been silent: %q", warn)
	}
	if !strings.Contains(warn, prov.AID()) {
		t.Fatalf("the warning must name the recipient: %q", warn)
	}
}

// A peer that starts collecting its mail again must stop being reported quiet.
//
// The mark is remembered per peer so the log stays quiet across a burst of messages. Remembering it
// without ever retracting it would make the first silence permanent: every later delegation would carry
// a warning that the hub itself no longer makes, which is worse than not warning at all — a mark that
// stops being true without saying so teaches an operator to ignore it.
func TestTheQuietMarkIsRetractedWhenThePeerCollectsAgain(t *testing.T) {
	srv := newFakeHub(t)
	ctx := context.Background()
	req := newTestDaemon(t, srv.URL, false)
	prov := newTestDaemon(t, srv.URL, true)
	if err := req.RegisterWithHub(ctx, srv.URL, "Alice", nil, GuestDefaultMessages, ""); err != nil {
		t.Fatal(err)
	}
	if err := prov.RegisterWithHub(ctx, srv.URL, "Bob", nil, GuestDefaultMessages, ""); err != nil {
		t.Fatal(err)
	}
	backdateLastSeen(srv.URL, prov.AID(), 72*time.Hour)
	if out := delegateThrough(t, req, prov.AID(), "are you there"); out["recipient_quiet"] != true {
		t.Fatalf("setup: the first delegation should have been marked quiet: %v", out)
	}

	// The provider comes back and collects its mailbox; the hub records the poll.
	if err := prov.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	out := delegateThrough(t, req, prov.AID(), "welcome back")
	if _, marked := out["recipient_quiet"]; marked {
		t.Fatalf("the peer has collected its mail; the stale quiet mark must be dropped: %v", out)
	}
	if warn, _ := out["warning"].(string); warn != "" {
		t.Fatalf("no warning is due once the peer is collecting again: %q", warn)
	}
}

// Two identities created without ever starting a daemon must not be handed the same control port.
//
// LoadConfig wrote the hardcoded 127.0.0.1:39811 for any data dir without a config, bypassing
// AllocControlPort. Both identities then named the same port: whichever daemon bound first won, the
// second could not start, and a CLI command aimed at either identity reached whichever daemon held the
// port — acting on the wrong node rather than failing.
func TestEachIdentityGetsItsOwnControlPort(t *testing.T) {
	t.Setenv("ANET_HOME", t.TempDir())
	a, b := IdentityLayout("alpha"), IdentityLayout("beta")

	ca, err := LoadConfig(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := LoadConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if ca.ControlAddr == "" || cb.ControlAddr == "" {
		t.Fatalf("both identities need a control address: %q %q", ca.ControlAddr, cb.ControlAddr)
	}
	if ca.ControlAddr == cb.ControlAddr {
		t.Fatalf("two identities were handed the same control address %q", ca.ControlAddr)
	}

	// The port has to be pinned on disk, not re-picked per process: the CLI and the daemon resolve it
	// separately, and a value that moved between reads would point them at different addresses.
	for _, tc := range []struct {
		name string
		l    Layout
		want string
	}{{"alpha", a, ca.ControlAddr}, {"beta", b, cb.ControlAddr}} {
		again, err := LoadConfig(tc.l)
		if err != nil {
			t.Fatal(err)
		}
		if again.ControlAddr != tc.want {
			t.Errorf("%s: control address moved between reads: %q then %q", tc.name, tc.want, again.ControlAddr)
		}
	}

	// A config that already names the historical port keeps it: an existing install must not be
	// renumbered underneath a script that was told which port to talk to.
	legacy := IdentityLayout("legacy")
	if err := SaveConfig(legacy, Config{ControlAddr: "127.0.0.1:39811"}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got.ControlAddr != "127.0.0.1:39811" {
		t.Errorf("an existing control_addr must be left alone, got %q", got.ControlAddr)
	}
}
