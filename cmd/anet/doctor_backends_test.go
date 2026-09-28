package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/daemon"
)

// doctor reads modules.a2a.backends (A2A-DESIGN §11.6): a backend that
// serves untrusted peers is a warning; the two combinations the daemon
// refuses to start with are failures; a trusted-only backend says what it
// will receive, and a configuration without a match "*" backend that it
// receives nothing.
func TestDoctorA2ABackends(t *testing.T) {
	const peer = "bafyreisomepeer000001"
	cases := []struct {
		name    string
		backend map[string]any
		policy  string
		trust   bool
		status  string
	}{
		{"untrusted and toolless", map[string]any{"accept_untrusted": true, "toolless": true}, "approve", false, stWarn},
		{"untrusted with tools", map[string]any{"accept_untrusted": true}, "closed", false, stFail},
		{"untrusted under open", map[string]any{"accept_untrusted": true, "toolless": true}, "open", false, stFail},
		{"trusted only, nobody trusted", map[string]any{}, "closed", false, stInfo},
		{"trusted only", map[string]any{}, "closed", true, stOK},
		{"no catch-all backend", map[string]any{"match": "text.summarize"}, "closed", true, stInfo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			layout := freshInit(t)
			b, err := os.ReadFile(layout.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			be := map[string]any{"match": "*", "url": "http://127.0.0.1:9900", "allow_tcp": true}
			for k, v := range c.backend {
				be[k] = v
			}
			m["modules"] = map[string]any{"a2a": map[string]any{"backends": []any{be}}}
			m["inbound"].(map[string]any)["policy"] = c.policy
			b, _ = json.Marshal(m)
			if err := os.WriteFile(layout.ConfigPath(), b, 0o600); err != nil {
				t.Fatal(err)
			}
			if c.trust {
				if err := os.WriteFile(filepath.Join(layout.Root, "peers.trust"), []byte(peer+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, _ := doctorJSON(t, layout, testDoctorEnv(t))
			var got []string
			for _, raw := range out["checks"].([]any) {
				ch := raw.(map[string]any)
				if ch["id"] == "a2a.backends" {
					got = append(got, ch["status"].(string))
				}
			}
			if len(got) != 1 || got[0] != c.status {
				t.Fatalf("a2a.backends checks %v, want one %s", got, c.status)
			}
			if wantOK := c.status != stFail; out["ok"] != wantOK {
				t.Fatalf("ok = %v, want %v", out["ok"], wantOK)
			}
		})
	}
}

// doctor reads how the daemon reaches each backend (docs/notes/0030 N1): a
// TCP URL without allow_tcp is a fail (the daemon refuses to start), with it
// a warning; a socket whose directory others can write is a fail, a socket
// in a private directory ok, one that is not there a warning.
func TestDoctorBackendTransport(t *testing.T) {
	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(private, "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	openSock := filepath.Join(open, "b.sock")
	ln2, err := net.Listen("unix", openSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	cases := []struct {
		name    string
		modules map[string]any
		status  string
	}{
		{"service over TCP without allow_tcp", map[string]any{"service": map[string]any{"capabilities": []any{
			map[string]any{"id": "x", "url": "http://127.0.0.1:8080/x"}}}}, stFail},
		{"service over TCP with allow_tcp", map[string]any{"service": map[string]any{"allow_tcp": true, "capabilities": []any{
			map[string]any{"id": "x", "url": "http://127.0.0.1:8080/x"}, map[string]any{"id": "y", "url": "http://127.0.0.1:8080/y"}}}}, stWarn},
		{"service on a private socket", map[string]any{"service": map[string]any{"capabilities": []any{
			map[string]any{"id": "x", "url": "unix://" + sock + ":/x"}, map[string]any{"id": "y", "url": "unix://" + sock + ":/y"}}}}, stOK},
		{"service on a socket in a world-writable directory", map[string]any{"service": map[string]any{"capabilities": []any{
			map[string]any{"id": "x", "url": "unix://" + openSock}}}}, stFail},
		{"service on a socket that is not there", map[string]any{"service": map[string]any{"capabilities": []any{
			map[string]any{"id": "x", "url": "unix://" + filepath.Join(private, "gone.sock")}}}}, stWarn},
		{"A2A backend over TCP without allow_tcp", map[string]any{"a2a": map[string]any{"backends": []any{
			map[string]any{"match": "*", "url": "http://127.0.0.1:9900"}}}}, stFail},
		{"A2A backend on a private socket", map[string]any{"a2a": map[string]any{"backends": []any{
			map[string]any{"match": "*", "url": "unix://" + sock}}}}, stOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			layout := freshInit(t)
			b, err := os.ReadFile(layout.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			m["modules"] = c.modules
			b, _ = json.Marshal(m)
			if err := os.WriteFile(layout.ConfigPath(), b, 0o600); err != nil {
				t.Fatal(err)
			}
			out, _ := doctorJSON(t, layout, testDoctorEnv(t))
			var got []string
			for _, raw := range out["checks"].([]any) {
				ch := raw.(map[string]any)
				if ch["id"] == "backend.transport" {
					got = append(got, ch["status"].(string)+": "+ch["detail"].(string))
				}
			}
			// One line per socket or TCP host, however many capabilities share it.
			if len(got) != 1 || !strings.HasPrefix(got[0], c.status+": ") {
				t.Fatalf("backend.transport checks %q, want one %s", got, c.status)
			}
		})
	}
}

// setConfigKeys rewrites layout's config.json with each key set to its
// value (nil removes it).
func setConfigKeys(t *testing.T, layout daemon.Layout, kv map[string]any) {
	t.Helper()
	b, err := os.ReadFile(layout.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	b, _ = json.Marshal(m)
	if err := os.WriteFile(layout.ConfigPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkStatus is the status of doctor's check id in out ("" when absent).
func checkStatus(out map[string]any, id string) string {
	checks, _ := out["checks"].([]any)
	for _, c := range checks {
		m, _ := c.(map[string]any)
		if m["id"] == id {
			s, _ := m["status"].(string)
			return s
		}
	}
	return ""
}

// doctor says what no_response_after does (A2A-DESIGN §4.2): a fresh init
// writes it as 15m; 0 is reported as off; a value the daemon refuses to
// start with fails the report. The A2A backends' retry settings
// (modules.a2a.retry, §11.6) are reported beside the backends, and a value
// the module refuses fails it too.
func TestDoctorReportsTheNoResponseDeadlineAndBackendRetries(t *testing.T) {
	layout := freshInit(t)
	out, err := doctorJSON(t, layout, testDoctorEnv(t))
	tasks, _ := out["tasks"].(map[string]any)
	if err != nil || tasks["no_response_after"] != "15m0s" || tasks["configured"] != true || checkStatus(out, "tasks.no_response") != stOK {
		t.Fatalf("fresh init: %v, tasks %v, check %q", err, tasks, checkStatus(out, "tasks.no_response"))
	}
	for v, want := range map[string]string{"0": stInfo, "later": stFail, "2h": stOK} {
		setConfigKeys(t, layout, map[string]any{"no_response_after": v})
		out, _ := doctorJSON(t, layout, testDoctorEnv(t))
		if got := checkStatus(out, "tasks.no_response"); got != want || (want == stFail) == (out["ok"] == true) {
			t.Errorf("no_response_after %q: check %q, ok %v; want %q", v, got, out["ok"], want)
		}
	}
	setConfigKeys(t, layout, map[string]any{"no_response_after": nil})
	out, _ = doctorJSON(t, layout, testDoctorEnv(t))
	if tasks, _ := out["tasks"].(map[string]any); tasks["no_response_after"] != "15m0s" || tasks["configured"] != false {
		t.Fatalf("left out: %v", tasks)
	}

	backend := []any{map[string]any{"match": "*", "url": "http://127.0.0.1:9900", "allow_tcp": true}}
	for retry, want := range map[string]string{"": stOK, `{"give_up_after":"20m"}`: stInfo, `{"give_up_after":"0"}`: stInfo,
		`{"max_interval":"soon"}`: stFail} {
		a2a := map[string]any{"backends": backend}
		if retry != "" {
			var r any
			_ = json.Unmarshal([]byte(retry), &r)
			a2a["retry"] = r
		}
		setConfigKeys(t, layout, map[string]any{"modules": map[string]any{"a2a": a2a}})
		out, _ := doctorJSON(t, layout, testDoctorEnv(t))
		if got := checkStatus(out, "a2a.backends.retry"); got != want {
			t.Errorf("retry %s: check %q, want %q", retry, got, want)
		}
	}
}
