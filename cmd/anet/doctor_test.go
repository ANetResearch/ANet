package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// testDoctorEnv reads no real home directory and finds no running daemon.
func testDoctorEnv(t *testing.T) doctorEnv {
	return doctorEnv{home: t.TempDir(), hermesHome: filepath.Join(t.TempDir(), "hermes")}
}

func doctorJSON(t *testing.T, layout daemon.Layout, env doctorEnv) (map[string]any, error) {
	t.Helper()
	var buf bytes.Buffer
	err := doctorTo(&buf, layout, env, true)
	var out map[string]any
	if jerr := json.Unmarshal(buf.Bytes(), &out); jerr != nil {
		t.Fatalf("doctor --json is not JSON: %v\n%s", jerr, buf.String())
	}
	return out, err
}

func freshInit(t *testing.T) daemon.Layout {
	t.Helper()
	layout := daemon.NewLayout(t.TempDir())
	if err := initTo(io.Discard, layout, false); err != nil {
		t.Fatal(err)
	}
	return layout
}

// SI-5: after `anet init` on a fresh data directory, `anet doctor --json`
// reports every key of the invariant under its own name with the safe
// default, and nothing as changed.
func TestAFreshInitIsSafeByDoctorJSON(t *testing.T) {
	layout := freshInit(t)
	out, err := doctorJSON(t, layout, testDoctorEnv(t))
	if err != nil {
		t.Fatalf("doctor on a fresh init failed: %v\n%v", err, out["checks"])
	}
	si5, ok := out["si5"].(map[string]any)
	if !ok {
		t.Fatalf("no si5 object: %v", out)
	}
	emptyList := func(v any) bool { l, ok := v.([]any); return ok && len(l) == 0 }
	want := map[string]func(any) bool{
		"inbound.policy":              func(v any) bool { return v == "closed" },
		"peers.allow":                 emptyList,
		"peers.trust":                 emptyList,
		"inbound.public_capabilities": emptyList,
		"auto_reply.untrusted":        func(v any) bool { return v == "off" },
		"payments.auto_max":           func(v any) bool { return v == float64(0) },
		"payments.agent_max":          func(v any) bool { return v == float64(0) },
		"payments.agent_daily_max":    func(v any) bool { return v == float64(0) },
		"payments.payees_file":        func(v any) bool { s, _ := v.(string); return s != "" },
		"payments.payees":             emptyList,
	}
	for _, k := range daemon.SI5Keys {
		check, ok := want[k]
		if !ok {
			t.Errorf("SI-5 key %s has no expectation in this test", k)
			continue
		}
		v, present := si5[k]
		if !present || !check(v) {
			t.Errorf("si5[%q] = %#v (present %v): not the safe default", k, v, present)
		}
	}
	if len(si5) != len(daemon.SI5Keys) {
		t.Errorf("si5 has %d keys, SI5Keys %d", len(si5), len(daemon.SI5Keys))
	}
	if !emptyList(out["si5_changed"]) || out["ok"] != true {
		t.Fatalf("si5_changed %v, ok %v", out["si5_changed"], out["ok"])
	}
	// The payee list is on and its file exists, empty.
	if fi, err := os.Stat(filepath.Join(layout.Root, si5["payments.payees_file"].(string))); err != nil || fi.Size() != 0 {
		t.Fatalf("payees file: %v", err)
	}
}

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
		"payments.payees_file":     cfgEdit(func(m map[string]any) { block(m, "payments")["payees_file"] = "" }),
		"payments.payees":          listFile("payees.allow"),
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

// A token file others can read fails the report, and doctor exits non-zero;
// a missing config is a warning with the fix, not a failure.
func TestDoctorFailsOnlyOnRealProblems(t *testing.T) {
	layout := daemon.NewLayout(filepath.Join(t.TempDir(), "none"))
	out, err := doctorJSON(t, layout, testDoctorEnv(t))
	if err != nil || out["ok"] != true {
		t.Fatalf("no config: err %v ok %v", err, out["ok"])
	}
	if _, statErr := os.Stat(layout.Root); !os.IsNotExist(statErr) {
		t.Fatal("doctor created the data directory")
	}

	layout = freshInit(t)
	if err := os.WriteFile(layout.ControlTokenPath(), []byte("t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = doctorJSON(t, layout, testDoctorEnv(t))
	if !errors.Is(err, errQuiet) || out["ok"] != false {
		t.Fatalf("readable token: err %v ok %v", err, out["ok"])
	}
}

// Hermes: a config holding the local A2A token must be 0600, and an
// a2a_agents URL whose port is not a2a_addr.txt's is pointed at
// `anet agents wire --refresh`.
func TestDoctorChecksHermesA2AAgents(t *testing.T) {
	layout := freshInit(t)
	env := testDoctorEnv(t)
	if err := os.WriteFile(filepath.Join(layout.Root, "a2a_addr.txt"), []byte("127.0.0.1:39900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(env.hermesHome, 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "mcp_servers:\n  anet:\n    command: /usr/local/bin/anet\na2a_agents:\n" +
		"  - url: http://127.0.0.1:39900/a2a/v1/agents/bafyreiA\n    auth: {type: bearer, token: x}\n" +
		"  - url: \"http://127.0.0.1:39901/a2a/v1/agents/bafyreiB\"\n"
	cfg := filepath.Join(env.hermesHome, "config.yaml")
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := collectDoctor(layout, env)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rep.Hermes.A2AAgents); n != 2 || !rep.Hermes.A2AAgents[0].Matches || rep.Hermes.A2AAgents[1].Matches {
		t.Fatalf("a2a_agents = %+v", rep.Hermes.A2AAgents)
	}
	var hermesWired, refreshHint bool
	for _, a := range rep.Agents {
		if a.Tool == "hermes" && a.Wired {
			hermesWired = true
		}
	}
	for _, c := range rep.Checks {
		if c.ID == "hermes.a2a_agents" && c.Status == stWarn && c.Hint == "anet agents wire --refresh" {
			refreshHint = true
		}
	}
	if !hermesWired || !refreshHint || !rep.OK {
		t.Fatalf("wired %v refresh hint %v ok %v: %+v", hermesWired, refreshHint, rep.OK, rep.Checks)
	}
	if err := os.Chmod(cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	if rep, _ = collectDoctor(layout, env); rep.OK {
		t.Fatal("a readable Hermes config holding the A2A token did not fail")
	}
}

// sandbox mode without auto_reply.api_key fails closed at run time; doctor
// says so.
func TestDoctorFlagsSandboxWithoutAPIKey(t *testing.T) {
	layout := freshInit(t)
	b, _ := os.ReadFile(layout.ConfigPath())
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["auto_reply"] = map[string]any{"backend": "exec", "agent": "claude", "untrusted": "sandbox"}
	b, _ = json.Marshal(m)
	if err := os.WriteFile(layout.ConfigPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := collectDoctor(layout, testDoctorEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.AutoReply.APIKeyConfigured {
		t.Fatalf("sandbox without api_key: ok %v %+v", rep.OK, rep.AutoReply)
	}
}
