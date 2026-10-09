package daemon

// backend_policy.go checks, before the daemon starts, the keys that say who
// may be on the far side of a Unix socket backend (LEC §3.2, §4.4):
// expected_user, socket_group and peer_check, the same three in
// modules.service, in each modules.a2a.backends[] entry and in
// modules.anetlink, read by the one struct the modules embed
// (backendconn.Policy). A value the module could not use refuses the start
// with config_invalid naming the key and what it takes, instead of the
// module's own error a moment later. expected_uid, the key these replaced,
// is config_key_removed (removedSettings).

import (
	"errors"
	"strconv"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
)

func init() { configChecks = append(configChecks, checkBackendPolicies) }

// backendPolicyKeys are the keys checked, with config_invalid's {rule} for
// each in both languages.
var backendPolicyKeys = []struct{ key, zh, en string }{
	{"expected_user", `须是本机的用户名或十进制数字 uid，写成字符串，如 "1001"`,
		`it is a user name on this machine or a decimal uid, written as a string such as "1001"`},
	{"socket_group", `须是本机的组名或十进制数字 gid，写成字符串`,
		`it is a group name on this machine or a decimal gid, written as a string`},
	{"peer_check", `只能不设（严格核实，缺省）或设为 "path-only"`,
		`it is left out (the strict default) or "path-only"`},
}

// checkBackendPolicies is the ConfigCheck: the first backend policy key,
// in file order, whose value the modules cannot use.
func checkBackendPolicies(_ Config, raw map[string]any) *a2ashape.Refusal {
	mods, _ := raw["modules"].(map[string]any)
	type block struct {
		at string
		v  map[string]any
	}
	var blocks []block
	if v, ok := mods["service"].(map[string]any); ok {
		blocks = append(blocks, block{"modules.service", v})
	}
	if a, ok := mods["a2a"].(map[string]any); ok {
		bs, _ := a["backends"].([]any)
		for i, b := range bs {
			if v, ok := b.(map[string]any); ok {
				blocks = append(blocks, block{"modules.a2a.backends[" + strconv.Itoa(i) + "]", v})
			}
		}
	}
	if v, ok := mods["anetlink"].(map[string]any); ok {
		blocks = append(blocks, block{"modules.anetlink", v})
	}
	for _, b := range blocks {
		if r := checkBackendPolicy(b.at, b.v); r != nil {
			return r
		}
	}
	return nil
}

// checkBackendPolicy is config_invalid for the first of the keys of one
// block that is not a string, or that backendconn cannot resolve.
func checkBackendPolicy(at string, block map[string]any) *a2ashape.Refusal {
	var p backendconn.Policy
	into := map[string]*string{"expected_user": &p.ExpectedUser, "socket_group": &p.SocketGroup, "peer_check": &p.PeerCheck}
	for _, k := range backendPolicyKeys {
		v, ok := block[k.key]
		if !ok || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return configInvalid(at+"."+k.key, ruleText(k.zh, k.en))
		}
		*into[k.key] = s
	}
	_, err := p.Resolve()
	var pe *backendconn.PolicyError
	if !errors.As(err, &pe) {
		return nil
	}
	for _, k := range backendPolicyKeys {
		if k.key == pe.Key {
			return configInvalid(at+"."+k.key, ruleText(k.zh, k.en))
		}
	}
	return nil
}
