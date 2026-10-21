# Read-AnetSettings returns anetPath and identity. Each comes from the
# environment of the caller when it is set there, otherwise from the settings
# file of this extension (the .env file that Gemini CLI writes into the
# extension directory), which holds nothing else of the host.
function Get-AnetDotenvValue([string]$File, [string]$Key) {
  foreach ($line in [IO.File]::ReadAllLines($File)) {
    if (-not $line.StartsWith($Key + '=')) { continue }
    $v = $line.Substring($Key.Length + 1).TrimEnd("`r")
    if ($v.Length -ge 2 -and $v.StartsWith('"') -and $v.EndsWith('"')) {
      $v = $v.Substring(1, $v.Length - 2) -replace '\\(["\\])', '$1'
    } elseif ($v.Length -ge 2 -and $v.StartsWith("'") -and $v.EndsWith("'")) {
      $v = $v.Substring(1, $v.Length - 2)
    }
    return $v
  }
  return ''
}

function Read-AnetSettings {
  $values = @((Get-AnetEnv 'ANET_PLUGIN_BIN'), (Get-AnetEnv 'ANET_PLUGIN_IDENTITY'))
  $script = Get-AnetEnv 'ANET_RUN_SCRIPT'
  if ($script -eq '') { return $values }
  # Only the .env file of the extension that holds this script; no other location is read.
  $file = Join-Path (Split-Path -Parent (Split-Path -Parent $script)) '.env'
  if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { return $values }
  if ($values[0] -eq '') { $values[0] = Get-AnetDotenvValue $file 'ANET_PLUGIN_BIN' }
  if ($values[1] -eq '') { $values[1] = Get-AnetDotenvValue $file 'ANET_PLUGIN_IDENTITY' }
  return $values
}
