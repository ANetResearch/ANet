package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
