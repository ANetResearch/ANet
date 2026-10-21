//go:build !no_mcp

package agentwire

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// ZCode (Z.ai). ZCode reads MCP servers from plugins (.mcp.json of a plugin,
// or mcpServers of its manifest) and from the parameters of a session, and
// from no user-level file (checked in the zcode.cjs of ZCode 3.15). anet is
// therefore wired as the anet plugin, from the ANet marketplace for ZCode,
// through ZCode's own command-line program:
//
//	zcode plugins marketplace add https://agentnetwork.org.cn/zcode/marketplace.json
//	zcode plugins install anet@agentnetwork
//
// and unwired with `zcode plugins uninstall anet@agentnetwork --force`
// (ZCode refuses to uninstall without --force when no terminal answers its
// question). The state comes from `zcode plugins list --json`. anet never
// reads or writes a file under ~/.zcode: the configuration there holds the
// user's model credentials.
//
// Without the program (the desktop application alone), nothing is written:
// an explicit wire fails with the two steps to take in the application, and
// wire --all skips ZCode with the same steps as a note when ~/.zcode exists.
//
// On Windows the program may be zcode.cmd. It is run with fixed arguments
// only (no path, no setting of the user), none of which cmd.exe would read
// as its own syntax, so the batch file is run like an executable.
type zcodeTool struct{}

const (
	zcodeMarketplaceURL  = "https://agentnetwork.org.cn/zcode/marketplace.json"
	zcodeMarketplaceName = "agentnetwork"
	zcodePluginName      = "anet"
	zcodePluginID        = zcodePluginName + "@" + zcodeMarketplaceName
)

// zcodeQueryTimeout bounds one read-only call of the program (a list).
const zcodeQueryTimeout = 60 * time.Second

// cliTool is a tool wired through its own CLI alone: without the CLI there
// is nothing to inspect.
type cliTool interface {
	cliOnlyTool()
}

func (zcodeTool) cliOnlyTool() {}

func (zcodeTool) name() string { return ToolZCode }

// configPath is empty: the plugin state is ZCode's, read through its CLI.
func (zcodeTool) configPath(*Options) string { return "" }

func zcodeCLI(o *Options) string {
	p, err := o.LookPath("zcode")
	if err != nil {
		return ""
	}
	return p
}

func (zcodeTool) detected(o *Options) bool { return zcodeCLI(o) != "" }

func (zcodeTool) skipNotes(o *Options) []Msg {
	if dirExists(filepath.Join(o.Home, ".zcode")) {
		return []Msg{msg("wire.note.zcode_desktop", "url", zcodeMarketplaceURL)}
	}
	return nil
}

// zcodePlugin is a plugin as `zcode plugins list --json` reports it.
type zcodePlugin struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// zcodeMarketplace is a marketplace as `zcode plugins marketplace list
// --json` reports it.
type zcodeMarketplace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// zcodeJSON runs a read-only command of the program and decodes the JSON
// array it prints. The program may print diagnostics around the array, and
// exits with 1 when it has error diagnostics about other plugins; the array
// is what counts.
func zcodeJSON(ctx context.Context, o *Options, cli string, into any, args ...string) error {
	out, err := o.Run(ctx, cli, args...)
	start := bytes.IndexByte(out, '[')
	if start >= 0 {
		if derr := json.NewDecoder(bytes.NewReader(out[start:])).Decode(into); derr == nil {
			return nil
		}
	}
	if err == nil {
		err = errNoJSON
	}
	return fail("wire_cli_failed", "command", cliCommand(append([]string{cli}, args...)), "error", cliError(err, out))
}

type zcodeError string

func (e zcodeError) Error() string { return string(e) }

const errNoJSON = zcodeError("the program printed no JSON list")

// anetPlugins are the installed plugins named anet, from any marketplace:
// the plugin of the ZCode official marketplace is the same plugin.
func zcodeAnetPlugins(ctx context.Context, o *Options, cli string) ([]zcodePlugin, error) {
	var all []zcodePlugin
	if err := zcodeJSON(ctx, o, cli, &all, "plugins", "list", "--json"); err != nil {
		return nil, err
	}
	var out []zcodePlugin
	for _, p := range all {
		if p.Name == zcodePluginName || strings.HasPrefix(p.ID, zcodePluginName+"@") {
			out = append(out, p)
		}
	}
	return out, nil
}

func (zcodeTool) planWire(o *Options) ([]change, error) {
	cli := zcodeCLI(o)
	if cli == "" {
		return nil, fail("wire_cli_failed.zcode_missing", "url", zcodeMarketplaceURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), zcodeQueryTimeout)
	defer cancel()
	plugins, err := zcodeAnetPlugins(ctx, o, cli)
	if err != nil {
		return nil, err
	}
	var run [][]string
	for _, p := range plugins {
		if p.Enabled {
			return nil, nil
		}
	}
	if len(plugins) > 0 {
		run = append(run, []string{cli, "plugins", "enable", plugins[0].ID})
	} else {
		var markets []zcodeMarketplace
		if err := zcodeJSON(ctx, o, cli, &markets, "plugins", "marketplace", "list", "--json"); err != nil {
			return nil, err
		}
		known := false
		for _, m := range markets {
			if m.ID == zcodeMarketplaceName || m.Name == zcodeMarketplaceName {
				known = true
			}
		}
		if !known {
			run = append(run, []string{cli, "plugins", "marketplace", "add", zcodeMarketplaceURL})
		}
		run = append(run, []string{cli, "plugins", "install", zcodePluginID})
	}
	var out []Msg
	return []change{{
		cliOnly: true, run: run, output: &out,
		confirm: func(ctx context.Context) error {
			ps, err := zcodeAnetPlugins(ctx, o, cli)
			if err != nil {
				return err
			}
			for _, p := range ps {
				if p.Enabled {
					return nil
				}
			}
			return fail("wire_verify_failed.zcode_add")
		},
		note: msg("wire.change.zcode_install", "name", zcodePluginID),
	}}, nil
}

func (zcodeTool) planUnwire(o *Options) ([]change, []Msg, error) {
	cli := zcodeCLI(o)
	if cli == "" {
		// Unwire --all reaches every tool; without the program there is
		// nothing anet can check or remove.
		if dirExists(filepath.Join(o.Home, ".zcode")) {
			return nil, []Msg{msg("wire.note.zcode_desktop_remove")}, nil
		}
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), zcodeQueryTimeout)
	defer cancel()
	plugins, err := zcodeAnetPlugins(ctx, o, cli)
	if err != nil || len(plugins) == 0 {
		return nil, nil, err
	}
	var run [][]string
	for _, p := range plugins {
		run = append(run, []string{cli, "plugins", "uninstall", p.ID, "--force"})
	}
	var out []Msg
	return []change{{
		cliOnly: true, run: run, output: &out,
		confirm: func(ctx context.Context) error {
			ps, err := zcodeAnetPlugins(ctx, o, cli)
			if err != nil {
				return err
			}
			if len(ps) > 0 {
				return fail("wire_verify_failed.zcode_remove")
			}
			return nil
		},
		note: msg("wire.change.zcode_uninstall", "name", plugins[0].ID),
	}}, nil, nil
}
