//go:build no_mcp

package main

// The Hermes checks of a -tags no_mcp build, whose doctor reads the
// configuration files instead of asking agentwire (doctor_agents_nomcp.go).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ANetResearch/ANet/internal/anethome"
)

// Hermes: a config holding the local A2A token must be 0600, and an
// a2a_agents URL whose port is not a2a_addr.txt's is pointed at
// `anet agents wire --refresh`.
func TestDoctorChecksHermesA2AAgents(t *testing.T) {
	layout := freshInit(t)
	env := testDoctorEnv(t)
	a2aDir := anethome.A2ADir(layout.Root)
	if err := os.MkdirAll(a2aDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a2aDir, anethome.A2AAddrFile), []byte("127.0.0.1:39900\n"), 0o600); err != nil {
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

// The a2a_agents entries go stale when a2a_token.txt is replaced; doctor
// says so, points at `anet agents wire --refresh`, and prints neither token.
func TestDoctorFindsAStaleHermesA2AToken(t *testing.T) {
	layout := freshInit(t)
	env := testDoctorEnv(t)
	a2aDir := anethome.A2ADir(layout.Root)
	if err := os.MkdirAll(a2aDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{anethome.A2AAddrFile: "127.0.0.1:39900\n", anethome.A2ATokenFile: "tok-current-0123\n"} {
		if err := os.WriteFile(filepath.Join(a2aDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(env.hermesHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(env.hermesHome, "config.yaml")
	write := func(token string) {
		yaml := "a2a_agents:\n  - {url: http://127.0.0.1:39900/a2a/v1/agents/bafyreiA, auth: {type: bearer, token: " +
			token + "}, timeout: 3600}\n"
		if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tok-current-0123")
	rep, err := collectDoctor(layout, env)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hermes.A2AToken != "current" || len(rep.Hermes.A2AAgents) != 1 ||
		rep.Hermes.A2AAgents[0].URL != "http://127.0.0.1:39900/a2a/v1/agents/bafyreiA" || !rep.Hermes.A2AAgents[0].Matches {
		t.Fatalf("current token: %q %+v", rep.Hermes.A2AToken, rep.Hermes.A2AAgents)
	}
	write("tok-old-4567")
	var buf bytes.Buffer
	if err := doctorTo(&buf, layout, env, true); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Hermes struct {
			A2AToken string `json:"a2a_token"`
		} `json:"hermes"`
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	hint := false
	for _, c := range out.Checks {
		if c.ID == "hermes.a2a_token" && c.Status == stWarn && c.Hint == "anet agents wire --refresh" {
			hint = true
		}
	}
	if out.Hermes.A2AToken != "stale" || !hint {
		t.Fatalf("stale token: %q %+v", out.Hermes.A2AToken, out.Checks)
	}
	if s := buf.String(); bytes.Contains([]byte(s), []byte("tok-current")) || bytes.Contains([]byte(s), []byte("tok-old")) {
		t.Fatal("doctor printed a token")
	}
}
