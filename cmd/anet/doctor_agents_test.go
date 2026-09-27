//go:build !no_mcp

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ANetResearch/ANet/internal/agentwire"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/module"
)

const doctorAID = "bafyreiaaaaaaaaaaaaaa"

// wireForDoctor runs `anet agents wire` the way the CLI does, on the fake
// machine doctor is pointed at.
func wireForDoctor(t *testing.T, layout daemon.Layout, env doctorEnv, refresh bool, tool string, aids ...string) {
	t.Helper()
	dataDir, _ := filepath.Abs(layout.Root)
	opts := agentwire.Options{Bin: env.exe, DataDir: dataDir, Home: env.home, Getenv: env.env, LookPath: env.lookPath,
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("no tool CLI is run in this test")
		},
		Now: func() time.Time { return time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC) },
		A2A: aids, Refresh: refresh}
	res, err := agentwire.Wire(opts, []string{tool}, false)
	if err != nil || len(res) != 1 || !res[0].OK() {
		t.Fatalf("wire %s: %v %+v", tool, err, res)
	}
}

func setLocalA2A(t *testing.T, layout daemon.Layout, addr, token string) {
	t.Helper()
	dir := module.StatePath(layout.Root, module.A2AModuleName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{module.A2AAddrFile: addr + "\n", module.A2ATokenFile: token + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func checksOf(rep *doctorReport, id string) []doctorCheck {
	var out []doctorCheck
	for _, c := range rep.Checks {
		if c.ID == id {
			out = append(out, c)
		}
	}
	return out
}

// doctor reports the coding tools as agentwire.Inspect sees them — wired
// and current, wired but not what wire would write now, installed and not
// wired — and the Hermes a2a_agents entries as 0017 Q13 defines their
// expiry: a token that is no longer a2a_token.txt's, or an address that is
// no longer a2a_addr.txt's, both repaired by `anet agents wire --refresh`.
// No token is printed.
func TestDoctorReadsAgentwire(t *testing.T) {
	layout := freshInit(t)
	env := testDoctorEnv(t)
	if err := os.MkdirAll(env.hermesHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	setLocalA2A(t, layout, "127.0.0.1:39900", "tok-current-0123")
	wireForDoctor(t, layout, env, false, agentwire.ToolHermes, doctorAID)

	collect := func() *doctorReport {
		t.Helper()
		rep, err := collectDoctor(layout, env)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	rep := collect()
	tools := map[string]agentWire{}
	for _, a := range rep.Agents {
		tools[a.Tool] = a
	}
	if h := tools["hermes"]; !h.Wired || !h.Current || h.Config != filepath.Join(env.hermesHome, "config.yaml") {
		t.Fatalf("hermes: %+v", h)
	}
	if c := tools["cursor"]; !c.Present || c.Wired {
		t.Fatalf("cursor: %+v", c)
	}
	if cs := checksOf(rep, "agents.cursor"); len(cs) != 1 || cs[0].Status != stInfo || cs[0].Hint != "anet agents wire cursor" {
		t.Fatalf("an installed tool without anet: %+v", cs)
	}
	if n := len(rep.Hermes.A2AAgents); n != 1 || rep.Hermes.A2AAgents[0].AID != doctorAID ||
		!rep.Hermes.A2AAgents[0].Matches || rep.Hermes.A2AAgents[0].Token != "current" || rep.Hermes.A2AToken != "current" {
		t.Fatalf("a2a_agents: %+v (%s)", rep.Hermes.A2AAgents, rep.Hermes.A2AToken)
	}
	if !rep.OK || !rep.Hermes.Config.Private {
		t.Fatalf("ok %v, config %+v", rep.OK, rep.Hermes.Config)
	}

	// The token was replaced (a2a_token.txt deleted and the daemon
	// restarted): the entry has expired.
	setLocalA2A(t, layout, "127.0.0.1:39900", "tok-new-4567")
	rep = collect()
	if rep.Hermes.A2AToken != "stale" || rep.Hermes.A2AAgents[0].Token != "stale" {
		t.Fatalf("a replaced token: %s %+v", rep.Hermes.A2AToken, rep.Hermes.A2AAgents)
	}
	if cs := checksOf(rep, "hermes.a2a_token"); len(cs) != 1 || cs[0].Status != stWarn || cs[0].Hint != "anet agents wire --refresh" {
		t.Fatalf("hermes.a2a_token: %+v", cs)
	}
	if cs := checksOf(rep, "agents.hermes"); len(cs) != 1 || cs[0].Status != stWarn ||
		cs[0].Hint != "anet agents wire hermes --refresh" {
		t.Fatalf("agents.hermes after the token changed: %+v", cs)
	}

	// The port moved too (module/a2a had to take another one).
	setLocalA2A(t, layout, "127.0.0.1:39901", "tok-new-4567")
	rep = collect()
	if cs := checksOf(rep, "hermes.a2a_agents"); len(cs) != 1 || cs[0].Status != stWarn || cs[0].Hint != "anet agents wire --refresh" ||
		!strings.Contains(cs[0].Detail, "39900") {
		t.Fatalf("hermes.a2a_agents after the port moved: %+v", cs)
	}
	var buf bytes.Buffer
	if err := doctorTo(&buf, layout, env, true); err != nil {
		t.Fatal(err)
	}
	if s := buf.String(); strings.Contains(s, "tok-current") || strings.Contains(s, "tok-new") {
		t.Fatal("doctor printed a token")
	}

	// --refresh repairs both.
	wireForDoctor(t, layout, env, true, agentwire.ToolHermes)
	rep = collect()
	if rep.Hermes.A2AToken != "current" || !rep.Hermes.A2AAgents[0].Matches {
		t.Fatalf("after --refresh: %s %+v", rep.Hermes.A2AToken, rep.Hermes.A2AAgents)
	}
	if cs := checksOf(rep, "agents.hermes"); len(cs) != 1 || cs[0].Status != stOK {
		t.Fatalf("agents.hermes after --refresh: %+v", cs)
	}

	// Without a2a_addr.txt and a2a_token.txt nothing can be compared: said,
	// not guessed.
	if err := os.RemoveAll(module.StatePath(layout.Root, module.A2AModuleName)); err != nil {
		t.Fatal(err)
	}
	rep = collect()
	if cs := checksOf(rep, "hermes.a2a_agents"); rep.Hermes.A2AToken != "unknown" || len(cs) != 1 || cs[0].Status != stUnknown {
		t.Fatalf("without the interface's files: %s %+v", rep.Hermes.A2AToken, cs)
	}

	// The file holds the token: readable by others is a failure.
	if err := os.Chmod(filepath.Join(env.hermesHome, "config.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rep = collect(); rep.OK {
		t.Fatal("a readable Hermes config holding the A2A token did not fail")
	}
}

// A wired entry that is not what wire writes now — another binary, say,
// after the install moved — is wired but not current, with the fix.
func TestDoctorSaysWhenAnEntryIsNotCurrent(t *testing.T) {
	layout := freshInit(t)
	env := testDoctorEnv(t)
	if err := os.MkdirAll(filepath.Join(env.home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	wireForDoctor(t, layout, env, false, agentwire.ToolCursor)
	rep, err := collectDoctor(layout, env)
	if err != nil {
		t.Fatal(err)
	}
	if cs := checksOf(rep, "agents.cursor"); len(cs) != 1 || cs[0].Status != stOK {
		t.Fatalf("freshly wired: %+v", cs)
	}
	env.exe = filepath.Join(t.TempDir(), "elsewhere", "anet")
	if rep, err = collectDoctor(layout, env); err != nil {
		t.Fatal(err)
	}
	cs := checksOf(rep, "agents.cursor")
	if len(cs) != 1 || cs[0].Status != stWarn || cs[0].Hint != "anet agents wire cursor" {
		t.Fatalf("wired for another binary: %+v", cs)
	}
	for _, a := range rep.Agents {
		if a.Tool == "cursor" && (!a.Wired || a.Current || len(a.Pending) == 0) {
			t.Fatalf("cursor: %+v", a)
		}
	}
}
