param(
    [string]$Source = "registry",
    [string]$Out = "config.yaml",
    [double]$MaxGB = 50,
    [switch]$Force
)

$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $exe = Join-Path $root "feathernvr.exe"
    if (-not (Test-Path $exe)) {
        Write-Host "feathernvr.exe not found; running build.ps1 first..."
        & (Join-Path $PSScriptRoot "build.ps1")
    }

    $args = @("-import-blueiris", $Source, "-import-out", $Out, "-max-gb", "$MaxGB")
    if ($Force) {
        $args += "-force"
    }

    & $exe @args
    if ($LASTEXITCODE -ne 0) {
        throw "Blue Iris import failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}
