package main

// UX-T6 (ux §2.7 UX-D5, §2.3 UX-D3; SI-5): what a fresh install is, item by
// item, and what the daemon will not start with. This file is the frame: an
// item whose area has not landed yet is skipped with the work item that
// brings it (R-5), and fails once it holds while the skip is still there.
// The areas add the mutation of their own items (ux §6.2, unit/ux.py).

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// freshSI5 is a fresh install: `anet init`, then `anet doctor --json`.
type freshSI5 struct {
	layout daemon.Layout
	config map[string]any
	doctor map[string]any
}

func readFreshSI5(t *testing.T) freshSI5 {
	t.Helper()
	l := freshInit(t)
	b, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	doc, derr := doctorJSON(t, l, testDoctorEnv(t))
	if derr != nil {
		t.Fatalf("doctor on a fresh install failed: %v", derr)
	}
	return freshSI5{layout: l, config: cfg, doctor: doc}
}

// dig is the value at a dotted key of m, and whether it is there.
func dig(m map[string]any, key string) (any, bool) {
	var cur any = m
	for _, k := range strings.Split(key, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[k]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// emptyPrivateFile is a list file of a fresh install: there, empty, 0600.
func emptyPrivateFile(f freshSI5, name string) error {
	fi, err := os.Stat(filepath.Join(f.layout.Root, name))
	if err != nil {
		return err
	}
	if fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
		return errors.New(name + ": size " + fi.Mode().String())
	}
	return nil
}

// si5Items are ux §2.7 UX-D5, one per assertion. todo is the work item that
// makes an item hold, while it does not.
var si5Items = []struct {
	name  string
	todo  string
	check func(f freshSI5) error
}{
	{"inbound.policy is closed", "", func(f freshSI5) error {
		if v, _ := dig(f.config, "inbound.policy"); v != "closed" {
			return errors.New("inbound.policy " + toString(v))
		}
		return nil
	}},
	{"peers.allow, peers.trust and peers.deny are empty 0600 files", "", func(f freshSI5) error {
		for _, n := range []string{"peers.allow", "peers.trust", "peers.deny"} {
			if err := emptyPrivateFile(f, n); err != nil {
				return err
			}
		}
		return nil
	}},
	{"public_capabilities is []", "", func(f freshSI5) error {
		v, ok := dig(f.config, "inbound.public_capabilities")
		if l, isList := v.([]any); !ok || !isList || len(l) != 0 {
			return errors.New("public_capabilities " + toString(v))
		}
		return nil
	}},
	{"no auto_reply block", "", func(f freshSI5) error {
		if _, ok := f.config["auto_reply"]; ok {
			return errors.New("auto_reply is there")
		}
		return nil
	}},
	{"payments: auto_max = agent_max = agent_daily_max = 0, explicit_max = 10, daily_max = 50", "", func(f freshSI5) error {
		for k, want := range map[string]float64{"auto_max": 0, "agent_max": 0, "agent_daily_max": 0, "explicit_max": 10, "daily_max": 50} {
			if v, _ := dig(f.config, "payments."+k); v != want {
				return errors.New("payments." + k + " " + toString(v))
			}
		}
		return nil
	}},
	{"payees.allow is an empty 0600 file", "", func(f freshSI5) error { return emptyPrivateFile(f, "payees.allow") }},
	{"stream_preview is off", "", func(f freshSI5) error {
		si5, _ := f.doctor["si5"].(map[string]any)
		if si5["stream_preview"] != "off" {
			return errors.New("doctor si5 has no stream_preview off")
		}
		return nil
	}},
	{"content_retention is 30d", "", func(f freshSI5) error {
		if v, _ := dig(f.config, "content_retention"); v != "30d" {
			return errors.New("content_retention " + toString(v))
		}
		return nil
	}},
	{"modules.p2p.lan is not true", "", func(f freshSI5) error {
		if v, _ := dig(f.config, "modules.p2p.lan"); v == true {
			return errors.New("modules.p2p.lan is true")
		}
		return nil
	}},
	{"no removed key of ux §2.3", "", func(f freshSI5) error {
		b, _ := json.Marshal(f.config)
		found, err := daemon.FindRemovedSettings(b)
		if err != nil {
			return err
		}
		for _, r := range found {
			return errors.New(r.Path + " is in a fresh config.json")
		}
		return nil
	}},
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// UX-T6, UX-D5: a fresh install holds every item of ux §2.7.
func TestFreshInstallIsSafe(t *testing.T) {
	f := readFreshSI5(t)
	for _, it := range si5Items {
		t.Run(it.name, func(t *testing.T) {
			err := it.check(f)
			if it.todo != "" {
				if err == nil {
					t.Fatalf("this item holds now: drop its %s", it.todo)
				}
				t.Skip(it.todo + ": " + err.Error())
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
}

// UX-T6, UX-D3: each removed key of ux §2.3, alone in a fresh install's
// config.json, stops `anet daemon` before it starts; the sentence names the
// key and what to do instead, and it is the sentence doctor's config.removed
// shows (UX-T1).
func TestRemovedKeysStopAnetDaemon(t *testing.T) {
	keys := map[string]any{
		"pricing": "3 credits", "payments.publish_prices": false, "modules.x402.voucher_addr": "127.0.0.1:8402",
		"modules.x402.voucher_url": "https://x/redeem", "modules.x402.witness_hub": "https://hub.example",
		"modules.x402.witness_every_seconds": 60, "providers": map[string]any{}, "accept_delegations": true,
		"rotation_grace": "2h", "inbound.allow_file": "peers.allow", "inbound.deny_file": "peers.deny",
		"inbound.trust_file": "peers.trust", "inbound.reject_notice": map[string]any{}, "inbound.pending": map[string]any{},
		"inbound.public_capabilities[].evidence": "full", "auto_reply.require_image": true, "auto_reply.usage_hint": "x",
		"auto_reply.error_reply": "x", "auto_reply.poll_interval_seconds": 5, "auto_reply.max_auto_replies": 3,
		"payments.payees_file": "payees.allow", "modules.anetlink.timeout_s": 10, "modules.service.expected_uid": 1000,
		"modules.a2a.backends[].expected_uid": 1000, "modules.anetlink.expected_uid": 1000,
		"modules.p2p.dial_timeout_ms": 500, "modules.p2p.reach_timeout_ms": 500,
	}
	for _, lang := range []string{"C", "zh_CN.UTF-8"} {
		l := a2ashape.LangFrom(func(k string) string { return map[string]string{"LANG": lang}[k] })
		for _, key := range slices.Sorted(maps.Keys(keys)) {
			value := keys[key]
			t.Run(lang+"/"+key, func(t *testing.T) {
				// The key of a module block that is not in this build is
				// also module_absent.configured, which is a second sentence
				// (MOD-F1); this test is about the removed key alone.
				if rest, ok := strings.CutPrefix(key, "modules."); ok {
					m, _, _ := strings.Cut(rest, ".")
					needModule(t, m)
				}
				h := newCLIHome(t, lang)
				layout := daemon.NewLayout(h.data)
				if err := initTo(nopWriter{}, layout, false, nil, nil); err != nil {
					t.Fatal(err)
				}
				b, _ := os.ReadFile(layout.ConfigPath())
				var cfg map[string]any
				_ = json.Unmarshal(b, &cfg)
				putKey(cfg, key, value)
				b, _ = json.Marshal(cfg)
				found, err := daemon.FindRemovedSettings(b)
				if err != nil || len(found) != 1 {
					t.Fatalf("%v %+v", err, found)
				}
				if err := os.WriteFile(layout.ConfigPath(), b, 0o600); err != nil {
					t.Fatal(err)
				}
				before := snapshotDir(t, h.data)
				_, stderr, code := runCLIWithin(t, h, 20*time.Second, []string{"daemon"})
				want := a2ashape.Sentence("config_key_removed", l, a2ashape.Args{"key": found[0].Path, "instead": found[0].Instead})
				if code != 1 || stderr != want+"\n" {
					t.Fatalf("anet daemon with %s: exit %d, stderr %q; want 1 and %q", key, code, stderr, want)
				}
				if !sameSnapshot(before, snapshotDir(t, h.data)) {
					t.Error("anet daemon wrote to the data directory before refusing")
				}
				// doctor says it in the same words.
				env := testDoctorEnv(t)
				env.getenv = func(k string) string { return map[string]string{"LANG": lang}[k] }
				rep, err := collectDoctor(layout, env)
				if err != nil {
					t.Fatal(err)
				}
				var doc []string
				for _, c := range rep.Checks {
					if c.ID == "config.removed" {
						doc = append(doc, c.Detail)
					}
				}
				if len(doc) != 1 || doc[0] != want || rep.OK {
					t.Errorf("doctor config.removed: %q (ok %v); want %q", doc, rep.OK, want)
				}
			})
		}
	}
}

// putKey sets a dotted key of ux §2.3 in m ("name[]": the first element of
// that array, made when missing).
func putKey(m map[string]any, key string, value any) {
	segs := strings.Split(key, ".")
	cur := m
	for i, s := range segs {
		name, each := strings.CutSuffix(s, "[]")
		if i == len(segs)-1 {
			cur[name] = value
			return
		}
		if each {
			el := map[string]any{"id": "net.echo"}
			cur[name] = []any{el}
			cur = el
			continue
		}
		next, ok := cur[name].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[name] = next
		}
		cur = next
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// `anet up` refuses the same config.json before it spawns the daemon or
// opens its log.
func TestAnetUpRefusesARemovedKey(t *testing.T) {
	l := freshInit(t)
	b, _ := os.ReadFile(l.ConfigPath())
	var cfg map[string]any
	_ = json.Unmarshal(b, &cfg)
	cfg["rotation_grace"] = "2h"
	b, _ = json.Marshal(cfg)
	if err := os.WriteFile(l.ConfigPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runDaemonDetached(l); !errors.Is(err, errQuiet) {
		t.Fatalf("anet up: %v", err)
	}
	if _, err := os.Stat(l.LogPath()); err == nil {
		t.Error("anet up opened the daemon log before refusing")
	}
}
