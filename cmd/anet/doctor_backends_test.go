package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
			be := map[string]any{"match": "*", "url": "http://127.0.0.1:9900"}
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
