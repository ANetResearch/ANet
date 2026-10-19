# read_settings prints two lines, anetPath and identity, from the environment
# of the caller. A host without a plugin settings store passes the two values
# as ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY.
read_settings() {
  printf '%s\n' "${ANET_PLUGIN_BIN:-}" "${ANET_PLUGIN_IDENTITY:-}"
}
