$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $localGo = Join-Path $root ".tools\go\bin\go.exe"
    if (Test-Path $localGo) {
        $goBin = $localGo
    } else {
        $cmd = Get-Command go -ErrorAction SilentlyContinue
        if (-not $cmd) {
            throw "Go toolchain not found in .tools\go\bin or PATH"
        }
        $goBin = $cmd.Source
    }

    $env:CGO_ENABLED = "0"
    Write-Host "Building feathernvr.exe with $goBin ..."
    & $goBin build -trimpath -ldflags="-s -w" -o (Join-Path $root "feathernvr.exe") ./cmd/feathernvr
    if ($LASTEXITCODE -ne 0) {
        throw "go build failed with exit code $LASTEXITCODE"
    }
    Write-Host "Built: $(Join-Path $root 'feathernvr.exe')"
}
finally {
    Pop-Location
}
