package main

// UX-T9a (ux §5.1, §6.1): doctor's output keeps its form whatever the data
// directory holds. Every id it prints is registered (ux §5.2), every finding
// that is not ok says what to do (a hint, "-" when nothing is to run), a
// failing finding makes the exit status 1 and only then, and doctor changes
// no byte of the data directory. UX-T9b (the id set equals ux §5.2) comes
// with UX-19, once every area's checks are in.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANetCore/coredet"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// doctorStates are data directories doctor must report on in form.
func doctorStates(t *testing.T) map[string]func(*testing.T) (daemon.Layout, doctorEnv) {
	edit := func(l daemon.Layout, f func(map[string]any)) {
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
	return map[string]func(*testing.T) (daemon.Layout, doctorEnv){
		"empty directory": func(t *testing.T) (daemon.Layout, doctorEnv) {
			return daemon.NewLayout(t.TempDir()), testDoctorEnv(t)
		},
		"fresh init": func(t *testing.T) (daemon.Layout, doctorEnv) {
			return freshInit(t), testDoctorEnv(t)
		},
		"removed keys": func(t *testing.T) (daemon.Layout, doctorEnv) {
			l := freshInit(t)
			edit(l, func(m map[string]any) {
				m["pricing"] = "3 credits"
				m["rotation_grace"] = "2h"
				m["inbound"].(map[string]any)["public_capabilities"] = []any{map[string]any{"id": "net.echo", "evidence": "full"}}
			})
			return l, testDoctorEnv(t)
		},
		"invite in the environment": func(t *testing.T) (daemon.Layout, doctorEnv) {
			env := testDoctorEnv(t)
			env.getenv = func(k string) string {
				if k == daemon.InviteEnv {
					return "inv_abc"
				}
				return ""
			}
			return freshInit(t), env
		},
		"config that does not parse": func(t *testing.T) (daemon.Layout, doctorEnv) {
			l := freshInit(t)
			if err := os.WriteFile(l.ConfigPath(), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			return l, testDoctorEnv(t)
		},
		"an identity of anet 0.2": func(t *testing.T) (daemon.Layout, doctorEnv) {
			l := freshInit(t)
			old, err := coredet.Marshal(map[uint64]any{1: bytes.Repeat([]byte{1}, 32), 2: bytes.Repeat([]byte{2}, 32), 3: []any{}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(l.IdentityPath(), old, 0o600); err != nil {
				t.Fatal(err)
			}
			return l, testDoctorEnv(t)
		},
		"an identity without a recovery passphrase, unconfirmed": func(t *testing.T) (daemon.Layout, doctorEnv) {
			l := freshInit(t)
			c, _ := unprotectedIdentity(t)
			writeIdentity(t, l, c)
			return l, testDoctorEnv(t)
		},
		"readable config and token": func(t *testing.T) (daemon.Layout, doctorEnv) {
			l := freshInit(t)
			if err := os.Chmod(l.ConfigPath(), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(l.ControlTokenPath(), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return l, testDoctorEnv(t)
		},
	}
}

func TestDoctorOutputForm(t *testing.T) {
	for name, state := range doctorStates(t) {
		t.Run(name, func(t *testing.T) {
			l, env := state(t)
			before := snapshotDir(t, l.Root)
			var buf bytes.Buffer
			err := doctorTo(&buf, l, env, true)
			if !sameSnapshot(before, snapshotDir(t, l.Root)) {
				t.Error("doctor changed the data directory (UX-K1)")
			}
			var rep struct {
				Checks []doctorCheck `json:"checks"`
				OK     bool          `json:"ok"`
			}
			if jerr := json.Unmarshal(buf.Bytes(), &rep); jerr != nil {
				t.Fatalf("doctor --json is not JSON: %v", jerr)
			}
			failed := false
			for _, c := range rep.Checks {
				if !doctorIDKnown(c.ID) {
					if p := doctorIDsPending[c.ID]; p != "" {
						t.Logf("%s: %s replaces this id", c.ID, p)
					} else {
						t.Errorf("id %s is not registered (ux §5.2, doctor_ids.go)", c.ID)
					}
				}
				switch c.Status {
				case stOK, stInfo, stWarn, stFail, stUnknown:
				default:
					t.Errorf("%s: status %q (UX-K3)", c.ID, c.Status)
				}
				if c.Status != stOK && c.Hint == "" {
					t.Errorf("%s is %s without a hint (UX-K4)", c.ID, c.Status)
				}
				if c.Reason != "" {
					if _, ok := a2ashape.Lookup(c.Reason); !ok {
						t.Errorf("%s: reason %q is not in the reason table (UX-K7)", c.ID, c.Reason)
					}
				}
				failed = failed || c.Status == stFail
			}
			if failed != (err != nil) || failed == rep.OK {
				t.Errorf("a failing finding and only one exits 1: failed %v, err %v, ok %v (UX-K3)", failed, err, rep.OK)
			}
			if err != nil && !errors.Is(err, errQuiet) {
				t.Errorf("doctor failed instead of reporting: %v", err)
			}
		})
	}
}

// The ids doctor may still print outside the register (ux §5.3, each until
// its work item replaces it) may only go away: an id added there would pass
// UX-T9a unregistered.
func TestDoctorIDsPendingOnlyShrinks(t *testing.T) {
	most := map[string]string{"auto_reply.sandbox": "v030-todo(EXE-7)"}
	for id, p := range doctorIDsPending {
		if most[id] != p {
			t.Errorf("%s (%s) is let through unregistered; register it in doctor_ids.go (ux §5.2)", id, p)
		}
	}
}

// A finding told by the reason table: its reason is the key, its detail the
// sentence, its hint the command or "-" (UX-K4, K5, K7).
func TestSayCheck(t *testing.T) {
	c := sayCheck("hub", stInfo, "no_hub_yet", nil, a2ashape.English)
	if c.Reason != "no_hub_yet" || c.Detail != a2ashape.Sentence("no_hub_yet", a2ashape.English, nil) ||
		c.Hint != "anet hub-register "+a2ashape.OfficialHubURL {
		t.Errorf("%+v", c)
	}
	c = sayCheck("identity.recovery", stInfo, "identity.recovery_note", nil, a2ashape.Chinese)
	if c.Hint != noHint || c.Detail != a2ashape.Sentence("identity.recovery_note", a2ashape.Chinese, nil) {
		t.Errorf("a finding without a command: %+v", c)
	}
	if c := sayCheck("identity", stOK, "identity.ok", nil, a2ashape.English); c.Hint != "" {
		t.Errorf("an ok finding has no hint: %+v", c)
	}
}

// config.removed: each removed key fails with config_key_removed, as it stops
// the daemon; ANET_INVITE warns; a fresh install has no such finding. The
// sentence is the one the daemon refuses with (UX-T1).
func TestDoctorConfigRemoved(t *testing.T) {
	states := doctorStates(t)
	find := func(name string) []doctorCheck {
		l, env := states[name](t)
		rep, err := collectDoctor(l, env)
		if err != nil {
			t.Fatal(err)
		}
		var out []doctorCheck
		for _, c := range rep.Checks {
			if c.ID == "config.removed" {
				out = append(out, c)
			}
		}
		return out
	}
	if got := find("fresh init"); len(got) != 0 {
		t.Errorf("a fresh install has config.removed: %+v", got)
	}
	got := find("removed keys")
	want := []string{"pricing", "rotation_grace", "inbound.public_capabilities[0].evidence"}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i, c := range got {
		if c.Status != stFail || c.Reason != "config_key_removed" || !strings.Contains(c.Detail, want[i]) {
			t.Errorf("%+v", c)
		}
	}
	got = find("invite in the environment")
	if len(got) != 1 || got[0].Status != stWarn || !strings.Contains(got[0].Detail, daemon.InviteEnv) {
		t.Errorf("ANET_INVITE: %+v", got)
	}
}

// UX-K5: the human report is "[status] id  sentence" and "  → hint".
func TestDoctorHumanForm(t *testing.T) {
	l, env := doctorStates(t)["removed keys"](t)
	var buf bytes.Buffer
	_ = doctorTo(&buf, l, env, false)
	out := buf.String()
	want := "[fail] config.removed  " + a2ashape.Sentence("config_key_removed", a2ashape.English,
		a2ashape.Args{"key": "pricing", "instead": "pricing"}) + "\n"
	if !strings.Contains(out, want) {
		t.Errorf("no %q in\n%s", want, out)
	}
	if strings.Contains(out, "→ -") {
		t.Error("a hint of - is not printed as a command")
	}
	if !strings.Contains(out, "[info] identity  ") {
		t.Errorf("the status leads each line:\n%s", out)
	}
}

// UX-K2: a check that needs the running daemon reads /status, or is unknown
// with doctor.daemon_down, whose hint is its next step, anet up (UX-K4; ux
// §5.2 gives the doctor-only sentences that name a command a next step).
func TestDoctorRuntimeStatus(t *testing.T) {
	l := freshInit(t)
	env := testDoctorEnv(t)
	rep := &doctorReport{}
	var said []doctorCheck
	say := func(id, status, key string, args a2ashape.Args) {
		said = append(said, sayCheck(id, status, key, args, a2ashape.English))
	}
	if _, ok := runtimeStatus(env, rep, say, "delivery.outbox", l); ok || len(said) != 1 ||
		said[0].Status != stUnknown || said[0].Reason != doctorDaemonDown || said[0].Hint != "anet up" {
		t.Errorf("daemon down: %+v", said)
	}
	rep.Control.Running = true
	env.status = func(addr, token string) (map[string]any, error) { return map[string]any{"aid": "x"}, nil }
	said = nil
	if st, ok := runtimeStatus(env, rep, say, "delivery.outbox", l); !ok || st["aid"] != "x" || len(said) != 0 {
		t.Errorf("daemon up: %v %v %+v", st, ok, said)
	}
	if !doctorIDKnown("agents.claude") || !doctorIDKnown("hermes.config") || doctorIDKnown("agents") == false ||
		doctorIDKnown("agents.") || doctorIDKnown("identity.nothing") {
		t.Error("doctorIDKnown")
	}
	for _, d := range doctorIDs {
		for _, k := range d.keys {
			if _, ok := a2ashape.Lookup(k); !ok {
				t.Errorf("%s: key %s is not in the reason table", d.id, k)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(l.Root, "control_token.txt")); err == nil {
		t.Error("runtimeStatus wrote a token")
	}
}
