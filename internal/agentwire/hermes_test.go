//go:build !no_mcp

package agentwire

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	aidA = "bafyreiaaaaaaaaaaaaaa"
	aidB = "bafyreibbbbbbbbbbbbbb"
)

// setA2A writes the local A2A interface's state files as module/a2a does.
func (h *host) setA2A(addr, token string) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(a2aStatePath(h.data, A2AAddrFile)), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(a2aStatePath(h.data, A2AAddrFile), []byte(addr+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(a2aStatePath(h.data, A2ATokenFile), []byte(token+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) hermesOpts(refresh bool, aids ...string) Options {
	o := h.opts()
	o.A2A, o.Refresh = aids, refresh
	return o
}

func hermesState(t *testing.T, h *host) State {
	t.Helper()
	states, err := Inspect(h.opts())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if s.Tool == ToolHermes {
			return s
		}
	}
	t.Fatal("no hermes state")
	return State{}
}

// a2a_agents: written only when asked, one entry per AID with the local
// interface's URL and token, the file kept 0600; --refresh follows a port
// or token change; unwire takes the tokens out.
func TestHermesA2AEntries(t *testing.T) {
	h := newHost(t)
	seed := seeds[ToolHermes][".hermes/config.yaml"]
	h.write(".hermes/config.yaml", seed, 0o644)
	h.setA2A("127.0.0.1:39900", "tok-1")

	// Not by default.
	one(t, h.wire(ToolHermes), Written)
	if cfg := h.read(".hermes/config.yaml"); strings.Contains(cfg, "a2a_agents") || strings.Contains(cfg, "tok-1") {
		t.Fatalf("a plain wire wrote a2a_agents:\n%s", cfg)
	}

	// --a2a adds them, and the file is narrowed to 0600.
	one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Written)
	cfg := h.read(".hermes/config.yaml")
	for _, want := range []string{
		"a2a_agents:",
		`  "` + aidA + `":`,
		`    url: "http://127.0.0.1:39900/a2a/v1/agents/` + aidA + `"`,
		"      type: bearer",
		`      token: "tok-1"`,
		"    timeout: 3600",
	} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("config.yaml lacks %q:\n%s", want, cfg)
		}
	}
	if m := h.mode(".hermes/config.yaml"); m != 0o600 {
		t.Fatalf("config.yaml holds the A2A token and is %o, want 0600", m)
	}
	for _, b := range h.backups() {
		if m := h.mode(b); m&0o077 != 0 {
			t.Fatalf("backup %s is %o", b, m)
		}
	}
	if st := hermesState(t, h); len(st.A2A) != 1 || !st.A2A[0].PortOK || !st.A2A[0].TokenOK || !st.Current {
		t.Fatalf("inspect after --a2a: %+v", st)
	}

	// A plain wire leaves them alone; so does --a2a with the same AID.
	one(t, h.wire(ToolHermes), UpToDate)
	one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), UpToDate)

	// The port moved and the token was replaced: doctor sees it, --refresh
	// fixes it.
	h.setA2A("127.0.0.1:39901", "tok-2")
	st := hermesState(t, h)
	if len(st.A2A) != 1 || st.A2A[0].PortOK || st.A2A[0].TokenOK || !st.A2A[0].Stale() || st.Current {
		t.Fatalf("inspect did not notice the change: %+v", st)
	}
	one(t, h.wire(ToolHermes), UpToDate) // without --refresh, nothing
	one(t, h.wireWith(h.hermesOpts(true), ToolHermes), Written)
	cfg = h.read(".hermes/config.yaml")
	if strings.Contains(cfg, "tok-1") || strings.Contains(cfg, ":39900/") || !strings.Contains(cfg, `token: "tok-2"`) ||
		!strings.Contains(cfg, "127.0.0.1:39901/a2a/v1/agents/"+aidA) {
		t.Fatalf("--refresh did not rewrite the entry:\n%s", cfg)
	}
	if st := hermesState(t, h); !st.A2A[0].PortOK || !st.A2A[0].TokenOK || !st.Current {
		t.Fatalf("inspect after refresh: %+v", st)
	}

	// A second agent joins the first.
	one(t, h.wireWith(h.hermesOpts(false, aidB), ToolHermes), Written)
	if st := hermesState(t, h); len(st.A2A) != 2 || st.A2A[0].AID != aidA || st.A2A[1].AID != aidB {
		t.Fatalf("entries: %+v", st.A2A)
	}

	// unwire --a2a removes only the named entry; the file still holds a
	// token, so it is left 0600 even if someone widened it meanwhile.
	if err := os.Chmod(h.p(".hermes/config.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	one(t, h.unwireWith(h.hermesOpts(false, aidA), ToolHermes), Removed)
	cfg = h.read(".hermes/config.yaml")
	if strings.Contains(cfg, aidA) || !strings.Contains(cfg, aidB) || !strings.Contains(cfg, "  anet:\n") {
		t.Fatalf("unwire --a2a %s removed the wrong things:\n%s", aidA, cfg)
	}
	if m := h.mode(".hermes/config.yaml"); m != 0o600 {
		t.Fatalf("config.yaml still holds a token and is %o, want 0600", m)
	}

	// A full unwire takes the MCP entry and every token, and gives back
	// the original text.
	one(t, h.unwire(ToolHermes), Removed)
	cfg = h.read(".hermes/config.yaml")
	if strings.Contains(cfg, "tok-") || strings.Contains(cfg, "a2a_agents") {
		t.Fatalf("unwire left a token behind:\n%s", cfg)
	}
	if cfg != seed {
		t.Fatalf("unwire did not restore config.yaml:\n%s", cfg)
	}
	if h.exists(".hermes/SOUL.md") {
		t.Fatalf("SOUL.md was created by wire and should be gone:\n%s", h.read(".hermes/SOUL.md"))
	}
}

// Without the local A2A interface's state files there is nothing to point
// Hermes at, and the error says why and what to do.
func TestHermesA2AWithoutTheInterfaceFailsClearly(t *testing.T) {
	h := newHost(t)
	r := one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Failed)
	if !strings.Contains(r.Err.Error(), A2AAddrFile) || !strings.Contains(r.Err.Error(), "anet up") {
		t.Fatalf("unclear error: %v", r.Err)
	}
	if h.exists(".hermes/config.yaml") {
		t.Fatal("a failed wire wrote the config")
	}

	h.setA2A("10.0.0.5:39900", "tok")
	r = one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Failed)
	if !strings.Contains(r.Err.Error(), "回环") {
		t.Fatalf("a token must never be written into a non-loopback URL: %v", r.Err)
	}
}

