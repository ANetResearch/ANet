# read_settings_file prints two lines, anetPath and identity, from the file
# that is named by its argument, and returns 0. It returns 1 when the file or
# a JSON reader is missing or holds no option of this plugin. When several
# entries of this plugin exist (installed from different marketplaces), it
# prints two empty lines: an ambiguous store selects nothing.
read_settings_file() {
  [ -r "$1" ] || return 1
  if command -v python3 >/dev/null 2>&1 && { [ "$(uname -s)" != Darwin ] || xcode-select -p >/dev/null 2>&1; }; then
    python3 -I - "$1" <<'PY' 2>/dev/null
import json, sys
try:
    configs = json.load(open(sys.argv[1], encoding="utf-8")).get("pluginConfigs", {})
except Exception:
    sys.exit(1)
found = [v for k, v in sorted(configs.items()) if k.startswith("anet@") and isinstance(v, dict)]
if not found:
    sys.exit(1)
entry = found[0].get("options") if len(found) == 1 else None
entry = entry if isinstance(entry, dict) else {}
for key in ("anetPath", "identity"):
    value = entry.get(key, "")
    print(value if isinstance(value, str) and "\n" not in value and "\r" not in value else "")
PY
    return $?
  fi
  if command -v plutil >/dev/null 2>&1; then
    # Same rule as above: an entry of exactly one marketplace is read, and only string values count.
    found=
    count=0
    for market in agentnetwork codebuddy-plugins-official; do
      if plutil -extract "pluginConfigs.anet@$market" json -o /dev/null "$1" >/dev/null 2>&1; then
        found=$market
        count=$((count + 1))
      fi
    done
    [ "$count" -gt 0 ] || return 1
    for key in anetPath identity; do
      value=
      if [ "$count" -eq 1 ] && [ "$(plutil -type "pluginConfigs.anet@$found.options.$key" "$1" 2>/dev/null)" = string ]; then
        value=$(plutil -extract "pluginConfigs.anet@$found.options.$key" raw -o - "$1" 2>/dev/null) || value=
      fi
      # A value with a line break is not a valid path or name.
      case $value in
        *"
"*|*"$(printf '\r')"*) value= ;;
      esac
      printf '%s\n' "$value"
    done
    return 0
  fi
  return 1
}

# read_settings prints the two lines from the user settings of the host: the
# directory that WORKBUDDY_CONFIG_DIR or CODEBUDDY_CONFIG_DIR names when one is
# set, otherwise ~/.codebuddy or ~/.workbuddy. The runner cannot tell which of
# the two hosts started it, so when both files hold options of this plugin and
# the options differ, nothing is selected (two empty lines). It prints nothing
# when no settings file holds an option of this plugin.
read_settings() {
  if [ -n "${WORKBUDDY_CONFIG_DIR:-}" ]; then
    read_settings_file "$WORKBUDDY_CONFIG_DIR/settings.json"
  elif [ -n "${CODEBUDDY_CONFIG_DIR:-}" ]; then
    read_settings_file "$CODEBUDDY_CONFIG_DIR/settings.json"
  else
    from_codebuddy=$(read_settings_file "${HOME:-}/.codebuddy/settings.json") && have_codebuddy=1 || have_codebuddy=0
    from_workbuddy=$(read_settings_file "${HOME:-}/.workbuddy/settings.json") && have_workbuddy=1 || have_workbuddy=0
    if [ "$have_codebuddy" -eq 1 ] && [ "$have_workbuddy" -eq 1 ] && [ "$from_codebuddy" != "$from_workbuddy" ]; then
      printf '\n\n'
    elif [ "$have_codebuddy" -eq 1 ]; then
      printf '%s\n' "$from_codebuddy"
    elif [ "$have_workbuddy" -eq 1 ]; then
      printf '%s\n' "$from_workbuddy"
    fi
  fi
  return 0
}
