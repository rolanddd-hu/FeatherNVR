$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $exe = Join-Path $root "feathernvr.exe"
    if (-not (Test-Path $exe)) {
        Write-Host "feathernvr.exe not found; running build.ps1 first..."
        & (Join-Path $PSScriptRoot "build.ps1")
    }

    $cfg = Join-Path $root "config.yaml"
    & $exe -config $cfg @args
}
finally {
    Pop-Location
}
