zcode_config=${ZCODE_CONFIG_FILE:-${HOME:-}/.zcode/cli/config.json}

# read_settings prints two lines, anetPath and identity, from the ZCode
# configuration. It prints nothing when the file or a JSON reader is missing.
read_settings() {
  [ -r "$zcode_config" ] || return 0
  if command -v python3 >/dev/null 2>&1 && { [ "$(uname -s)" != Darwin ] || xcode-select -p >/dev/null 2>&1; }; then
    python3 -I - "$zcode_config" <<'PY' 2>/dev/null
import json, sys
try:
    options = json.load(open(sys.argv[1], encoding="utf-8")).get("plugins", {}).get("options", {})
except Exception:
    sys.exit(0)
found = [v for k, v in sorted(options.items()) if k.startswith("anet@") and isinstance(v, dict)]
entry = found[0] if len(found) == 1 else {}
for key in ("anetPath", "identity"):
    value = entry.get(key, "")
    # One line per setting: a value with a line break is not a valid path or name.
    print(value if isinstance(value, str) and "\n" not in value and "\r" not in value else "")
PY
    return 0
  fi
  if command -v plutil >/dev/null 2>&1; then
    # Same rule as above: an entry of exactly one marketplace is read, and only string values count.
    found=
    count=0
    for market in zcode-plugins-official agentnetwork; do
      if plutil -extract "plugins.options.anet@$market" json -o /dev/null "$zcode_config" >/dev/null 2>&1; then
        found=$market
        count=$((count + 1))
      fi
    done
    for key in anetPath identity; do
      value=
      if [ "$count" -eq 1 ] && [ "$(plutil -type "plugins.options.anet@$found.$key" "$zcode_config" 2>/dev/null)" = string ]; then
        value=$(plutil -extract "plugins.options.anet@$found.$key" raw -o - "$zcode_config" 2>/dev/null) || value=
      fi
      # A value with a line break is not a valid path or name.
      case $value in
        *"
"*|*"$(printf '\r')"*) value= ;;
      esac
      printf '%s\n' "$value"
    done
  fi
  return 0
}
