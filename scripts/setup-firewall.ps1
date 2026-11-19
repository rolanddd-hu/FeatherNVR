# Run this script in an elevated (Administrator) PowerShell terminal to open
# FeatherNVR's HTTP Web UI (8080) and go2rtc WebRTC (8555) ports on Private networks.

param(
    [int]$WebPort = 8080,
    [int]$WebRTCPort = 8555
)

$ErrorActionPreference = "Stop"

$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Error "Please run this script as Administrator to add Windows Firewall rules."
}

New-NetFirewallRule -DisplayName "FeatherNVR Web UI (TCP $WebPort)" `
    -Direction Inbound -Protocol TCP -LocalPort $WebPort -Profile Private -Action Allow -ErrorAction Stop | Out-Null

New-NetFirewallRule -DisplayName "FeatherNVR WebRTC (TCP $WebRTCPort)" `
    -Direction Inbound -Protocol TCP -LocalPort $WebRTCPort -Profile Private -Action Allow -ErrorAction Stop | Out-Null

New-NetFirewallRule -DisplayName "FeatherNVR WebRTC (UDP $WebRTCPort)" `
    -Direction Inbound -Protocol UDP -LocalPort $WebRTCPort -Profile Private -Action Allow -ErrorAction Stop | Out-Null

Write-Host "Added Windows Firewall rules for TCP $WebPort and TCP/UDP $WebRTCPort (Private profile)." -ForegroundColor Green
