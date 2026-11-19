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
    if (-not (Test-Path $cfg)) {
        Write-Host "config.yaml not found." -ForegroundColor Yellow
        Write-Host "Copy configs\config.example.yaml to config.yaml and edit your cameras first:" -ForegroundColor Yellow
        Write-Host "  Copy-Item .\configs\config.example.yaml .\config.yaml"
        exit 1
    }

    & $exe -config $cfg
}
finally {
    Pop-Location
}
