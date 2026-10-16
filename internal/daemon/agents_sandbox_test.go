//go:build mod4_pending

package daemon

// LEC AV-4, UP-2, UP-3 (EXE-6): each agent's configuration in the sandbox, its command line in a direct
// and in a sandboxed run, and the agents and configurations the sandbox does not run, which fail closed.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ANetResearch/ANet/internal/runtime/interactions"
)


// liveRoute is the injection route's address in the sandbox, as the agents' configuration must name it.
func liveRoute(name string) string { return "http://127.0.0.1:3128/anet-upstream/" + name }


// readAgentConfig reads the model service address, pinned model and key out of the configuration file an
// agent's prepare wrote (raw), parsed as the agent parses it, and checks the rest of what the file must
// say. model is the pinned model; gotModel is "" unless every place the file names it agrees.
func readAgentConfig(t *testing.T, agent string, raw []byte, model string) (gotBase, gotModel, gotKey string) {
	t.Helper()
	switch agent {
	case agentCodex:
		vals := tomlValues(t, string(raw))
		gotBase, gotModel, gotKey = vals["base_url"], vals["model"], egressPlaceholderKey
		for k, v := range map[string]string{"model_provider": "anet", "name": "anet", "wire_api": "responses",
			"env_key": "OPENAI_API_KEY", "supports_websockets": "false", "web_search": "disabled",
			"check_for_update_on_startup": "false"} {
			if vals[k] != v {
				t.Errorf("codex %s = %q, want %q", k, vals[k], v)
			}
		}
		if !strings.Contains(string(raw), "\n[model_providers.anet]\n") {
			t.Errorf("codex provider table missing:\n%s", raw)
		}
	case agentOpenCode:
		var c struct {
			Provider map[string]struct {
				NPM     string                           `json:"npm"`
				Options struct{ BaseURL, APIKey string } `json:"options"`
				Models  map[string]any                   `json:"models"`
			} `json:"provider"`
			Model      string            `json:"model"`
			Permission map[string]string `json:"permission"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		p := c.Provider["anet"]
		gotBase, gotKey = p.Options.BaseURL, p.Options.APIKey
		if _, ok := p.Models[model]; ok && c.Model == "anet/"+model {
			gotModel = model
		}
		if p.NPM != "@ai-sdk/openai-compatible" || c.Permission["bash"] != "allow" || c.Permission["edit"] != "allow" {
			t.Errorf("opencode configuration %s", raw)
		}
	case agentHermes:
		vals := yamlJSONValues(t, string(raw))
		gotBase, gotModel, gotKey = vals["base_url"], vals["default"], vals["api_key"]
		if !strings.HasPrefix(string(raw), "model:\n  provider: custom\n") {
			t.Errorf("hermes configuration %s", raw)
		}
	case agentOpenClaw:
		var c struct {
			Models struct {
				Providers map[string]struct {
					BaseURL string                `json:"baseUrl"`
					APIKey  string                `json:"apiKey"`
					API     string                `json:"api"`
					Models  []struct{ ID string } `json:"models"`
				} `json:"providers"`
			} `json:"models"`
			Agents struct {
				Defaults struct {
					Model struct{ Primary string } `json:"model"`
				} `json:"defaults"`
			} `json:"agents"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		p := c.Models.Providers["anet"]
		gotBase, gotKey = p.BaseURL, p.APIKey
		if len(p.Models) == 1 && p.Models[0].ID == model && c.Agents.Defaults.Model.Primary == "anet/"+model {
			gotModel = model
		}
		if p.API != "openai-completions" {
			t.Errorf("openclaw api %q", p.API)
		}
	}
	return gotBase, gotModel, gotKey
}

// tomlValues reads `key = value` lines (strings decoded) of the TOML prepareCodex writes.
func tomlValues(t *testing.T, s string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ln := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(ln, " = ")
		if !ok {
			continue
		}
		if strings.HasPrefix(v, `"`) {
			u, err := strconv.Unquote(v)
			if err != nil {
				t.Fatalf("%s: %v", ln, err)
			}
			v = u
		}
		out[k] = v
	}
	return out
}

// yamlJSONValues reads the `  key: "json string"` lines of the YAML prepareHermes writes.
func yamlJSONValues(t *testing.T, s string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ln := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), ": ")
		if !ok || !strings.HasPrefix(v, `"`) {
			continue
		}
		var u string
		if err := json.Unmarshal([]byte(v), &u); err != nil {
			t.Fatalf("%s: %v", ln, err)
		}
		out[k] = u
	}
	return out
}






// openCodeArgsStub parses its arguments as opencode run does (what follows "--" joins the message): --model takes a
// value, any other argument starting with "-" before "--" is an option it does not know (an error), the
// other words and everything after "--" are the message. It writes the message beside itself.
const openCodeArgsStub = `#!/bin/sh
here=$(dirname "$0")
[ "$1" = run ] || { echo "not a run" >&2; exit 2; }
shift
msg=""
while [ $# -gt 0 ]; do
  case "$1" in
    --) shift; for a in "$@"; do msg="$msg${msg:+ }$a"; done; break ;;
    --model) shift 2 ;;
    -*) echo "unknown option $1" >&2; exit 2 ;;
    *) msg="$msg${msg:+ }$1"; shift ;;
  esac
done
printf '%s' "$msg" > "$here/message"
echo STUB-OK
`

// hermesArgsStub parses its arguments as hermes does (argparse): -z/--oneshot and -m/--model take a value,
// given as the next argument or after "="; a next argument starting with "-" is not a value but an
// option, so -z is left without one (an error). It writes the one-shot message beside itself.
const hermesArgsStub = `#!/bin/sh
here=$(dirname "$0")
msg=""
while [ $# -gt 0 ]; do
  case "$1" in
    --oneshot=*) msg=${1#--oneshot=}; shift ;;
    -z|--oneshot)
      case "$2" in -*|"") echo "argument -z/--oneshot: expected one argument" >&2; exit 2 ;; esac
      msg=$2; shift 2 ;;
    -m|--model) shift 2 ;;
    *) echo "unrecognized arguments: $1" >&2; exit 2 ;;
  esac
done
printf '%s' "$msg" > "$here/message"
echo STUB-OK
`

// hermesArgparse is hermes's top-level parser as far as the one-shot run goes (-z/--oneshot,
// -m/--model and the options around them), run on Python's own argparse: it prints the one-shot message
// as JSON, or exits 2 as hermes does when its arguments do not parse.
const hermesArgparse = `
import argparse, json, sys
p = argparse.ArgumentParser(prog="hermes")
p.add_argument("-z", "--oneshot", metavar="PROMPT", default=None)
p.add_argument("-m", "--model", default=None)
p.add_argument("--provider", default=None)
p.add_argument("-t", "--toolsets", default=None)
p.add_argument("-s", "--skills", action="append", default=None)
p.add_argument("-r", "--resume", default=None)
p.add_argument("-c", "--continue", dest="continue_last", nargs="?", const=True, default=None)
p.add_argument("--usage-file", default=None)
p.add_argument("--yolo", action="store_true")
p.add_argument("-v", "--version", action="store_true")
ns = p.parse_args(sys.argv[1:])
print(json.dumps(ns.oneshot))
`

