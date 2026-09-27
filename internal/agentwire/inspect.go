//go:build !no_mcp

package agentwire

import (
	"errors"
	"io/fs"
	"net/url"
	"os"
)

// State is what `anet doctor` and `anet agents` report for one tool. It is
// computed by planning a wire and an unwire and making neither.
type State struct {
	Tool     string
	Detected bool
	// Wired: anet's MCP entry is in the tool's configuration.
	Wired bool
	// Current: wire would change nothing (the entry, the guide and, for
	// Hermes, the a2a_agents entries are as wire writes them now).
	Current bool
	// Pending lists what wire would change when not Current.
	Pending []string
	// Conflict is a same-named entry anet does not manage; Err is any
	// other reason the tool cannot be planned.
	Conflict string
	Err      string
	// Config is the file holding the MCP entry, Mode its permission bits
	// (0 when it does not exist).
	Config string
	Mode   fs.FileMode
	// A2A lists Hermes' a2a_agents entries written by anet.
	A2A []A2AState
}

// A2AState is one Hermes a2a_agents entry against the node's current local
// A2A interface. After module/a2a had to move the port, or the token was
// replaced, the entry no longer works; `anet agents wire --refresh` fixes
// both (§11.1, §13.1 doctor).
type A2AState struct {
	AID     string
	URL     string
	PortOK  bool // the URL's host:port is a2a_addr.txt
	TokenOK bool // the token is a2a_token.txt
	// Unknown is set when a2a_addr.txt or a2a_token.txt cannot be read, so
	// neither check means anything.
	Unknown string
}

// Inspect reports every supported tool.
func Inspect(o Options) ([]State, error) {
	o.A2A, o.Refresh = nil, false
	if err := o.normalize(); err != nil {
		return nil, err
	}
	var out []State
	for _, name := range Tools() {
		t, _ := toolByName(name)
		st := State{Tool: name, Detected: t.detected(&o), Config: t.configPath(&o)}
		st.Mode = fileMode(realPath(st.Config))
		changes, err := t.planWire(&o)
		var ce *ConflictError
		switch {
		case errors.As(err, &ce):
			st.Conflict = ce.Error()
		case err != nil:
			st.Err = err.Error()
		default:
			st.Current = len(changes) == 0
			for _, c := range changes {
				st.Pending = append(st.Pending, c.note)
			}
		}
		if un, _, err := t.planUnwire(&o); err == nil {
			for _, c := range un {
				if c.path == st.Config {
					st.Wired = true
				}
			}
		}
		if name == ToolHermes {
			st.A2A = inspectA2A(&o, st.Config)
		}
		out = append(out, st)
	}
	return out, nil
}

func inspectA2A(o *Options, path string) []A2AState {
	cur, err := os.ReadFile(realPath(path))
	if err != nil {
		return nil
	}
	entries, err := hermesA2AEntries(string(cur), path)
	if err != nil || len(entries) == 0 {
		return nil
	}
	ep, eperr := readA2A(o)
	var out []A2AState
	for _, e := range entries {
		s := A2AState{AID: e.aid, URL: e.url}
		if eperr != nil {
			s.Unknown = eperr.Error()
		} else {
			if u, err := url.Parse(e.url); err == nil {
				s.PortOK = u.Host == ep.addr && u.Path == "/a2a/v1/agents/"+e.aid
			}
			s.TokenOK = e.token == ep.token
		}
		out = append(out, s)
	}
	return out
}