// An existing config that already holds a token and is group-readable is
// narrowed even when nothing else changes.
func TestHermesTokenFileIsNarrowedOnRewire(t *testing.T) {
	h := newHost(t)
	h.setA2A("127.0.0.1:39900", "tok")
	one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Written)
	if err := os.Chmod(h.p(".hermes/config.yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	one(t, h.wire(ToolHermes), Written)
	if m := h.mode(".hermes/config.yaml"); m != 0o600 {
		t.Fatalf("mode %o, want 0600", m)
	}
}

// The forms mcp_servers can take in a Hermes config.
func TestHermesMCPServersForms(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg      string
		conflict bool
	}{
		"absent":             {cfg: "model: x\n"},
		"empty":              {cfg: "mcp_servers:\nmodel: x\n"},
		"empty flow":         {cfg: "model: x\nmcp_servers: {}\n"},
		"null":               {cfg: "mcp_servers: null   # none yet\nmodel: x\n"},
		"four-space":         {cfg: "mcp_servers:\n    fs:\n        command: npx\n"},
		"comment at col 0":   {cfg: "mcp_servers:\n# servers\n  fs:\n    command: npx\ndisplay: {}\n"},
		"quoted key":         {cfg: "\"mcp_servers\":\n  fs:\n    command: npx\n"},
		"unmanaged anet":     {cfg: "mcp_servers:\n  anet:\n    command: anet\n    args: [mcp]\n", conflict: true},
		"unmanaged quoted":   {cfg: "mcp_servers:\n  \"anet\":\n    command: x\n", conflict: true},
		"non-empty flow map": {cfg: "mcp_servers: {fs: {command: npx}}\n", conflict: true},
		// A list is not a map: an anet: key among its items would make the
		// file unreadable, whether the items are indented or not.
		"sequence":          {cfg: "mcp_servers:\n  - fs\nmodel: x\n", conflict: true},
		"sequence at col 0": {cfg: "mcp_servers:\n- fs\nmodel: x\n", conflict: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			h.write(".hermes/config.yaml", tc.cfg, 0o600)
			if tc.conflict {
				one(t, h.wire(ToolHermes), Conflict)
				if h.read(".hermes/config.yaml") != tc.cfg || h.exists(".hermes/SOUL.md") {
					t.Fatal("a conflicted tool must be left entirely alone")
				}
				return
			}
			one(t, h.wire(ToolHermes), Written)
			cfg := h.read(".hermes/config.yaml")
			n := 0
			for _, l := range splitLines(cfg) {
				if strings.HasPrefix(l, "mcp_servers:") || strings.HasPrefix(l, "\"mcp_servers\":") {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("mcp_servers is defined %d times:\n%s", n, cfg)
			}
			if !strings.Contains(cfg, "anet:\n") || !strings.Contains(cfg, `command: "`+testBin+`"`) {
				t.Fatalf("entry missing:\n%s", cfg)
			}
			// The entry sits under mcp_servers, indented like its siblings.
			lines := splitLines(cfg)
			k, _, _ := hermesMCP.topKey(lines)
			end := regionEnd(lines, k)
			found := false
			for j := k + 1; j < end; j++ {
				if strings.TrimSpace(lines[j]) == "anet:" && indentOf(lines[j]) == childIndent(lines, k, end) {
					found = true
				}
			}
			if !found {
				t.Fatalf("anet: is not a child of mcp_servers:\n%s", cfg)
			}
			one(t, h.wire(ToolHermes), UpToDate)
			one(t, h.unwire(ToolHermes), Removed)
			if got := h.read(".hermes/config.yaml"); got != tc.cfg {
				t.Fatalf("round trip:\n--- want\n%s--- got\n%s", tc.cfg, got)
			}
		})
	}
}

