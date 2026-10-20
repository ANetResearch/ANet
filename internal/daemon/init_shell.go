package daemon

// init_shell.go is the offline controller grant (`anet init --shell-controller <aid>`): it makes one
// AID the only caller of the shell module and puts it on peers.allow, under one confirmation, while no
// daemon runs. The two writes are what the shell module and the inbound policy each need before that
// peer can run a command (the policy admits only a peer on peers.allow or peers.trust; the shell module
// then admits only the AIDs on its own allow list). Writing both under one confirmation, before the node
// is online, means that a declined confirmation leaves nothing open. Every refusal comes before the
// first byte is written.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ANetResearch/ANetCore/identity"

	"github.com/ANetResearch/ANet/internal/a2ashape"
)

// ShellControllerPlan is what InitShellController would write.
type ShellControllerPlan struct {
	AID         string          `json:"aid"`
	ModuleBlock json.RawMessage `json:"modules_shell,omitempty"` // nil when modules.shell is already this block
	AddAllow    bool            `json:"peers_allow_added"`       // false when aid is already on peers.allow or peers.trust
	Changed     bool            `json:"changed"`
}

// shellControllerBlock is the modules.shell block of a controller grant, in the key order it is shown.
type shellControllerBlock struct {
	Allow          []string `json:"allow"`
	AllowArbitrary bool     `json:"allow_arbitrary"`
	Shell          string   `json:"shell,omitempty"`
}

// InitShellController makes aid the one caller of the shell module (D9). Offline: it holds the data lock
// for the whole call (daemon_running refusal otherwise). Refusals, all before anything is written:
// config_invalid when aid is not identity.ValidAID, when modules.shell exists and differs from the
// block it would write, when "shell" is in `without`, when inbound.public_capabilities names a
// capability starting "shell.", or when aid is on peers.deny. The block is
// {"allow":[aid],"allow_arbitrary":true} plus "shell":interp when interp != "". When the plan
// changes nothing, confirm is not called and Changed is false. Otherwise confirm(plan) is called; a
// non-nil error is returned unchanged and nothing is written. Then InitLayout(l), the block into
// config.json (existing keys preserved, writeFileAtomic 0600), then aid appended to peers.allow
// (the editPeerFile format, writeFileAtomic 0600). No other key or file is written.
func InitShellController(l Layout, aid, interp string, confirm func(ShellControllerPlan) error) (ShellControllerPlan, error) {
	if !identity.ValidAID(aid) {
		return ShellControllerPlan{}, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules.shell.allow",
			"rule": ruleOf("须是完整的 AID", "it is a full AID")})
	}
	if strings.IndexFunc(interp, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
		return ShellControllerPlan{}, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules.shell.shell",
			"rule": ruleOf("不能包含控制字符", "it contains no control characters")})
	}
	// The lock is held until the writes are done, so a daemon cannot start between the check and the
	// write. A data directory that does not exist yet has no daemon; it is locked (and so created) only
	// once the confirmation has been given, so a refusal or a declined confirmation creates nothing.
	var lock *DataLock
	if _, err := os.Stat(l.Root); err == nil {
		k, err := lockForShellController(l)
		if err != nil {
			return ShellControllerPlan{}, err
		}
		lock = k
	}
	defer func() { lock.Release() }()

	plan, err := planShellController(l, aid, interp)
	if err != nil {
		return plan, err
	}
	if !plan.Changed {
		return plan, nil
	}
	if err := confirm(plan); err != nil {
		return plan, err
	}
	if lock == nil {
		k, err := lockForShellController(l)
		if err != nil {
			return plan, err
		}
		lock = k
		// Another process may have written the directory while the confirmation was asked. The plan the
		// user confirmed must still be the one that is carried out.
		again, err := planShellController(l, aid, interp)
		if err != nil {
			return plan, err
		}
		if !samePlan(again, plan) {
			return plan, fmt.Errorf("anet: %s changed while the confirmation was asked; nothing was changed, run the command again", l.Root)
		}
	}

	if _, err := InitLayout(l); err != nil {
		return plan, err
	}
	if plan.ModuleBlock != nil {
		if err := writeShellBlock(l, plan.ModuleBlock); err != nil {
			return plan, err
		}
	}
	if plan.AddAllow {
		path := filepath.Join(l.Root, peersAllowFile)
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return plan, err
		}
		if out, changed := editPeerLines(raw, aid, true); changed {
			if err := writeFileAtomic(path, []byte(out), 0o600); err != nil {
				return plan, err
			}
		}
	}
	return plan, nil
}

// lockForShellController takes the data lock of l, answering daemon_running when a daemon holds it.
func lockForShellController(l Layout) (*DataLock, error) {
	k, err := AcquireDataLock(l)
	if errors.Is(err, ErrDataLockHeld) {
		return nil, a2ashape.Refuse("daemon_running", a2ashape.Args{"path": l.Root})
	}
	return k, err
}

// samePlan reports whether two plans write the same things.
func samePlan(a, b ShellControllerPlan) bool {
	return a.AID == b.AID && bytes.Equal(a.ModuleBlock, b.ModuleBlock) && a.AddAllow == b.AddAllow && a.Changed == b.Changed
}

