# Run this script as Administrator to register FeatherNVR as a Windows Scheduled Task
# that starts automatically at boot in the background.

param(
    [string]$TaskName = "FeatherNVR"
)

$ErrorActionPreference = "Stop"

$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Error "Please run this script as Administrator to register a Scheduled Task."
}

$root = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $root "feathernvr.exe"
$cfg = Join-Path $root "config.yaml"

if (-not (Test-Path $exe)) {
    throw "feathernvr.exe not found at $exe. Run .\scripts\build.ps1 first."
}
if (-not (Test-Path $cfg)) {
    throw "config.yaml not found at $cfg. Copy configs\config.example.yaml to config.yaml first."
}

$action = New-ScheduledTaskAction -Execute $exe -Argument "-config `"$cfg`"" -WorkingDirectory $root
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 `
    -RestartInterval (New-TimeSpan -Minutes 1)

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -User "SYSTEM" -RunLevel Highest -Force | Out-Null

Write-Host "Scheduled Task '$TaskName' registered. Start now with: Start-ScheduledTask -TaskName $TaskName" -ForegroundColor Green
