//go:build !no_mcp

package agentwire

// Fuzz targets for wire and unwire over configuration files anet did not
// write (docs/notes/0033). The property is the one the package comment
// promises: a file the tool could read before, it can still read after,
// with anet's entry where the tool looks for it.
//
// TOML and YAML are edited as text, without a parser (see the package
// comment), so a parser is the oracle here: Python's tomllib (the TOML 1.0
// reference behaviour; Codex reads config.toml with a TOML 1.0 parser) and
// PyYAML's safe_load (what Hermes, a Python program, reads config.yaml
// with). The test drives one python3 process for the whole run. Without
// python3, tomllib and PyYAML the parser checks are skipped and the rest
// still runs; the fuzz runs recorded in docs/notes/0033 had them.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const oracleScript = `
import sys, struct, json
import tomllib, yaml
inp, out = sys.stdin.buffer, sys.stdout.buffer
def pick(doc, *path):
    for p in path:
        if not isinstance(doc, dict) or p not in doc:
            return None
        doc = doc[p]
    return doc
while True:
    h = inp.read(5)
    if len(h) < 5:
        break
    n = struct.unpack('>I', h[1:5])[0]
    data = inp.read(n)
    res = {"ok": False}
    try:
        text = data.decode('utf-8')
        if h[0:1] == b't':
            doc = tomllib.loads(text)
            res["anet"] = json.loads(json.dumps(pick(doc, "mcp_servers", "anet"), default=str))
        else:
            doc = yaml.safe_load(text)
            res["anet"] = json.loads(json.dumps(pick(doc, "mcp_servers", "anet"), default=str))
            a2a = pick(doc, "a2a_agents")
            if isinstance(a2a, dict):
                res["a2a"] = {str(k): json.loads(json.dumps(v, default=str)) for k, v in a2a.items()}
        res["ok"] = True
    except BaseException as e:
        if isinstance(e, KeyboardInterrupt):
            raise
        res = {"ok": False, "err": type(e).__name__ + ": " + str(e)[:300]}
    b = json.dumps(res).encode()
    out.write(struct.pack('>I', len(b)) + b)
    out.flush()
