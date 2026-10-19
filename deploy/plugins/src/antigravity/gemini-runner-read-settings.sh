# read_settings prints two lines, anetPath and identity. Each comes from the
# environment of the caller when it is set there, otherwise from the settings
# file of this extension (the .env file that Gemini CLI writes into the
# extension directory), which holds nothing else of the host.
gemini_dotenv_value() {
  [ -r "$1" ] || return 0
  sed -n "s/^$2=//p" "$1" | head -n 1 | sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\\(.*\\)'\$/\\1/" -e 's/\\\(["\\]\)/\1/g'
}

read_settings() {
  settings_bin=${ANET_PLUGIN_BIN:-}
  settings_identity=${ANET_PLUGIN_IDENTITY:-}
  # Only the .env file of the extension that holds this script; no other location is read.
  here=$(cd "$(dirname "$0")/.." 2>/dev/null && pwd) || here=
  if [ -n "$here" ] && [ -f "$here/.env" ]; then
    [ -n "$settings_bin" ] || settings_bin=$(gemini_dotenv_value "$here/.env" ANET_PLUGIN_BIN)
    [ -n "$settings_identity" ] || settings_identity=$(gemini_dotenv_value "$here/.env" ANET_PLUGIN_IDENTITY)
  fi
  printf '%s\n' "$settings_bin" "$settings_identity"
}
