package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readConfigObject(t *testing.T, l Layout) map[string]any {
	t.Helper()
	b, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sub(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object in %v", key, m)
	}
	return v
}

// anet init on an empty data directory writes every SI-5 key explicitly,
// creates the four empty list files 0600, writes no auto_reply and no
// modules.a2a block, and a second run changes nothing (A2A-DESIGN §13.1).
func TestInitWritesTheSafeDefaultsExplicitlyAndIsIdempotent(t *testing.T) {
	l := NewLayout(t.TempDir())
	rep, err := InitLayout(l)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Created || !rep.Wrote || len(rep.Kept) != 0 || len(rep.Problems) != 0 {
		t.Fatalf("first init: %+v", rep)
	}
	cfg := readConfigObject(t, l)
	in, pay := sub(t, cfg, "inbound"), sub(t, cfg, "payments")
	if in["policy"] != PolicyClosed {
		t.Errorf("inbound.policy = %v", in["policy"])
	}
	if caps, ok := in["public_capabilities"].([]any); !ok || len(caps) != 0 {
		t.Errorf("inbound.public_capabilities = %#v, want an explicit []", in["public_capabilities"])
	}
	for _, k := range []string{"auto_max", "agent_max", "agent_daily_max"} {
		if pay[k] != json.Number("0") {
			t.Errorf("payments.%s = %#v, want an explicit 0", k, pay[k])
		}
	}
	if pay["payees_file"] != defaultPayeesFile {
		t.Errorf("payments.payees_file = %v", pay["payees_file"])
	}
	if _, ok := cfg["auto_reply"]; ok {
		t.Error("init wrote an auto_reply block, which turns the auto-reply loop on")
	}
	if mods, ok := cfg["modules"].(map[string]any); ok && mods["a2a"] != nil {
		t.Error("init wrote modules.a2a")
	}
	for _, name := range []string{"peers.allow", "peers.deny", "peers.trust", "payees.allow"} {
		fi, err := os.Stat(filepath.Join(l.Root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: size %d mode %v", name, fi.Size(), fi.Mode().Perm())
		}
	}
	before, _ := os.ReadFile(l.ConfigPath())
	rep, err = InitLayout(l)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(l.ConfigPath())
	if rep.Wrote || string(before) != string(after) {
		t.Fatalf("second init rewrote the config: %+v", rep)
	}
	for _, c := range rep.Changes {
		if c.Action != "kept" {
			t.Errorf("second init: %+v", c)
		}
	}
	st, err := ReadPolicy(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed := st.SI5(); len(changed) != 0 {
		t.Fatalf("after init SI-5 differs at %v", changed)
	}
}

// On an existing config init adds what is missing and nothing else: an
// operator's values (even unsafe ones), unknown keys and the list files'
// contents stay; the wire-1 accept_delegations key goes; what differs from a
// fresh install is reported.
func TestInitOnlyAddsMissingKeys(t *testing.T) {
	root := t.TempDir()
	l := NewLayout(root)
	const aid = "bafyreiallowedpeer0001"
	orig := `{
  "control_addr": "127.0.0.1:40001",
  "hub_url": "http://hub.example",
  "accept_delegations": true,
  "inbound": {"policy": "open", "allow_file": "my.allow"},
  "payments": {"auto_max": 7},
  "auto_reply": {"backend": "openai", "api_base": "http://127.0.0.1:1", "model": "m"},
  "future_key": {"x": 1}
}`
	if err := os.WriteFile(l.ConfigPath(), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "my.allow"), []byte(aid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := InitLayout(l)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Created || !rep.Wrote {
		t.Fatalf("report %+v", rep)
	}
	cfg := readConfigObject(t, l)
	in, pay, ar := sub(t, cfg, "inbound"), sub(t, cfg, "payments"), sub(t, cfg, "auto_reply")
	checks := map[string][2]any{
		"control_addr":          {cfg["control_addr"], "127.0.0.1:40001"},
		"hub_url":               {cfg["hub_url"], "http://hub.example"},
		"inbound.policy":        {in["policy"], PolicyOpen},
		"inbound.allow_file":    {in["allow_file"], "my.allow"},
		"inbound.deny_file":     {in["deny_file"], defaultDenyFile},
		"payments.auto_max":     {pay["auto_max"], json.Number("7")},
		"payments.agent_max":    {pay["agent_max"], json.Number("0")},
		"payments.explicit_max": {pay["explicit_max"], json.Number("10")},
		"payments.payees_file":  {pay["payees_file"], defaultPayeesFile},
		"auto_reply.untrusted":  {ar["untrusted"], UntrustedOff},
		"auto_reply.model":      {ar["model"], "m"},
	}
	for k, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %#v, want %#v", k, v[0], v[1])
		}
	}
	if _, ok := cfg["accept_delegations"]; ok {
		t.Error("accept_delegations kept")
	}
	if fk, ok := cfg["future_key"].(map[string]any); !ok || fk["x"] != json.Number("1") {
		t.Errorf("unknown key lost: %#v", cfg["future_key"])
	}
	if b, _ := os.ReadFile(filepath.Join(root, "my.allow")); string(b) != aid+"\n" {
		t.Errorf("allow list changed: %q", b)
	}
	if fi, _ := os.Stat(l.ConfigPath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v", fi.Mode().Perm())
	}
	kept := map[string]bool{}
	for _, k := range rep.Kept {
		kept[k.Key] = true
	}
	for _, k := range []string{"inbound.policy", "peers.allow", "payments.auto_max"} {
		if !kept[k] {
			t.Errorf("init did not report %s as differing from a fresh install: %+v", k, rep.Kept)
		}
	}
	added := map[string]bool{}
	for _, c := range rep.Changes {
		if c.Action == "added" || c.Action == "removed" {
			added[c.Key] = true
		}
	}
	for _, k := range []string{"inbound.deny_file", "inbound.public_capabilities", "payments.explicit_max",
		"auto_reply.untrusted", "accept_delegations"} {
		if !added[k] {
			t.Errorf("change %s not reported: %+v", k, rep.Changes)
		}
	}
	if added["inbound.policy"] || added["payments.auto_max"] || added["control_addr"] {
		t.Errorf("an existing key was reported as added: %+v", rep.Changes)
	}
	// The completed config is one the daemon loads.
	if _, err := LoadConfig(l); err != nil {
		t.Fatal(err)
	}
}

// A config that does not parse is refused and left exactly as it was.
func TestInitRefusesAMalformedConfig(t *testing.T) {
	l := NewLayout(t.TempDir())
	bad := []byte(`{"inbound": {"policy": "closed",}`)
	if err := os.WriteFile(l.ConfigPath(), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitLayout(l); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("init on a malformed config: %v", err)
	}
	if b, _ := os.ReadFile(l.ConfigPath()); string(b) != string(bad) {
		t.Fatal("malformed config was rewritten")
	}
}

// ReadPolicy creates nothing: doctor must not be the thing that makes a
// data directory.
func TestReadPolicyCreatesNothing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	st, err := ReadPolicy(NewLayout(root))
	if err != nil {
		t.Fatal(err)
	}
	if st.ConfigPresent {
		t.Fatal("config reported present")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("ReadPolicy created %s", root)
	}
	if _, changed := st.SI5(); len(changed) != 0 {
		t.Fatalf("defaults differ from SI-5 at %v", changed)
	}
}