`

// parsed is what the oracle read from one file.
type parsed struct {
	OK   bool           `json:"ok"`
	Err  string         `json:"err"`
	Anet any            `json:"anet"` // mcp_servers.anet: whatever the file makes it
	A2A  map[string]any `json:"a2a"`
}

type oracle struct {
	mu  sync.Mutex
	in  io.WriteCloser
	out *bufio.Reader
}

var (
	oracleOnce sync.Once
	theOracle  *oracle
)

// parserOracle starts the python3 oracle, or returns nil when this machine
// has no python3 with tomllib and PyYAML.
func parserOracle(tb testing.TB) *oracle {
	oracleOnce.Do(func() {
		py, err := exec.LookPath("python3")
		if err != nil {
			return
		}
		if exec.Command(py, "-c", "import tomllib, yaml").Run() != nil {
			return
		}
		cmd := exec.Command(py, "-c", oracleScript)
		in, err := cmd.StdinPipe()
		if err != nil {
			return
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		cmd.Stderr = os.Stderr
		if cmd.Start() != nil {
			return
		}
		theOracle = &oracle{in: in, out: bufio.NewReader(out)}
	})
	if theOracle == nil {
		tb.Log("python3 with tomllib and PyYAML not found: the parser checks are skipped")
	}
	return theOracle
}

// parse asks the oracle to read text as TOML (kind 't') or YAML ('y').
func (o *oracle) parse(t *testing.T, kind byte, text string) parsed {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	hdr := make([]byte, 5)
	hdr[0] = kind
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(text)))
	if _, err := o.in.Write(append(hdr, text...)); err != nil {
		t.Fatalf("oracle: %v", err)
	}
	var n [4]byte
	if _, err := io.ReadFull(o.out, n[:]); err != nil {
		t.Fatalf("oracle: %v", err)
	}
	b := make([]byte, binary.BigEndian.Uint32(n[:]))
	if _, err := io.ReadFull(o.out, b); err != nil {
		t.Fatalf("oracle: %v", err)
	}
	var p parsed
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("oracle: %v: %s", err, b)
	}
	return p
}

// fuzzHost is one fake home for a fuzz worker: the file under test is
// rewritten in place for every input.
type fuzzHost struct {
	o Options
}

func newFuzzHost(f *testing.F) *fuzzHost {
	root := f.TempDir()
	home := filepath.Join(root, "home")
	env := map[string]string{}
	// DataDir is a fixed path, not under the temporary directory: it is
	// written into the seeds, and a fuzz worker has a temporary directory
	// of its own.
	return &fuzzHost{o: Options{
		Bin: testBin, DataDir: "/home/fuzz/.anet", Home: home,
		Getenv:   func(k string) string { return env[k] },
		LookPath: func(string) (string, error) { return "", os.ErrNotExist },
	}}
}

// put makes path hold exactly src ("" with absent: no file).
func (h *fuzzHost) put(t *testing.T, path, src string, absent bool) {
	t.Helper()
	_ = os.Remove(path)
	if absent {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// changeTo is what changes leave in path: its new content, whether it is
// deleted, and whether any change touches it.
func changeTo(changes []change, path string) (after string, del, touched bool) {
	for _, c := range changes {
		if c.path == path && !c.chmodOnly {
			return string(c.after), c.del, true
		}
	}
	return "", false, false
}

// wantCodexEntry reports whether the oracle's mcp_servers.anet is the
// entry codexBlock writes.
func wantCodexEntry(o *Options, entry any) bool {
	got, _ := entry.(map[string]any)
	env, _ := got["env"].(map[string]any)
	args, _ := got["args"].([]any)
	return got["command"] == o.Bin && len(args) == 1 && args[0] == "mcp" && env["ANET_DATA_DIR"] == o.DataDir
}

var codexSeeds = []string{
	"",
	"model = \"gpt-5\"\n\n[mcp_servers.docs]\ncommand = \"docs-mcp\"\nargs = []\n",
	"[mcp_servers.anet]\ncommand = \"anet\"\nargs = [\"mcp\"]\n",
	"[mcp_servers.\"anet\"]\ncommand = \"x\"\n",
	"[ mcp_servers . anet ]\ncommand = \"x\"\n",
	"[mcp_servers.anet.env]\nA = \"b\"\n",
	"model = \"m\"\nmcp_servers.anet.command = \"x\"\n",
	"[mcp_servers]\nanet = { command = \"x\" }\n",
	"[mcp_servers]\nanet.command = \"x\"\n",
	"mcp_servers = { anet = { command = \"x\" } }\n",
	"model = \"m\"\nmcp_servers = { docs = { command = \"docs-mcp\" } }\n",
	"mcp_servers.docs.command = \"docs-mcp\"\n",
	"[mcp_servers.anetwork]\ncommand = \"x\"\n",
	"# [mcp_servers.anet]\nmodel = \"m\"\n",
	"instructions = \"\"\"\n[mcp_servers.anet]\n\"\"\"\n",
	"instructions = '''\n[mcp_servers.anet]\n'''\n",
	"[profiles.anet]\nmodel = \"m\"\n",
	"[[profiles]]\nname = \"a\"\n",
	"model = \"m\"\r\n[tui]\r\nx = 1\r\n",
	"a = \"no newline at the end\"",
	// Real Codex configurations: literal strings, trailing comments,
	// quoted keys (projects are keyed by path).
	"model_instructions = 'answer in \"plain\" words'\n[mcp_servers.docs]\ncommand = \"d\"\n",
	"notify = [\"notify-send\", \"Codex\"] # desktop notice\n",
	"[projects.\"/home/u/src\"]\ntrust_level = \"trusted\"\n",
	"[mcp_servers.\"docs\\u002dv2\"]\ncommand = \"d\"\n",
	"[shell_environment_policy]\ninherit = \"core\"\nexclude = [\"AWS_*\"]\n",
}

// FuzzCodexConfig: wire over any config.toml Codex can read leaves one it
// can still read, with mcp_servers.anet being anet's entry; wiring again
// changes nothing; unwire leaves a readable file.
func FuzzCodexConfig(f *testing.F) {
	for _, s := range codexSeeds {
		f.Add(s)
	}
	blk := joinLines(codexBlock(&Options{Bin: testBin, DataDir: "/home/u/.anet"}))
	f.Add("model = \"m\"\n\n" + blk)
	f.Add(blk + "[mcp_servers.anet]\ncommand = \"x\"\n")
	h := newFuzzHost(f)
	orc := parserOracle(f)
	f.Fuzz(func(t *testing.T, src string) {
		o := &h.o
		path := codexConfig(o)
		h.put(t, path, src, false)
		before := parsed{}
		if orc != nil {
			before = orc.parse(t, 't', src)
		}
		changes, err := codexTool{}.planWire(o)
		if err != nil {
			return
		}
		after, del, touched := changeTo(changes, path)
		if !touched {
			after = src
		}
		if del {
			t.Fatal("wire deletes config.toml")
		}
		if before.OK {
			p := orc.parse(t, 't', after)
			if !p.OK {
				t.Fatalf("wire made a readable config.toml unreadable (%s)\n--- before\n%s\n--- after\n%s", p.Err, src, after)
			}
			if !wantCodexEntry(o, p.Anet) {
				t.Fatalf("after wire, mcp_servers.anet is %v\n--- before\n%s\n--- after\n%s", p.Anet, src, after)
			}
		}
		// Wiring again changes nothing.
		h.put(t, path, after, false)
		again, err := codexTool{}.planWire(o)
		if err != nil {
			t.Fatalf("second wire: %v\n--- file\n%s", err, after)
		}
		if a2, _, ok := changeTo(again, path); ok && a2 != after {
			t.Fatalf("second wire changed config.toml\n--- first\n%s\n--- second\n%s", after, a2)
		}
		// Unwire takes the block out and leaves a readable file.
		un, _, err := codexTool{}.planUnwire(o)
		if err != nil {
			t.Fatalf("unwire: %v\n--- file\n%s", err, after)
		}
		rest, del, ok := changeTo(un, path)
		if !ok {
			t.Fatalf("unwire found nothing to remove\n--- file\n%s", after)
		}
		if before.OK && !del {
			if p := orc.parse(t, 't', rest); !p.OK {
				t.Fatalf("unwire left an unreadable config.toml (%s)\n--- wired\n%s\n--- unwired\n%s", p.Err, after, rest)
			}
		}
	})
}

var hermesSeeds = []string{
	"",
	"model:\n  default: claude-sonnet\n\nmcp_servers:\n  filesystem:\n    command: \"npx\"\n" +
		"    args: [\"-y\", \"@modelcontextprotocol/server-filesystem\", \"/tmp\"]\n\ndisplay:\n  compact: true\n",
	"model: x\n",
	"mcp_servers:\nmodel: x\n",
	"model: x\nmcp_servers: {}\n",
	"mcp_servers: null   # none yet\nmodel: x\n",
	"mcp_servers:\n    fs:\n        command: npx\n",
	"mcp_servers:\n# servers\n  fs:\n    command: npx\ndisplay: {}\n",
	"\"mcp_servers\":\n  fs:\n    command: npx\n",
	"mcp_servers:\n  anet:\n    command: anet\n    args: [mcp]\n",
	"mcp_servers:\n  \"anet\":\n    command: x\n",
	"mcp_servers: {fs: {command: npx}}\n",
	"mcp_servers:\n  - fs\nmodel: x\n",
	"mcp_servers:\n- fs\nmodel: x\n",
	"a2a_agents:\n  researcher:\n    url: \"http://localhost:9999\"\n  \"" + fuzzAID + "\":\n    url: \"http://x\"\n",
	"a2a_agents:\n  researcher:\n    url: \"http://localhost:9999\"\n",
	"prompt: |\n  mcp_servers:\n    anet: 1\nmodel: x\n",
	"---\nmodel: x\n...\n",
	"model: x\r\nmcp_servers:\r\n  fs:\r\n    command: npx\r\n",
}

const (
	fuzzAID   = "bafyreiabcdefgh234567"
	fuzzToken = "tok-fuzz"
)

// FuzzHermesConfig: the same property for Hermes' config.yaml, through
// both of anet's blocks: mcp_servers.anet (wire) and an a2a_agents entry
// (wire --a2a). Unwire of either leaves a readable file.
func FuzzHermesConfig(f *testing.F) {
	for _, s := range hermesSeeds {
		f.Add(s)
	}
	h := newFuzzHost(f)
	orc := parserOracle(f)
	ep := a2aEndpoint{addr: "127.0.0.1:39900", token: fuzzToken}
	f.Fuzz(func(t *testing.T, src string) {
		o := &h.o
		const path = "config.yaml"
		before := parsed{}
		if orc != nil {
			before = orc.parse(t, 'y', src)
		}
		out, err := hermesMCP.set(src, path, hermesMCPBody(o), []string{"anet"})
		if err != nil {
			return
		}
		if before.OK {
			p := orc.parse(t, 'y', out)
			if !p.OK {
				t.Fatalf("wire made a readable config.yaml unreadable (%s)\n--- before\n%s\n--- after\n%s", p.Err, src, out)
			}
			if !wantCodexEntry(o, p.Anet) {
				t.Fatalf("after wire, mcp_servers.anet is %v\n--- before\n%s\n--- after\n%s", p.Anet, src, out)
			}
		}
		if again, err := hermesMCP.set(out, path, hermesMCPBody(o), []string{"anet"}); err != nil || again != out {
			t.Fatalf("second wire: %v\n--- first\n%s\n--- second\n%s", err, out, again)
		}
		existing, err := hermesA2AEntries(out, path)
		if err != nil {
			return
		}
		aids := []string{}
		for _, e := range existing {
			aids = append(aids, e.aid)
		}
		var added []string
		if !contains(aids, fuzzAID) {
			aids, added = append(aids, fuzzAID), []string{fuzzAID}
		}
		out2, err := hermesA2A.set(out, path, hermesA2ABody(ep, aids), added)
		if err == nil && before.OK {
			p := orc.parse(t, 'y', out2)
			if !p.OK {
				t.Fatalf("wire --a2a made a readable config.yaml unreadable (%s)\n--- before\n%s\n--- after\n%s", p.Err, out, out2)
			}
			e, _ := p.A2A[fuzzAID].(map[string]any)
			auth, _ := e["auth"].(map[string]any)
			if e["url"] != a2aURL(ep.addr, fuzzAID) || auth["token"] != fuzzToken {
				t.Fatalf("after wire --a2a, a2a_agents[%s] is %v\n--- before\n%s\n--- after\n%s", fuzzAID, e, out, out2)
			}
			if !wantCodexEntry(o, p.Anet) {
				t.Fatalf("wire --a2a lost mcp_servers.anet (%v)\n--- before\n%s\n--- after\n%s", p.Anet, out, out2)
			}
		}
		if err != nil {
			out2 = out
		}
		// unwire: both blocks, then the file must still read.
		rest, _, err := hermesMCP.drop(out2, path)
		if err != nil {
			t.Fatalf("unwire mcp: %v\n--- file\n%s", err, out2)
		}
		rest, _, err = hermesA2A.drop(rest, path)
		if err != nil {
			t.Fatalf("unwire a2a: %v\n--- file\n%s", err, out2)
		}
		if before.OK {
			if p := orc.parse(t, 'y', rest); !p.OK {
				t.Fatalf("unwire left an unreadable config.yaml (%s)\n--- wired\n%s\n--- unwired\n%s", p.Err, out2, rest)
			}
		}
	})
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// FuzzJSONConfig: Cursor's mcp.json and opencode's opencode.json. The tools
// read them with JavaScript's JSON.parse — exact member names, the last of
// two same-named members wins — which is what a Go map decode does too. A
// file that parses as an object before wire parses after it with anet's
// entry under the container; wire reports nothing to do only when that
// entry is already there as the tool reads it.
func FuzzJSONConfig(f *testing.F) {
	f.Add([]byte(""), false, uint16(0))
	f.Add([]byte("{\n  \"mcpServers\": {\n    \"other\": {\n      \"command\": \"other-mcp\",\n      \"args\": []\n    }\n  }\n}\n"), false, uint16(0))
	f.Add([]byte("{\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"theme\": \"tokyonight\",\n"+
		"  \"mcp\": {\n    \"other\": {\n      \"type\": \"remote\",\n      \"url\": \"https://example.com/mcp\"\n    }\n  }\n}\n"), true, uint16(0))
	f.Add([]byte(`{"mcpServers":{"anet":{"command":"/opt/anet/bin/anet","args":["mcp"],"env":{"ANET_DATA_DIR":"/x"}}}}`), false, uint16(0))
	f.Add([]byte(`{"mcp":{"anet":{"type":"local","command":["/opt/anet/bin/anet","mcp"],"enabled":true}}}`), true, uint16(0))
	f.Add([]byte("{\"mcpServers\":null}"), false, uint16(0))
	f.Add([]byte("{\"a\":1}\n// comment\n"), true, uint16(0))
	h := newFuzzHost(f)
	// The entries as wire writes them for this host: the up-to-date path.
	cur, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"anet": cursorTool{}.server(&h.o).want}})
	oc, _ := json.Marshal(map[string]any{"mcp": map[string]any{"anet": opencodeTool{}.server(&h.o).want}})
	f.Add(cur, false, uint16(0))
	f.Add(oc, true, uint16(0))
	f.Fuzz(func(t *testing.T, src []byte, opencode bool, flip uint16) {
		// A knob the mutator turns cheaply: the case of one letter of the
		// file ("command" to "Command"), which byte mutations rarely hit
		// and which adds no coverage for the fuzzer to keep.
		src = flipCase(src, flip)
		o := &h.o
		var s jsonServer
		if opencode {
			s = opencodeTool{}.server(o)
		} else {
			s = cursorTool{}.server(o)
		}
		h.put(t, s.path, string(src), false)
		want, err := json.Marshal(s.want)
		if err != nil {
			t.Fatal(err)
		}
		var beforeDoc map[string]any
		readable := len(strings.TrimSpace(string(src))) == 0 || json.Unmarshal(src, &beforeDoc) == nil && beforeDoc != nil
		c, err := s.planWire(o)
		if err != nil {
			return
		}
		after := src
		if c != nil {
			after = c.after
		}
		if !readable {
			return
		}
		var doc map[string]any
		if err := json.Unmarshal(after, &doc); err != nil {
			t.Fatalf("wire made a readable file unreadable (%v)\n--- before\n%s\n--- after\n%s", err, src, after)
		}
		servers, _ := doc[s.container].(map[string]any)
		got, _ := servers["anet"].(map[string]any)
		if !jsEntryOK(o, opencode, got) {
			t.Fatalf("after wire (changed=%v), %s.anet reads as %v, want %s\n--- before\n%s\n--- after\n%s",
				c != nil, s.container, servers["anet"], want, src, after)
		}
	})
}

// jsEntryOK reports whether e, read with exact member names, is the entry
// wire writes for the tool: what the tool itself will find there.
func jsEntryOK(o *Options, opencode bool, e map[string]any) bool {
	env := "env"
	if opencode {
		env = "environment"
	}
	envOK := true
	m, _ := e[env].(map[string]any)
	want := mcpEnv(o)
	if len(m) != len(want) {
		envOK = false
	}
	for k, v := range want {
		if m[k] != v {
			envOK = false
		}
	}
	if opencode {
		cmd, _ := e["command"].([]any)
		return e["type"] == "local" && len(cmd) == 2 && cmd[0] == o.Bin && cmd[1] == "mcp" && e["enabled"] == true && envOK
	}
	args, _ := e["args"].([]any)
	return e["command"] == o.Bin && len(args) == 1 && args[0] == "mcp" && envOK
}

// flipCase changes the case of the ASCII letter at i-1 (0: none) of b, in
// a copy.
func flipCase(b []byte, i uint16) []byte {
	if i == 0 || int(i) > len(b) {
		return b
	}
	c := b[i-1]
	if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
		b = append([]byte(nil), b...)
		b[i-1] = c ^ 0x20
	}
	return b
}
