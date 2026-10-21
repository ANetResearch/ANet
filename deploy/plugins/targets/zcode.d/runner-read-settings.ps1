# Read-AnetSettings returns anetPath and identity from the ZCode configuration
# (%USERPROFILE%\.zcode\cli\config.json, plugins.options."anet@..."), or two
# empty strings when the file is missing or unreadable or holds the options of
# this plugin under more than one marketplace.
function Read-AnetSettings {
  $file = Get-AnetEnv 'ZCODE_CONFIG_FILE'
  if ($file -eq '') {
    $userHome = Get-AnetUserHome
    if ($userHome -eq '') { return @('', '') }
    $file = Join-Path (Join-Path (Join-Path $userHome '.zcode') 'cli') 'config.json'
  }
  $entries = @(Get-AnetOwnEntries (Get-AnetMember (Get-AnetMember (Read-AnetJsonFile $file) 'plugins') 'options'))
  if ($entries.Count -ne 1) { return @('', '') }
  return @(Get-AnetOptionValues $entries[0])
}
