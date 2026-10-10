package daemon

// ux UX-D3, §2.3 (UX-T6, the daemon's half): a config.json holding a key
// 0.3.0 removed does not start the daemon, and the refusal names the key and
// what to do instead. cmd/anet/si5_v030_test.go runs `anet daemon` on the
// same files.

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// removedValue is a value of the shape each removed key had.
func removedValue(key string) any {
	switch {
	case strings.HasSuffix(key, "_seconds"), strings.HasSuffix(key, "_ms"), strings.HasSuffix(key, "_s"),
		strings.HasSuffix(key, "expected_uid"), key == "auto_reply.max_auto_replies":
		return 7
	case key == "inbound.reject_notice":
		return map[string]any{"per_peer_per_hour": 6}
	case key == "inbound.pending":
		return map[string]any{"max_total": 10}
	case key == "accept_delegations", key == "payments.publish_prices", key == "auto_reply.require_image":
		return true
	case key == "providers":
		return map[string]any{"anetlink": map[string]any{"socket": "/run/x.sock"}}
	}
	return "x"
}

// withKey puts value at the dotted key of m, making the objects and the one
// array element on the way.
func withKey(m map[string]any, key string, value any) {
	segs := strings.Split(key, ".")
	cur := m
	for i, s := range segs {
		name, each := strings.CutSuffix(s, "[]")
		last := i == len(segs)-1
		if each {
			el := map[string]any{"id": "net.echo"}
			if arr, ok := cur[name].([]any); ok && len(arr) > 0 {
				el = arr[0].(map[string]any)
			} else {
				cur[name] = []any{el}
			}
			cur = el
			continue
		}
		if last {
			cur[name] = value
			return
		}
		next, ok := cur[name].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[name] = next
		}
		cur = next
	}
}

// pathOf is the path FindRemovedSettings reports for a key withKey wrote.
func pathOf(key string) string { return strings.ReplaceAll(key, "[]", "[0]") }

