package evtypes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// G-EVID (LEC §7.1): every sample in testdata — anet.autoreply.invoked for a direct run, a sandboxed
// run, an openai turn, a turn the budget turned away, a run killed for its disk use and one the sandbox
// could not take, and anet.hub.admission — has exactly the members of its registered payload, and
// together the samples use every member, so the shape is neither wider nor narrower than LEC §2.5.
// The daemon's TestAutoReplyEvidenceGolden writes the samples from the code that writes the records.
func TestFixedPayloadSamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no samples: %v", err)
	}
	seen := map[string]map[string]bool{}
	var walk func(prefix string, m map[string]any, into map[string]bool)
	walk = func(prefix string, m map[string]any, into map[string]bool) {
		for k, v := range m {
			into[prefix+k] = true
			if sub, ok := v.(map[string]any); ok && (k == "egress" || k == "connect") {
				walk(prefix+k+".", sub, into)
			}
		}
	}
	kinds := map[string]bool{}
	for _, f := range files {
		typ := AutoReplyInvoked
		if strings.HasPrefix(filepath.Base(f), "hub_admission") {
			typ = HubAdmission
		}
		shape, ok := PayloadOf(typ)
		if !ok {
			t.Fatalf("%s has no payload shape", typ)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if why := shape.Check(m); why != "" {
			t.Errorf("%s: %s", f, why)
		}
		if seen[typ] == nil {
			seen[typ] = map[string]bool{}
		}
		walk("", m, seen[typ])
		if typ == AutoReplyInvoked {
			kinds[m["mode"].(string)+"/"+m["exit"].(string)] = true
		}
	}
	for _, want := range []string{"direct/ok", "sandbox/ok", "openai/error", "sandbox/budget", "sandbox/resource"} {
		if !kinds[want] {
			t.Errorf("no anet.autoreply.invoked sample of %s", want)
		}
	}
	for typ, used := range seen {
		shape, _ := PayloadOf(typ)
		var all []string
		var add func(prefix string, p Payload)
		add = func(prefix string, p Payload) {
			for _, k := range append(append([]string{}, p.Required...), p.Optional...) {
				all = append(all, prefix+k)
				if sub, ok := p.Nested[k]; ok {
					add(prefix+k+".", sub)
				}
			}
		}
		add("", shape)
		sort.Strings(all)
		for _, k := range all {
			if !used[k] {
				t.Errorf("%s: no sample has %s", typ, k)
			}
		}
	}
}

// Check finds a missing member, an extra one, and one of a nested object.
func TestPayloadCheck(t *testing.T) {
	shape, _ := PayloadOf(AutoReplyInvoked)
	ok := map[string]any{"agent": "", "interaction_id": "ix", "peer_aid": "p", "trusted": false, "sandboxed": false,
		"duration_ms": 0, "mode": "openai", "sandbox": "", "exit": "ok"}
	if why := shape.Check(ok); why != "" {
		t.Fatalf("a complete payload: %s", why)
	}
	for name, edit := range map[string]func(map[string]any){
		"missing": func(m map[string]any) { delete(m, "exit") },
		"extra":   func(m map[string]any) { m["prompt"] = "x" },
		"nested":  func(m map[string]any) { m["egress"] = map[string]any{"model_requests": 1} },
		"scalar":  func(m map[string]any) { m["egress"] = "x" },
	} {
		m := map[string]any{}
		for k, v := range ok {
			m[k] = v
		}
		edit(m)
		if shape.Check(m) == "" {
			t.Errorf("%s: not found", name)
		}
	}
}