// planShellController reads l's configuration and peer lists and says what the grant would write, or
// refuses it (see InitShellController). It writes nothing.
func planShellController(l Layout, aid, interp string) (ShellControllerPlan, error) {
	plan := ShellControllerPlan{AID: aid}
	block, err := json.Marshal(shellControllerBlock{Allow: []string{aid}, AllowArbitrary: true, Shell: interp})
	if err != nil {
		return plan, err
	}

	cur := map[string]any{}
	raw, err := os.ReadFile(l.ConfigPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return plan, fmt.Errorf("anet: read config: %w", err)
	default:
		if cur, err = decodeObject(raw); err != nil {
			return plan, fmt.Errorf("anet: %s: %w (nothing was changed)", l.ConfigPath(), err)
		}
	}

	without, err := withoutOf(cur)
	if err != nil {
		return plan, err
	}
	if slices.Contains(without, "shell") {
		return plan, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "without",
			"rule": ruleOf("它关闭了 shell 模块；请先运行 anet init --with shell", "it turns the shell module off; run anet init --with shell first")})
	}
	if id, err := publicShellCapability(cur); err != nil {
		return plan, err
	} else if id != "" {
		return plan, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "inbound.public_capabilities",
			"rule": ruleOf("它把 "+cleanKey(id)+" 公开给任何对端，而 shell 模块只应由其控制者调用；请先删除该条目",
				"it makes "+cleanKey(id)+" public to any peer, while the shell module must be reachable by its controller alone; remove that entry first")})
	}

	var mods map[string]any
	if v, ok := cur["modules"]; ok && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return plan, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules",
				"rule": ruleOf("应为 JSON 对象", "expected a JSON object")})
		}
		mods = m
	}
	if v, ok := mods["shell"]; ok && v != nil {
		if !isShellControllerBlock(v, aid, interp) {
			return plan, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "modules.shell",
				"rule": ruleOf("已有不同的设置；请先删除 modules.shell，或手动编辑",
					"it is already set to something else; remove modules.shell first, or edit it by hand")})
		}
	} else {
		plan.ModuleBlock = block
	}

	deny, err := readPeerFile(filepath.Join(l.Root, peersDenyFile))
	if err != nil {
		return plan, fmt.Errorf("anet: read %s: %w (nothing was changed)", peersDenyFile, err)
	}
	if deny[aid] {
		return plan, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "peers.deny",
			"rule": ruleOf("它列有 "+aid+"；请先将其从 peers.deny 中删除", "it lists "+aid+"; remove it from peers.deny first")})
	}
	allow, err := readPeerFile(filepath.Join(l.Root, peersAllowFile))
	if err != nil {
		return plan, fmt.Errorf("anet: read %s: %w (nothing was changed)", peersAllowFile, err)
	}
	trust, err := readPeerFile(filepath.Join(l.Root, peersTrustFile))
	if err != nil {
		return plan, fmt.Errorf("anet: read %s: %w (nothing was changed)", peersTrustFile, err)
	}
	plan.AddAllow = !allow[aid] && !trust[aid]
	plan.Changed = plan.ModuleBlock != nil || plan.AddAllow
	return plan, nil
}

// publicShellCapability is the first inbound.public_capabilities id that starts with "shell.", or "".
func publicShellCapability(cur map[string]any) (string, error) {
	in, ok := cur["inbound"]
	if !ok || in == nil {
		return "", nil
	}
	inbound, ok := in.(map[string]any)
	if !ok {
		return "", a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "inbound",
			"rule": ruleOf("应为 JSON 对象", "expected a JSON object")})
	}
	pc, ok := inbound["public_capabilities"]
	if !ok || pc == nil {
		return "", nil
	}
	list, ok := pc.([]any)
	if !ok {
		return "", a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "inbound.public_capabilities",
			"rule": ruleOf("应为列表", "expected a list")})
	}
	for _, e := range list {
		entry, ok := e.(map[string]any)
		if !ok {
			return "", a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "inbound.public_capabilities",
				"rule": ruleOf("每一项应为 JSON 对象", "each entry is expected to be a JSON object")})
		}
		if id, _ := entry["id"].(string); strings.HasPrefix(id, "shell.") {
			return id, nil
		}
	}
	return "", nil
}

// isShellControllerBlock reports whether v (a decoded modules.shell) is exactly the controller block:
// the keys allow, allow_arbitrary and, when interp is not "", shell, and no other key; allow is the one
// AID aid, allow_arbitrary is true and shell is interp.
func isShellControllerBlock(v any, aid, interp string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	want := 2
	if interp != "" {
		want = 3
		if s, ok := m["shell"].(string); !ok || s != interp {
			return false
		}
	}
	if len(m) != want {
		return false
	}
	allow, ok := m["allow"].([]any)
	if !ok || len(allow) != 1 {
		return false
	}
	if s, ok := allow[0].(string); !ok || s != aid {
		return false
	}
	b, ok := m["allow_arbitrary"].(bool)
	return ok && b
}

// writeShellBlock sets modules.shell in config.json to block, keeping every other key.
func writeShellBlock(l Layout, block json.RawMessage) error {
	raw, err := os.ReadFile(l.ConfigPath())
	if err != nil {
		return fmt.Errorf("anet: read config: %w", err)
	}
	cur, err := decodeObject(raw)
	if err != nil {
		return fmt.Errorf("anet: %s: %w", l.ConfigPath(), err)
	}
	b, err := decodeObject(block)
	if err != nil {
		return err
	}
	mods, _ := cur["modules"].(map[string]any)
	if mods == nil {
		mods = map[string]any{}
	}
	mods["shell"] = b
	cur["modules"] = mods
	out, err := marshalConfigObject(cur)
	if err != nil {
		return err
	}
	return writeFileAtomic(l.ConfigPath(), out, 0o600)
}