// freshConfigFile is the config.json a fresh install has, as a map.
func freshConfigFile(t *testing.T) map[string]any {
	t.Helper()
	b, err := json.Marshal(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeConfigMap(t *testing.T, l Layout, m map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.ConfigPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Each removed key is found where it sits, in table order, with its phrase.
func TestFindRemovedSettings(t *testing.T) {
	for _, rs := range removedSettings {
		m := map[string]any{}
		withKey(m, rs.key, removedValue(rs.key))
		b, _ := json.Marshal(m)
		got, err := FindRemovedSettings(b)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Path != pathOf(rs.key) || got[0].Key != rs.key || got[0].Instead != rs.instead {
			t.Errorf("%s: %+v", rs.key, got)
		}
		if got[0].Pending != removedStillWritten[rs.key] {
			t.Errorf("%s: pending %q", rs.key, got[0].Pending)
		}
	}
	for raw, want := range map[string][]string{
		`{}`:                          nil,
		`{"pricing": null}`:           {"pricing"},
		`{"inbound": "closed"}`:       nil,
		`{"inbound": {"allow": "x"}}`: nil,
		`{"inbound": {"public_capabilities": [{"id": "a"}, {"id": "b", "evidence": "cid"}, {"evidence": "full"}]}}`: {
			"inbound.public_capabilities[1].evidence", "inbound.public_capabilities[2].evidence"},
		`{"modules": {"a2a": {"backends": [{"expected_uid": 1}]}, "p2p": {"dial_timeout_ms": 1, "reach_timeout_ms": 2}}}`: {
			"modules.a2a.backends[0].expected_uid", "modules.p2p.dial_timeout_ms", "modules.p2p.reach_timeout_ms"},
		`{"modules": {"x402": {"voucher_url": "u", "voucher_addr": "a"}}, "accept_delegations": false, "rotation_grace": "1h"}`: {
			"modules.x402.voucher_addr", "modules.x402.voucher_url", "accept_delegations", "rotation_grace"},
		`{"inbound": {"public_capabilities": "not a list"}}`: nil,
		// encoding/json reads a key in any case into its field, so a removed
		// key is found in any case too, spelled as the file spells it.
		`{"Rotation_Grace": "720h", "PRICING": "x"}`: {"PRICING", "Rotation_Grace"},
		`{"Modules": {"X402": {"Voucher_Addr": ":8402"}}, "payments": {"Publish_Prices": true}}`: {
			"payments.Publish_Prices", "Modules.X402.Voucher_Addr"},
		`{"rotation_grace": "1h", "Rotation_grace": "2h"}`: {"Rotation_grace", "rotation_grace"},
	} {
		got, err := FindRemovedSettings([]byte(raw))
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		var paths []string
		for _, g := range got {
			paths = append(paths, g.Path)
		}
		if !slices.Equal(paths, want) {
			t.Errorf("%s: %v, want %v", raw, paths, want)
		}
	}
	for _, raw := range []string{``, `[]`, `"x"`, `{`, `null`} {
		if _, err := FindRemovedSettings([]byte(raw)); err == nil {
			t.Errorf("%q is not a config object and must be an error", raw)
		}
	}
}

// Every removed key's phrase exists in the reason table, and every phrase of
// the table is some key's (or the invite variable's).
func TestRemovedSettingsHaveTheirPhrases(t *testing.T) {
	used := map[string]bool{"invite_env": true}
	for _, rs := range removedSettings {
		used[rs.instead] = true
		s := a2ashape.Sentence("config_key_removed", a2ashape.English, a2ashape.Args{"key": rs.key, "instead": rs.instead})
		if strings.Contains(s, "—") {
			t.Errorf("%s: phrase %q is not in the reason table: %s", rs.key, rs.instead, s)
		}
	}
	for k := range removedStillWritten {
		if !slices.ContainsFunc(removedSettings, func(rs struct{ key, instead string }) bool { return rs.key == k }) {
			t.Errorf("removedStillWritten names %s, which is not a removed key", k)
		}
	}
	// The phrases the table has for config_key_removed are exactly these.
	for _, id := range []string{"pricing", "publish_prices", "voucher", "witness_hub", "providers", "accept_delegations",
		"rotation_grace", "list_files", "reject_notice", "pending", "evidence", "require_image", "error_reply",
		"fixed_auto_reply", "payees_file", "anetlink_timeout", "expected_uid", "p2p_timeouts", "taskboard", "invite_env"} {
		if !used[id] {
			t.Errorf("phrase %s is no removed key's", id)
		}
	}
}

// UX-T6: each removed key alone in a fresh config.json stops the daemon, and
// the sentence names the key and what to do instead. A fresh config and a
// missing one start.
func TestRemovedSettingsStopTheDaemon(t *testing.T) {
	// A removed key under modules.anetlink makes a block without socket,
	// which has a default only where XDG_RUNTIME_DIR is set
	// (anetlink_config.go): the one refusal is then the removed key's.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	l := NewLayout(t.TempDir())
	if err := CheckConfigForStart(l); err != nil {
		t.Fatalf("no config.json: %v", err)
	}
	writeConfigMap(t, l, freshConfigFile(t))
	if err := CheckConfigForStart(l); err != nil {
		t.Fatalf("a fresh config.json: %v", err)
	}
	for _, rs := range removedSettings {
		t.Run(rs.key, func(t *testing.T) {
			if p := removedStillWritten[rs.key]; p != "" {
				t.Skip(p + ": this binary still writes " + rs.key)
			}
			l := NewLayout(t.TempDir())
			m := freshConfigFile(t)
			withKey(m, rs.key, removedValue(rs.key))
			writeConfigMap(t, l, m)
			err := CheckConfigForStart(l)
			var cr *ConfigRefusal
			if !errors.As(err, &cr) || len(cr.Refusals) != 1 {
				t.Fatalf("the daemon would start with %s: %v", rs.key, err)
			}
			r := cr.Refusals[0]
			if r.Code() != "config_key_removed" {
				t.Fatalf("code %s", r.Code())
			}
			for _, lang := range []a2ashape.Language{a2ashape.Chinese, a2ashape.English} {
				s := cr.Sentences(lang)[0]
				instead := a2ashape.Sentence("config_key_removed", lang, a2ashape.Args{"key": "K", "instead": rs.instead})
				instead = instead[strings.Index(instead, "K")+1:]
				instead = instead[strings.IndexAny(instead, ";；")+len("；"):]
				if !strings.Contains(s, pathOf(rs.key)) || !strings.Contains(s, strings.TrimRight(instead, ".。")) {
					t.Errorf("the sentence must name %s and what to do instead: %s", pathOf(rs.key), s)
				}
			}
		})
	}
}

// The keys left to the work items that still write them (removedStillWritten)
// may only go away. Every other test skips an exempted key, so a key added to
// the list would start the daemon again with no test failing; this list is
// the most it may hold. A work item that deletes its code deletes its lines
// here and there.
func TestRemovedStillWrittenOnlyShrinks(t *testing.T) {
	most := map[string]string{
		"auto_reply.require_image":         "v030-todo(UX-9)",
		"auto_reply.usage_hint":            "v030-todo(UX-9)",
		"auto_reply.error_reply":           "v030-todo(UX-9)",
		"auto_reply.poll_interval_seconds": "v030-todo(UX-9)",
		"auto_reply.max_auto_replies":      "v030-todo(UX-9)",
	}
	for k, p := range removedStillWritten {
		if most[k] != p {
			t.Errorf("%s (%s) is exempted from refusal; only the keys a work item still writes are (ux UX-D3)", k, p)
		}
	}
}

// Several removed keys are refused at once, so that one edit fixes them all;
// a key still written by this binary is not among them.
func TestRemovedSettingsAllAtOnce(t *testing.T) {
	l := NewLayout(t.TempDir())
	m := freshConfigFile(t)
	withKey(m, "pricing", "x")
	withKey(m, "rotation_grace", "2h")
	withKey(m, "modules.p2p.dial_timeout_ms", 5)
	writeConfigMap(t, l, m)
	var cr *ConfigRefusal
	if err := CheckConfigForStart(l); !errors.As(err, &cr) || len(cr.Refusals) != 3 {
		t.Fatalf("%v", err)
	}
	if lines := strings.Split(cr.Error(), "\n"); len(lines) != 3 {
		t.Fatalf("one sentence a line: %q", cr.Error())
	}
}

// A ConfigCheck an area adds refuses with its own code, after the removed
// keys.
func TestConfigChecksRun(t *testing.T) {
	saved := configChecks
	t.Cleanup(func() { configChecks = saved })
	configChecks = append(configChecks, func(c Config, raw map[string]any) *a2ashape.Refusal {
		if c.NoResponseAfter == "bad" {
			return a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "no_response_after", "rule": "a duration"})
		}
		return nil
	})
	l := NewLayout(t.TempDir())
	m := freshConfigFile(t)
	m["no_response_after"] = "bad"
	withKey(m, "pricing", "x")
	writeConfigMap(t, l, m)
	var cr *ConfigRefusal
	if err := CheckConfigForStart(l); !errors.As(err, &cr) || len(cr.Refusals) != 2 ||
		cr.Refusals[0].Code() != "config_key_removed" || cr.Refusals[1].Code() != "config_invalid" {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(l.ConfigPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckConfigForStart(l); err == nil || errors.As(err, &cr) {
		t.Fatalf("a config.json that does not parse: %v", err)
	}
}

// arrayIndex is the index of an array element in a reported path.
var arrayIndex = regexp.MustCompile(`\[[0-9]+\]`)

// FuzzFindRemovedSettings: any bytes are either refused as not a config
// object or give paths that are removed keys' paths.
func FuzzFindRemovedSettings(f *testing.F) {
	f.Add([]byte(`{"inbound": {"public_capabilities": [{"evidence": "cid"}], "allow_file": "x"}, "pricing": "1"}`))
	f.Add([]byte(`{"modules": {"a2a": {"backends": [{"expected_uid": 1}, {}]}}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[1,2]`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		got, err := FindRemovedSettings(raw)
		if err != nil {
			if got != nil {
				t.Fatal("results with an error")
			}
			return
		}
		for _, g := range got {
			if g.Path == "" || g.Instead == "" {
				t.Fatalf("%+v", g)
			}
			found := false
			for _, rs := range removedSettings {
				if rs.key == g.Key {
					found = true
				}
			}
			if !found || !strings.EqualFold(arrayIndex.ReplaceAllString(g.Path, "[]"), g.Key) {
				t.Fatalf("path %s for key %s", g.Path, g.Key)
			}
		}
	})
}
