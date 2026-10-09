package daemon

import (
	"encoding/json"
	"errors"
	"os/user"
	"regexp"
	"strings"
	"testing"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// startRefusals is what CheckConfigForStart says of a fresh config.json
// with modules set: nil when the daemon would start.
func startRefusals(t *testing.T, modules map[string]any) []*a2ashape.Refusal {
	t.Helper()
	// modules.anetlink.socket has a default only where XDG_RUNTIME_DIR is set
	// (anetlink_config.go); the blocks here leave it out.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	l := NewLayout(t.TempDir())
	m := freshConfigFile(t)
	m["modules"] = modules
	writeConfigMap(t, l, m)
	err := CheckConfigForStart(l)
	if err == nil {
		return nil
	}
	var cr *ConfigRefusal
	if !errors.As(err, &cr) {
		t.Fatalf("not a refusal: %v", err)
	}
	return cr.Refusals
}

// LEC §3.2: expected_user is a user name of this machine or a decimal uid
// — one /etc/passwd does not hold included — in all three modules; a name
// that is not there, a number that is not a string, a socket_group that is
// not there and a peer_check other than "path-only" stop the daemon with
// config_invalid naming the key.
func TestBackendPolicyKeysAreCheckedAtStart(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	sock := map[string]any{"match": "*", "url": "unix:///run/agent/a2a.sock"}
	with := func(kv map[string]any) map[string]any {
		b := map[string]any{}
		for k, v := range sock {
			b[k] = v
		}
		for k, v := range kv {
			b[k] = v
		}
		return b
	}
	for _, ok := range []map[string]any{
		{"service": map[string]any{"expected_user": "3999999999"}},
		{"service": map[string]any{"expected_user": me.Username, "peer_check": "path-only"}},
		{"a2a": map[string]any{"backends": []any{with(map[string]any{"expected_user": "1001", "peer_check": ""})}}},
		{"anetlink": map[string]any{"expected_user": "0", "socket_group": "0"}},
		{"anetlink": map[string]any{"socket": "/run/x/c1.sock"}},
	} {
		if r := startRefusals(t, ok); r != nil {
			t.Errorf("%v: refused: %v", ok, r[0].Args)
		}
	}
	for want, bad := range map[string]map[string]any{
		"modules.service.expected_user":      {"service": map[string]any{"expected_user": "no-such-user-anet-test"}},
		"modules.service.peer_check":         {"service": map[string]any{"peer_check": "strict"}},
		"modules.a2a.backends[1].peer_check": {"a2a": map[string]any{"backends": []any{with(nil), with(map[string]any{"peer_check": "none"})}}},
		"modules.a2a.backends[0].expected_user": {"a2a": map[string]any{"backends": []any{
			with(map[string]any{"expected_user": 1001})}}},
		"modules.anetlink.socket_group":  {"anetlink": map[string]any{"socket_group": "no-such-group-anet-test"}},
		"modules.anetlink.expected_user": {"anetlink": map[string]any{"expected_user": "4294967295"}},
	} {
		r := startRefusals(t, bad)
		if len(r) != 1 || r[0].Code() != "config_invalid" || r[0].Args["key"] != want {
			t.Errorf("%s: %+v", want, r)
			continue
		}
		for _, lang := range []a2ashape.Language{a2ashape.Chinese, a2ashape.English} {
			if s := a2ashape.Sentence("config_invalid", lang, r[0].Args); !strings.Contains(s, want) {
				t.Errorf("%s: %q does not name the key", want, s)
			}
		}
	}
}

// M-PC-3: expected_uid is gone from every module, not only from anetlink:
// in any of the three it stops the daemon with config_key_removed, whose
// sentence says to use expected_user, which takes a uid too (LEC §3.1).
func TestExpectedUIDStopsTheDaemonInEveryModule(t *testing.T) {
	for path, modules := range map[string]map[string]any{
		"modules.service.expected_uid":         {"service": map[string]any{"expected_uid": 1000}},
		"modules.a2a.backends[0].expected_uid": {"a2a": map[string]any{"backends": []any{map[string]any{"match": "*", "expected_uid": 1000}}}},
		"modules.anetlink.expected_uid":        {"anetlink": map[string]any{"expected_uid": 1000}},
	} {
		r := startRefusals(t, modules)
		if len(r) != 1 || r[0].Code() != "config_key_removed" || r[0].Args["key"] != path || r[0].Args["instead"] != "expected_uid" {
			t.Errorf("%s: %+v", path, r)
			continue
		}
		if s := a2ashape.Sentence("config_key_removed", a2ashape.English, r[0].Args); !strings.Contains(s, "expected_user") ||
			!strings.Contains(s, "numeric uid") {
			t.Errorf("%s: %q", path, s)
		}
	}
}

// FuzzCheckBackendPolicies: any config.json object either passes the
// backend policy check or is refused with config_invalid naming one of the
// three keys of one of the three modules' blocks.
func FuzzCheckBackendPolicies(f *testing.F) {
	f.Add([]byte(`{"modules":{"service":{"expected_user":"1001","peer_check":"path-only"}}}`))
	f.Add([]byte(`{"modules":{"a2a":{"backends":[{"match":"*"},{"expected_user":7},{"peer_check":"x"}]}}}`))
	f.Add([]byte(`{"modules":{"anetlink":{"socket_group":"no-such-group-anet-test"}}}`))
	f.Add([]byte(`{"modules":[1,2]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return
		}
		r := checkBackendPolicies(Config{}, m)
		if r == nil {
			return
		}
		key, _ := r.Args["key"].(string)
		if r.Code() != "config_invalid" || !backendPolicyKey.MatchString(key) {
			t.Fatalf("%s: %s %v", raw, r.Code(), r.Args)
		}
	})
}

// backendPolicyKey is the key a backend policy refusal may name.
var backendPolicyKey = regexp.MustCompile(`^modules\.(service|anetlink|a2a\.backends\[[0-9]+\])\.(expected_user|socket_group|peer_check)$`)
