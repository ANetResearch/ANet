//go:build !no_autoreply

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// Each SI-5 key moved off its safe default is reported under its own name,
// and only that key.
func TestDoctorReportsEachSI5KeyThatChanged(t *testing.T) {
	const aid = "bafyreisomepeer000001"
	editConfig := func(t *testing.T, l daemon.Layout, f func(m map[string]any)) {
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
	block := func(m map[string]any, k string) map[string]any { return m[k].(map[string]any) }
	listFile := func(name string) func(*testing.T, daemon.Layout) {
		return func(t *testing.T, l daemon.Layout) {
			if err := os.WriteFile(filepath.Join(l.Root, name), []byte(aid+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfgEdit := func(f func(m map[string]any)) func(*testing.T, daemon.Layout) {
		return func(t *testing.T, l daemon.Layout) { editConfig(t, l, f) }
	}
	mutations := map[string]func(*testing.T, daemon.Layout){
		"inbound.policy": cfgEdit(func(m map[string]any) { block(m, "inbound")["policy"] = "approve" }),
		"peers.allow":    listFile("peers.allow"),
		"peers.trust":    listFile("peers.trust"),
		"inbound.public_capabilities": cfgEdit(func(m map[string]any) {
			block(m, "inbound")["public_capabilities"] = []any{map[string]any{"id": "net.echo"}}
		}),
		"auto_reply.untrusted": cfgEdit(func(m map[string]any) {
			m["auto_reply"] = map[string]any{"backend": "exec", "agent": "claude", "untrusted": "sandbox", "api_key": "k"}
		}),
		"payments.auto_max":        cfgEdit(func(m map[string]any) { block(m, "payments")["auto_max"] = 1 }),
		"payments.agent_max":       cfgEdit(func(m map[string]any) { block(m, "payments")["agent_max"] = 1 }),
		"payments.agent_daily_max": cfgEdit(func(m map[string]any) { block(m, "payments")["agent_daily_max"] = 1 }),
		"payments.payees":          listFile("payees.allow"),
		"payments.explicit_max":    cfgEdit(func(m map[string]any) { block(m, "payments")["explicit_max"] = 11 }),
		"payments.daily_max":       cfgEdit(func(m map[string]any) { block(m, "payments")["daily_max"] = 51 }),
		"stream_preview":           cfgEdit(func(m map[string]any) { m["stream_preview"] = "on" }),
		"content_retention":        cfgEdit(func(m map[string]any) { m["content_retention"] = "7d" }),
		"modules.p2p.lan": cfgEdit(func(m map[string]any) {
			if _, ok := m["modules"].(map[string]any); !ok {
				m["modules"] = map[string]any{}
			}
			block(m, "modules")["p2p"] = map[string]any{"lan": true}
		}),
		// Listed as info, not a violation (ux §2.7): install.sh --without writes it.
		"without": cfgEdit(func(m map[string]any) { m["without"] = []any{"cas"} }),
	}
	for _, key := range daemon.SI5Keys {
		mut, ok := mutations[key]
		if !ok {
			t.Errorf("no mutation for SI-5 key %s", key)
			continue
		}
		layout := freshInit(t)
		mut(t, layout)
		out, _ := doctorJSON(t, layout, testDoctorEnv(t))
		var changed []string
		for _, v := range out["si5_changed"].([]any) {
			changed = append(changed, v.(string))
		}
		if !slices.Equal(changed, []string{key}) {
			t.Errorf("%s changed: doctor reports %v", key, changed)
		}
	}
}
