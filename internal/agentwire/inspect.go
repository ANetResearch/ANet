//go:build !no_mcp

package agentwire

import (
	"errors"
	"io/fs"
	"net/url"
	"os"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// State is what `anet doctor` and `anet agents` report for one tool. It is
// computed by planning a wire and an unwire and making neither.
type State struct {
	Tool     string
	Detected bool
	// Wired: anet's MCP entry is in the tool's configuration.
	Wired bool
	// Current: wire would change nothing (the entry and the guide are as
	// wire writes them now), and for Hermes no a2a_agents entry is Stale
	// (that one takes wire --refresh).
	Current bool
	// Pending lists what wire would change when not Current, in the
	// language of Options.Lang.
	Pending []string
	// Conflict is a same-named entry anet does not manage; Err is any
	// other reason the tool cannot be planned. Both are sentences in the
	// language of Options.Lang.
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
	// Unknown is set (as a sentence in the language of Options.Lang) when
	// a2a_addr.txt or a2a_token.txt cannot be read, so neither check means
	// anything.
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
			st.Conflict = ce.Text(o.Lang)
		case err != nil:
			st.Err = errText(err, o.Lang)
		default:
			st.Current = len(changes) == 0
			for _, c := range changes {
				st.Pending = append(st.Pending, c.note.Text(o.Lang))
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
			// Planning ran without --refresh, so entries pointing at an old
			// port or token planned as unchanged; they are not current.
			for _, a := range st.A2A {
				if a.Stale() && st.Conflict == "" && st.Err == "" {
					st.Current = false
					st.Pending = append(st.Pending, msg("wire.pending.a2a_stale").Text(o.Lang))
					break
				}
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// Stale reports that the entry no longer reaches this node: its port or
// token is not the current one. An entry that could not be checked is not
// stale, only unknown.
func (a A2AState) Stale() bool { return a.Unknown == "" && (!a.PortOK || !a.TokenOK) }

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
			s.Unknown = errText(eperr, o.Lang)
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

// errText is an error's sentence in a language: the sentence of the reason
// row for this package's own errors, the error's own words for the rest.
func errText(err error, lang a2ashape.Language) string {
	if m, ok := AsMsg(err); ok {
		return m.Text(lang)
	}
	return err.Error()
}
