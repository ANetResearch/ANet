# Read-AnetCodeBuddyFile returns anetPath and identity from one settings file
# of the host (key pluginConfigs."anet@...".options), two empty strings when
# the file holds entries of this plugin from more than one marketplace, and
# nothing when the file is missing or unreadable or holds no entry of it.
function Read-AnetCodeBuddyFile([string]$Path) {
  $entries = @(Get-AnetOwnEntries (Get-AnetMember (Read-AnetJsonFile $Path) 'pluginConfigs'))
  if ($entries.Count -eq 0) { return $null }
  if ($entries.Count -ne 1) { return ,@('', '') }
  return ,@(Get-AnetOptionValues (Get-AnetMember $entries[0] 'options'))
}

# Read-AnetSettings reads the user settings of the host: the directory that
# WORKBUDDY_CONFIG_DIR or CODEBUDDY_CONFIG_DIR names when one is set, otherwise
# %USERPROFILE%\.codebuddy and %USERPROFILE%\.workbuddy. The runner cannot tell
# which of the two hosts started it, so when both files hold options of this
# plugin and the options differ, nothing is selected.
function Read-AnetSettings {
  foreach ($name in @('WORKBUDDY_CONFIG_DIR', 'CODEBUDDY_CONFIG_DIR')) {
    $dir = Get-AnetEnv $name
    if ($dir -ne '') {
      $r = Read-AnetCodeBuddyFile (Join-Path $dir 'settings.json')
      if ($null -eq $r) { return @('', '') }
      return $r
    }
  }
  $userHome = Get-AnetUserHome
  if ($userHome -eq '') { return @('', '') }
  $fromCodeBuddy = Read-AnetCodeBuddyFile (Join-Path (Join-Path $userHome '.codebuddy') 'settings.json')
  $fromWorkBuddy = Read-AnetCodeBuddyFile (Join-Path (Join-Path $userHome '.workbuddy') 'settings.json')
  if ($null -ne $fromCodeBuddy -and $null -ne $fromWorkBuddy -and (($fromCodeBuddy -join "`n") -cne ($fromWorkBuddy -join "`n"))) {
    return @('', '')
  }
  if ($null -ne $fromCodeBuddy) { return $fromCodeBuddy }
  if ($null -ne $fromWorkBuddy) { return $fromWorkBuddy }
  return @('', '')
}
