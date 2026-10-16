//go:build !no_autoreply

package main

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
)

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
