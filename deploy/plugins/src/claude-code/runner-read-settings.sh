claude_settings=${CLAUDE_CONFIG_DIR:-${HOME:-}/.claude}/settings.json

# read_settings prints two lines, anetPath and identity, from the user settings
# of Claude Code. It prints nothing when the file or a JSON reader is missing.
read_settings() {
  [ -r "$claude_settings" ] || return 0
  if command -v python3 >/dev/null 2>&1 && { [ "$(uname -s)" != Darwin ] || xcode-select -p >/dev/null 2>&1; }; then
    python3 -I - "$claude_settings" <<'PY' 2>/dev/null
import json, sys
try:
    configs = json.load(open(sys.argv[1], encoding="utf-8")).get("pluginConfigs", {})
except Exception:
    sys.exit(0)
found = [v.get("options") for k, v in sorted(configs.items()) if k.startswith("anet@") and isinstance(v, dict)]
entry = found[0] if len(found) == 1 and isinstance(found[0], dict) else {}
for key in ("anetPath", "identity"):
    value = entry.get(key, "")
    print(value if isinstance(value, str) and "\n" not in value and "\r" not in value else "")
PY
    return 0
  fi
  if command -v plutil >/dev/null 2>&1; then
    # Only string values count; an error message of plutil is never used as a value.
    if plutil -extract "pluginConfigs.anet@agentnetwork.options" json -o /dev/null "$claude_settings" >/dev/null 2>&1; then
      for key in anetPath identity; do
        value=
        if [ "$(plutil -type "pluginConfigs.anet@agentnetwork.options.$key" "$claude_settings" 2>/dev/null)" = string ]; then
          value=$(plutil -extract "pluginConfigs.anet@agentnetwork.options.$key" raw -o - "$claude_settings" 2>/dev/null) || value=
        fi
        # A value with a line break is not a valid path or name.
        case $value in
          *"
"*|*"$(printf '\r')"*) value= ;;
        esac
        printf '%s\n' "$value"
      done
    fi
  fi
  return 0
}
