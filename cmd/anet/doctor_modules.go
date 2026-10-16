package main

// doctor_modules.go is doctor's row for the optional modules (MOD-F1,
// MOD-F10; ux §5.2 "modules"): ok with the modules this binary contains,
// info with the ones config.json turns off, and fail — the sentence the
// daemon refuses to start with — for a configuration block of a module that
// is not available. And capabilities.files, which needs the file-receiving
// service module.

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/ANetResearch/ANet/internal/a2ashape"
	"github.com/ANetResearch/ANet/internal/daemon"
	"github.com/ANetResearch/ANet/module"
)

// modulesChecks adds the modules row and returns cfg without the
// configuration blocks of modules that are not available, which the checks
// that read them would otherwise report a second time (or read with no
// module behind them).
func modulesChecks(add func(id, status, detail, hint string), say func(id, status, key string, args a2ashape.Args),
	rep *doctorReport, cfg daemon.Config, av module.Availability) daemon.Config {
	refusals := daemon.ModuleRefusals(cfg)
	for _, r := range refusals {
		say("modules", stFail, r.Key, r.Args)
	}
	if len(refusals) == 0 {
		add("modules", stOK, compiledModulesReport(), "")
	}
	if len(rep.Without) > 0 {
		add("modules", stInfo, "turned off in config.json (without): "+strings.Join(rep.Without, ", "), "")
	}
	live := cfg
	if !av.Available("autoreply") {
		live.AutoReply = nil // its block is the module_absent.configured fail above, not a configuration to check
	}
	live.Modules = daemon.ModulesConfig{}
	for n, raw := range cfg.Modules {
		if av.Available(n) {
			live.Modules[n] = raw
		}
	}
	return live
}

// capabilitiesFilesChecks adds capabilities.files_not_service.absent for
// each capability that declares files while the service module — the one
// module whose providers can receive them — is not available. The
// declaration is read from the configuration file: a capability of the
// inbound.public_capabilities list, or of modules.service.capabilities, with
// "files" set.
func capabilitiesFilesChecks(say func(id, status, key string, args a2ashape.Args), av module.Availability, configPath string) {
	reason := av.Absence("service")
	if reason == "" {
		return
	}
	for _, name := range declaredFilesCapabilities(configPath) {
		say("capabilities.files", stFail, "capabilities.files_not_service.absent",
			a2ashape.Args{"name": name, "m": "service", "reason": reason})
	}
}

// declaredFilesCapabilities are the ids of the capabilities config.json
// declares with files, sorted; none when the file does not say.
func declaredFilesCapabilities(configPath string) []string {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	var c struct {
		Inbound struct {
			Public []json.RawMessage `json:"public_capabilities"`
		} `json:"inbound"`
		Modules struct {
			Service struct {
				Capabilities []json.RawMessage `json:"capabilities"`
			} `json:"service"`
		} `json:"modules"`
	}
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, list := range [][]json.RawMessage{c.Inbound.Public, c.Modules.Service.Capabilities} {
		for _, raw := range list {
			var cp struct {
				ID    string `json:"id"`
				Files any    `json:"files"`
			}
			if json.Unmarshal(raw, &cp) != nil || cp.ID == "" {
				continue
			}
			if on, ok := cp.Files.(bool); (ok && !on) || cp.Files == nil {
				continue
			}
			seen[cp.ID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
