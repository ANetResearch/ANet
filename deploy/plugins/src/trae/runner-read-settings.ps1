# Read-AnetSettings returns anetPath and identity from the file "settings" that
# install.sh or install.ps1 of this bundle writes in the directory above bin.
# Its lines are "anetPath=<value>" and "identity=<value>"; the file is data and
# is never executed. Without the file, both settings are empty.
function Read-AnetSettings {
  $script = Get-AnetEnv 'ANET_RUN_SCRIPT'
  $values = @('', '')
  if ($script -eq '') { return $values }
  $file = Join-Path (Split-Path -Parent (Split-Path -Parent $script)) 'settings'
  if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { return $values }
  $seen = @($false, $false)
  foreach ($line in [IO.File]::ReadAllLines($file)) {
    $line = $line.TrimEnd("`r")
    if (-not $seen[0] -and $line.StartsWith('anetPath=')) { $values[0] = $line.Substring(9); $seen[0] = $true }
    elseif (-not $seen[1] -and $line.StartsWith('identity=')) { $values[1] = $line.Substring(9); $seen[1] = $true }
  }
  return $values
}
