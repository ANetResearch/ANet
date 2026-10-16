package daemon

// init_without.go is the offline write of the `without` list (MOD-7a):
// `anet init --without a,b` adds names, `anet init --with a` removes one.
// Nothing else writes the list: no control route, MCP tool or local A2A
// method does (Config.Without). The write happens only while no daemon holds
// the data directory, and every refusal comes before the first byte is
// written, so a refused command leaves the directory as it was.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/module"
)

// WithoutChange is what an init with --without/--with did to the list.
type WithoutChange struct {
	// Added and Removed are the names this call put into and took out of the
	// list; a name that was already there (or already absent) is in neither.
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	// Without is the list as it is on disk afterwards, sorted.
	Without []string `json:"without"`
}

// InitWithout is InitLayout plus an edit of `without`: names in add are
// added (only ever added: a name already present stays), names in remove are
// removed (only those names). Refusals, all before anything is written:
//
//   - config_invalid: a name that is not in the name table, listed twice, a
//     name in both lists, or an added name whose absent path has not landed
//     (AbsenceReady);
//   - daemon_running: a daemon holds the data directory;
//   - without_configured: an added name whose configuration key is already
//     in config.json (the node would not start on it).
func InitWithout(l Layout, add, remove []string) (InitReport, WithoutChange, error) {
	var ch WithoutChange
	if err := checkWithoutNames(add, remove); err != nil {
		return InitReport{}, ch, err
	}
	// The lock is held until the write is done, so a daemon cannot start
	// between the check and the write. A directory that does not exist yet
	// has no daemon.
	if _, err := os.Stat(l.Root); err == nil {
		lock, err := AcquireDataLock(l)
		if errors.Is(err, ErrDataLockHeld) {
			return InitReport{}, ch, a2ashape.Refuse("daemon_running", a2ashape.Args{"path": l.Root})
		}
		if err != nil {
			return InitReport{}, ch, err
		}
		defer lock.Release()
	}

	cur := map[string]any{}
	raw, err := os.ReadFile(l.ConfigPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return InitReport{}, ch, fmt.Errorf("anet: read config: %w", err)
	default:
		if cur, err = decodeObject(raw); err != nil {
			return InitReport{}, ch, fmt.Errorf("anet: %s: %w (nothing was changed)", l.ConfigPath(), err)
		}
	}
	have, err := withoutOf(cur)
	if err != nil {
		return InitReport{}, ch, err
	}
	for _, n := range add {
		if slices.Contains(have, n) {
			continue
		}
		if key := configuredKey(cur, n); key != "" {
			return InitReport{}, ch, a2ashape.Refuse("without_configured", a2ashape.Args{"key": key, "m": n})
		}
	}

	rep, err := InitLayout(l)
	if err != nil {
		return rep, ch, err
	}
	// InitLayout may have written the file; edit what is on disk now.
	if raw, err = os.ReadFile(l.ConfigPath()); err != nil {
		return rep, ch, fmt.Errorf("anet: read config: %w", err)
	}
	if cur, err = decodeObject(raw); err != nil {
		return rep, ch, err
	}
	next := slices.Clone(have)
	ch.Added, ch.Removed = []string{}, []string{}
	for _, n := range add {
		if !slices.Contains(next, n) {
			next = append(next, n)
			ch.Added = append(ch.Added, n)
		}
	}
	for _, n := range remove {
		if i := slices.Index(next, n); i >= 0 {
			next = slices.Delete(next, i, i+1)
			ch.Removed = append(ch.Removed, n)
		}
	}
	sort.Strings(next)
	sort.Strings(ch.Added)
	sort.Strings(ch.Removed)
	ch.Without = next
	if len(ch.Added) == 0 && len(ch.Removed) == 0 {
		return rep, ch, nil
	}
	if len(next) == 0 {
		delete(cur, "without")
	} else {
		cur["without"] = next
	}
	out, err := marshalConfigObject(cur)
	if err != nil {
		return rep, ch, err
	}
	if err := writeFileAtomic(l.ConfigPath(), out, 0o600); err != nil {
		return rep, ch, err
	}
	rep.Wrote = true
	for _, n := range ch.Added {
		rep.Changes = append(rep.Changes, InitChange{Key: "without", Action: "added", Value: n})
	}
	for _, n := range ch.Removed {
		rep.Changes = append(rep.Changes, InitChange{Key: "without", Action: "removed", Value: n,
			Note: "module turned on again"})
	}
	return rep, ch, nil
}

// checkWithoutNames validates the two lists.
func checkWithoutNames(add, remove []string) error {
	if len(add) > 0 {
		if _, err := module.NewAvailability(add); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, n := range remove {
		if _, ok := module.Lookup(n); !ok {
			return a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "with",
				"rule": ruleOf("没有叫 "+cleanKey(n)+" 的模块", "there is no module named "+cleanKey(n))})
		}
		if seen[n] {
			return a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "with",
				"rule": ruleOf("模块 "+n+" 写了不止一次", "the module "+n+" is listed more than once")})
		}
		seen[n] = true
	}
	for _, n := range add {
		if seen[n] {
			return a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "without",
				"rule": ruleOf("模块 "+n+" 同时出现在 --without 与 --with 里", "the module "+n+" is in both --without and --with")})
		}
	}
	return nil
}

func ruleOf(zh, en string) string {
	if a2ashape.Lang() == a2ashape.Chinese {
		return zh
	}
	return en
}

// withoutOf reads the `without` list of a decoded config.json; a value that
// is not a list of strings is config_invalid.
func withoutOf(cur map[string]any) ([]string, error) {
	v, ok := cur["without"]
	if !ok || v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "without",
			"rule": ruleOf("应为模块名列表", "expected a list of module names")})
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, a2ashape.Refuse("config_invalid", a2ashape.Args{"key": "without",
				"rule": ruleOf("应为模块名列表", "expected a list of module names")})
		}
		out = append(out, s)
	}
	return out, nil
}

// configuredKey is the configuration key of module n that config.json
// already has, and "" when it has none. stream_preview counts only when it
// is set to something other than off: a fresh config.json spells out "off".
func configuredKey(cur map[string]any, n string) string {
	info, ok := module.Lookup(n)
	if !ok || info.ConfigKey == "" {
		return ""
	}
	if blockName, nested := strings.CutPrefix(info.ConfigKey, "modules."); nested {
		if m, ok := cur["modules"].(map[string]any); ok {
			if v, has := m[blockName]; has && v != nil {
				return info.ConfigKey
			}
		}
		return ""
	}
	v, has := cur[info.ConfigKey]
	if !has || v == nil {
		return ""
	}
	if info.ConfigKey == "stream_preview" {
		if s, _ := v.(string); s == "" || s == streamPreviewOff {
			return ""
		}
	}
	return info.ConfigKey
}

// marshalConfigObject renders a config.json object and checks that this
// daemon still loads the result.
func marshalConfigObject(cur map[string]any) ([]byte, error) {
	out, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return nil, err
	}
	var check Config
	if err := json.Unmarshal(out, &check); err != nil {
		return nil, fmt.Errorf("the completed config would not load: %w", err)
	}
	return out, nil
}
