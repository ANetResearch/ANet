package daemon

// anetlink_config.go checks, before the daemon starts, the two keys of modules.anetlink that are the
// C1 client's own (LEC §3.2): socket, which may be left out where XDG_RUNTIME_DIR gives it a default,
// and allow_tcp, which may not be true, since C1 runs over a Unix socket only. The keys the block
// shares with every Unix socket backend (expected_user, socket_group, peer_check) are
// backend_policy.go's; timeout_s and expected_uid are removed keys (config_key_removed). A value the
// module could not use refuses the start with config_invalid naming the key, the same sentence in the
// daemon, doctor and the deployment-template test, instead of the module's own error a moment later.
//
// The module is not imported here: the daemon is built without it under -tags no_anetlink, and these
// checks only read config.json.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/backendconn"
)

func init() { configChecks = append(configChecks, checkANetLinkConfig) }

// anetlinkDefaultSocket is modules.anetlink.socket when the block leaves it out:
// $XDG_RUNTIME_DIR/anetlink/c1.sock, anetlinkd's own default (provider/anetlink.DefaultSocket says the
// same; TestTheDaemonKnowsANetLinksDefaultSocket holds them together). "" when XDG_RUNTIME_DIR is not
// set.
func anetlinkDefaultSocket() string {
	rd := os.Getenv("XDG_RUNTIME_DIR")
	if rd == "" {
		return ""
	}
	return filepath.Join(rd, "anetlink", "c1.sock")
}

// checkANetLinkConfig is the ConfigCheck of modules.anetlink: the block is an object, allow_tcp is not
// true, and socket is an absolute path a Unix socket can have, or left out where XDG_RUNTIME_DIR gives
// it a default.
func checkANetLinkConfig(_ Config, raw map[string]any) *a2ashape.Refusal {
	mods, _ := raw["modules"].(map[string]any)
	v, ok := mods["anetlink"]
	if !ok || v == nil {
		return nil
	}
	block, ok := v.(map[string]any)
	if !ok {
		return configInvalid("modules.anetlink", ruleText(`须是一个对象，如 {"socket": "/run/anetlink/c1.sock"}；全用缺省值时写 {}`,
			`it is an object such as {"socket": "/run/anetlink/c1.sock"}; {} takes the defaults`))
	}
	if t, ok := block["allow_tcp"]; ok && t != nil && t != false {
		return configInvalid("modules.anetlink.allow_tcp", ruleText("C1 只经 Unix socket，不能用 TCP", "C1 runs over a Unix socket only, never TCP"))
	}
	s, present := block["socket"]
	if !present || s == nil || s == "" {
		if anetlinkDefaultSocket() == "" {
			return configInvalid("modules.anetlink.socket", ruleText(
				"没有 XDG_RUNTIME_DIR 时没有缺省值，须写明 anetlinkd 所用 socket 的绝对路径（它的 --c1-socket）",
				"there is no default without XDG_RUNTIME_DIR, so give the absolute path of the socket anetlinkd serves (its --c1-socket)"))
		}
		return nil
	}
	if sock, ok := s.(string); !ok || !socketPathUsable(sock) {
		return configInvalid("modules.anetlink.socket", ruleText(
			"须是一个干净的绝对路径，不含冒号、空格或控制字符，且不超过 Unix socket 地址的长度",
			"it is a clean absolute path, without a colon, space or control character, short enough for a Unix socket address"))
	}
	return nil
}

// socketPathUsable reports a path backendconn can dial as a socket: what provider/anetlink.New
// accepts.
func socketPathUsable(sock string) bool {
	if strings.Contains(sock, ":") {
		return false
	}
	t, err := backendconn.Parse(backendconn.SchemeUnix + "://" + sock)
	return err == nil && t.Socket == sock
}
