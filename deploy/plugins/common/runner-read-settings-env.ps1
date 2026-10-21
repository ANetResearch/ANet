# Read-AnetSettings returns anetPath and identity from the environment of the
# caller. A host without a plugin settings store passes the two values as
# ANET_PLUGIN_BIN and ANET_PLUGIN_IDENTITY.
function Read-AnetSettings {
  return @((Get-AnetEnv 'ANET_PLUGIN_BIN'), (Get-AnetEnv 'ANET_PLUGIN_IDENTITY'))
}