// An a2a_agents entry for the same AID that anet did not write is a
// conflict; other operator entries are kept.
func TestHermesA2AConflictAndCoexistence(t *testing.T) {
	h := newHost(t)
	h.setA2A("127.0.0.1:39900", "tok")
	cfg := "a2a_agents:\n  researcher:\n    url: \"http://localhost:9999\"\n  \"" + aidA + "\":\n    url: \"http://x\"\n"
	h.write(".hermes/config.yaml", cfg, 0o600)
	one(t, h.wireWith(h.hermesOpts(false, aidA), ToolHermes), Conflict)
	if h.read(".hermes/config.yaml") != cfg {
		t.Fatal("changed despite the conflict")
	}
	one(t, h.wireWith(h.hermesOpts(false, aidB), ToolHermes), Written)
	got := h.read(".hermes/config.yaml")
	if !strings.Contains(got, "  researcher:\n") || !strings.Contains(got, aidB) {
		t.Fatalf("operator entry lost or anet's missing:\n%s", got)
	}
	one(t, h.unwire(ToolHermes), Removed)
	if got := h.read(".hermes/config.yaml"); got != cfg {
		t.Fatalf("round trip:\n--- want\n%s--- got\n%s", cfg, got)
	}
}

func TestParseA2AAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:39900\n":         "127.0.0.1:39900",
		"http://127.0.0.1:39900/":   "127.0.0.1:39900",
		"[::1]:39900":               "[::1]:39900",
		"localhost:1":               "localhost:1",
		"127.0.0.2:5":               "127.0.0.2:5",
		"0.0.0.0:39900":             "",
		"192.168.1.2:39900":         "",
		"example.com:80":            "",
		"127.0.0.1":                 "",
		"http://127.0.0.1:1/x?y=#z": "",
	} {
		got, err := parseA2AAddr(in)
		if want == "" {
			if err == nil {
				t.Errorf("parseA2AAddr(%q) = %q, want an error", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("parseA2AAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
