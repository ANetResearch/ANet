//go:build !no_mcp

package main

// doctor_agents.go is doctor's view of the coding tools: internal/agentwire's
// Inspect, which plans `anet agents wire` and `unwire` without carrying
// either out, so doctor and wire agree on what "wired" and "current" mean.
// For Hermes it also compares every a2a_agents entry anet wrote with the
// local A2A interface as it is now (0017 Q13): a token that is not
// a2a_token.txt's, or an address that is not a2a_addr.txt's, is what
// `anet agents wire --refresh` repairs. A -tags no_mcp build links no
// agentwire and reads the files instead (doctor_agents_nomcp.go).

import (
	"os/exec"
	"path/filepath"

	"github.com/ANetResearch/ANet/internal/agentwire"
	"github.com/ANetResearch/ANet/internal/daemon"
)

// inspectAgents asks agentwire.Inspect about every supported tool.
func inspectAgents(layout daemon.Layout, env doctorEnv) agentsView {
	var v agentsView
	dataDir, err := filepath.Abs(layout.Root)
	if err != nil {
		v.err = err.Error()
		return v
	}
	// The binary the entries should run: the one wire would write, when
	// this is an installed binary; a `go run` build is compared as it is.
	bin := env.exe
	if bin == "" {
		if bin, err = anetBinary(); err != nil {
			bin = env.binary()
		}
	}
	lookPath := env.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	states, err := agentwire.Inspect(agentwire.Options{Bin: bin, DataDir: dataDir, Home: env.home,
		Getenv: env.env, LookPath: lookPath})
	if err != nil {
		v.err = err.Error()
		return v
	}
	for _, st := range states {
		v.tools = append(v.tools, agentWire{Tool: st.Tool, Config: st.Config, Present: st.Detected || fileExists(st.Config),
			Wired: st.Wired, Current: st.Wired && st.Current, Pending: st.Pending, Conflict: st.Conflict, Error: st.Err,
			Handshake: "not_checked"})
		if st.Tool != agentwire.ToolHermes {
			continue
		}
		v.hermesConfig = st.Config
		for _, a := range st.A2A {
			e := a2aAgentEntry{URL: a.URL, AID: a.AID, Port: urlPort(a.URL), Matches: a.PortOK, Unknown: a.Unknown}
			switch {
			case a.Unknown != "":
				e.Token = "unknown"
			case a.TokenOK:
				e.Token = "current"
			default:
				e.Token = "stale"
			}
			v.a2a = append(v.a2a, e)
		}
	}
	return v
}
