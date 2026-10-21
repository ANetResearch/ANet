# Read-AnetSettings returns anetPath and identity from the user settings of
# Claude Code (settings.json in CLAUDE_CONFIG_DIR, default %USERPROFILE%\.claude,
# key pluginConfigs."anet@...".options), or two empty strings when the file is
# missing or unreadable or holds the options of this plugin under more than one
# marketplace.
function Read-AnetSettings {
  $dir = Get-AnetEnv 'CLAUDE_CONFIG_DIR'
  if ($dir -eq '') {
    $userHome = Get-AnetUserHome
    if ($userHome -eq '') { return @('', '') }
    $dir = Join-Path $userHome '.claude'
  }
  $entries = @(Get-AnetOwnEntries (Get-AnetMember (Read-AnetJsonFile (Join-Path $dir 'settings.json')) 'pluginConfigs'))
  if ($entries.Count -ne 1) { return @('', '') }
  return @(Get-AnetOptionValues (Get-AnetMember $entries[0] 'options'))
}
