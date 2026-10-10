package main

// TestDoctorSenderPrivacy (SS-12; ux §5.2, sealed-sender §5.4 SSR-U4):
// doctor's five sealed-sender ids — sender_privacy.mailbox_key, .session,
// .grants, .hub and p2p.lan — when each is shown, what it decides from
// /status.sender_privacy, and the order and thresholds of the three warnings
// of sender_privacy.hub.

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// findingOf writes a finding as "id status key" with its {n} and {m}.
func findingOf(f senderFinding) string {
	s := f.id + " " + f.status + " " + f.key
	for _, k := range []string{"n", "m"} {
		if v, ok := f.args[k]; ok {
			b, _ := json.Marshal(v)
			s += " " + k + "=" + string(b)
		}
	}
	return strings.TrimSpace(s)
}

func TestDoctorSenderPrivacy(t *testing.T) {
	healthy := senderPrivacyView{MailboxRegistered: true, SessionOK: true, Held: 4, Issued: 2}
	with := func(f func(*senderPrivacyView)) senderPrivacyView { v := healthy; f(&v); return v }
	okRows := []string{"sender_privacy.mailbox_key ok", "sender_privacy.session ok",
		"sender_privacy.grants info sender_privacy.grants_summary n=4 m=2", "sender_privacy.hub ok"}
	replace := func(i int, s string) []string { out := append([]string(nil), okRows...); out[i] = s; return out }
	for _, c := range []struct {
		name string
		sp   senderPrivacyView
		want []string
	}{
		{"all well", healthy, okRows},
		{"the mailbox key is not at the hub", with(func(v *senderPrivacyView) { v.MailboxRegistered = false }),
			replace(0, "sender_privacy.mailbox_key warn sender_privacy.mailbox_pending")},
		{"the last session opening failed", with(func(v *senderPrivacyView) { v.SessionOK = false }),
			replace(1, "sender_privacy.session warn session_unavailable")},
		{"a withdrawal not yet at the hub, under an hour", with(func(v *senderPrivacyView) { v.UnpushedRevocationAgeS = 3599 }),
			okRows},
		{"withdrawals an hour old", with(func(v *senderPrivacyView) { v.UnpushedRevocationAgeS, v.UnpushedRevocations1h = 3600, 2 }),
			replace(2, "sender_privacy.grants warn sender_privacy.withdrawals_pending n=2")},
		{"a withdrawal over an hour old, counted a moment before", with(func(v *senderPrivacyView) { v.UnpushedRevocationAgeS = 7200 }),
			replace(2, "sender_privacy.grants warn sender_privacy.withdrawals_pending n=1")},
		{"all three of the hub at once: the epoch first", with(func(v *senderPrivacyView) {
			v.HubEpochInvalid, v.RefusalDowngrades24h, v.SmallBuckets24h = true, 9, 4
		}), replace(3, "sender_privacy.hub warn sender_privacy.hub_epoch_invalid")},
		{"three downgrades and a small group: the downgrades", with(func(v *senderPrivacyView) {
			v.RefusalDowngrades24h, v.SmallBuckets24h = 3, 1
		}), replace(3, "sender_privacy.hub warn sender_privacy.hub_refusals n=3")},
		{"two downgrades and a small group: the group", with(func(v *senderPrivacyView) {
			v.RefusalDowngrades24h, v.SmallBuckets24h = 2, 1
		}), replace(3, "sender_privacy.hub warn sender_privacy.hub_small_bucket")},
		{"two downgrades alone: nothing to tell", with(func(v *senderPrivacyView) { v.RefusalDowngrades24h = 2 }), okRows},
		{"one downgrade, as a peer taking you off its list does", with(func(v *senderPrivacyView) { v.RefusalDowngrades24h = 1 }), okRows},
		{"one small group", with(func(v *senderPrivacyView) { v.SmallBuckets24h = 1 }),
			replace(3, "sender_privacy.hub warn sender_privacy.hub_small_bucket")},
	} {
		var got []string
		for _, f := range senderPrivacyFindings(c.sp) {
			got = append(got, findingOf(f))
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got:  %q\n want: %q", c.name, got, c.want)
		}
	}

	// Through doctor: when each is shown, and its sentence.
	setConfig := func(t *testing.T, l daemon.Layout, f func(map[string]any)) {
		t.Helper()
		b, err := os.ReadFile(l.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		b, _ = json.Marshal(m)
		if err := os.WriteFile(l.ConfigPath(), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	status := func(state string, sp senderPrivacyView) map[string]any {
		b, _ := json.Marshal(sp)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return map[string]any{"aid": "x", "hub": map[string]any{"state": state}, "sender_privacy": m}
	}
	run := func(t *testing.T, l daemon.Layout, st map[string]any) []doctorCheck {
		t.Helper()
		env := testDoctorEnv(t)
		if st != nil {
			env.running = func(string, string) bool { return true }
			env.status = func(string, string) (map[string]any, error) { return st, nil }
		}
		rep, err := collectDoctor(l, env)
		if err != nil {
			t.Fatal(err)
		}
		var out []doctorCheck
		for _, c := range rep.Checks {
			if strings.HasPrefix(c.ID, "sender_privacy.") || c.ID == "p2p.lan" {
				out = append(out, c)
			}
		}
		return out
	}
	ids := func(cs []doctorCheck) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.ID+" "+c.Status)
		}
		return out
	}

	l := freshInit(t)
	busy := with(func(v *senderPrivacyView) { v.SessionOK, v.RefusalDowngrades24h = false, 4 })
	if got := run(t, l, status(daemon.JoinRegistered, busy)); len(got) != 0 {
		t.Errorf("no hub in config.json: %v", ids(got))
	}
	setConfig(t, l, func(m map[string]any) { m["hub_url"] = "https://hub.example" })
	got := run(t, l, nil)
	if want := []string{"sender_privacy.mailbox_key unknown", "sender_privacy.session unknown", "sender_privacy.grants unknown",
		"sender_privacy.hub unknown"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("daemon down: %v, want %v (UX-K2)", ids(got), want)
	}
	for _, c := range got {
		if c.Reason != doctorDaemonDown || c.Hint != "anet up" {
			t.Errorf("daemon down: %+v", c)
		}
	}
	if got := run(t, l, status("pending_admission", busy)); len(got) != 0 {
		t.Errorf("not registered yet: %v", ids(got))
	}
	got = run(t, l, status(daemon.JoinRegistered, busy))
	want := []doctorCheck{
		{ID: "sender_privacy.mailbox_key", Status: stOK},
		sayCheck("sender_privacy.session", stWarn, "session_unavailable", nil, a2ashape.English),
		sayCheck("sender_privacy.grants", stInfo, "sender_privacy.grants_summary", a2ashape.Args{"n": 4, "m": 2}, a2ashape.English),
		sayCheck("sender_privacy.hub", stWarn, "sender_privacy.hub_refusals", a2ashape.Args{"n": 4}, a2ashape.English),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered:\n got:  %+v\n want: %+v", got, want)
	}
	if got[1].Hint != "anet status" || !strings.Contains(got[3].Detail, "4") {
		t.Errorf("session: hint %q; hub: %q", got[1].Hint, got[3].Detail)
	}
	// The human report (UX-K5).
	env := testDoctorEnv(t)
	env.running = func(string, string) bool { return true }
	env.status = func(string, string) (map[string]any, error) { return status(daemon.JoinRegistered, busy), nil }
	var buf bytes.Buffer
	_ = doctorTo(&buf, l, env, false)
	line := "[warn] sender_privacy.hub  " + a2ashape.Sentence("sender_privacy.hub_refusals", a2ashape.English, a2ashape.Args{"n": 4}) + "\n"
	if !strings.Contains(buf.String(), line) {
		t.Errorf("no %q in\n%s", line, buf.String())
	}

	// p2p.lan: only while it is on, daemon or not.
	for _, lan := range []any{nil, false, true} {
		setConfig(t, l, func(m map[string]any) {
			if lan == nil {
				delete(m, "modules")
				return
			}
			m["modules"] = map[string]any{"p2p": map[string]any{"socket": "/run/anetpeer.sock", "lan": lan}}
		})
		var p2p []doctorCheck
		for _, c := range run(t, l, nil) {
			if c.ID == "p2p.lan" {
				p2p = append(p2p, c)
			}
		}
		if lan == true {
			if len(p2p) != 1 || !reflect.DeepEqual(p2p[0], sayCheck("p2p.lan", stInfo, "p2p.lan_on", nil, a2ashape.English)) {
				t.Errorf("modules.p2p.lan true: %+v", p2p)
			}
		} else if len(p2p) != 0 {
			t.Errorf("modules.p2p.lan %v: %+v", lan, p2p)
		}
	}
}
