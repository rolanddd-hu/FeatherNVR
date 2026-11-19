# FeatherNVR

A lightweight, zero-transcode Windows NVR built in Go + `go2rtc` designed to replace heavy setups like Blue Iris when you only need RTSP/ONVIF cameras, motion-triggered recording with a rolling FIFO storage cap, and a fast mobile web interface on your LAN.

## Features

- **Zero video re-encoding:** Main streams are copied directly from `go2rtc`'s fMP4 stream into crash-safe fragmented MP4 clips (`ftyp + moov + moof/mdat`) with an in-memory GOP ring buffer for pre-motion recording and a global rolling FIFO storage cap (editable live from the **Status** tab).
- **Flexible low-CPU motion detection:** Supports `hikvision_isapi`, `isapi_or_pixel`, `onvif`, `onvif_or_pixel`, and `pixel` (2-fps grayscale sub-stream diff with ignore zones and live score monitoring).
- **Web UI for desktop & mobile:**
  - **Fit All** full-viewport live grid plus `Auto` / `1`–`4` column modes.
  - **Edit Layout** mode to drag-and-swap (or tap-to-swap) camera tiles, persisted server-side in SQLite.
  - **PTZ controls** (D-pad, zoom, speed slider, presets) for Hikvision ISAPI and ONVIF PTZ cameras.
  - **Day-grouped Recordings** with a **24h / Fit Clips** timeline, desktop mouse-scroll timeline zoom, and continuous playback across the day's clips (`1x`–`8x`).
  - **Large View** and pure stream-only **Fullscreen** modes with desktop mouse-wheel video zoom/pan (`1:1`–`8x`) and click-to-toggle/close.
- **Blue Iris importer:** Imports cameras, credentials, DVR channels, and PTZ configuration directly from the Windows Registry or an exported `.reg` file.

## Why It Uses Minimal CPU

- **Single RTSP connection per camera:** `go2rtc` ingests each camera once and fans it out to live WebRTC/MSE/HLS viewers, the recorder, and any pixel detector.
- **Hardware/camera-offloaded motion first:** Uses Hikvision DVR ISAPI `alertStream` or ONVIF `PullPoint` subscriptions at near-zero CPU when supported, falling back cleanly to 2-fps sub-stream pixel diff only when needed.

## Prerequisites

- **Go 1.22+** (Go 1.27+ module toolchain; pure-Go `modernc.org/sqlite`, builds cleanly with `CGO_ENABLED=0`).
- **`go2rtc.exe`:** Download the Windows amd64 release binary from <https://github.com/AlexxIT/go2rtc/releases> and place it at `bin\go2rtc.exe`.
- **`ffmpeg.exe` (optional):** Only required for `pixel` / fallback motion detection and generating clip thumbnails (auto-detected on `PATH` or common Windows install paths, or set `ffmpeg_bin` in `config.yaml`).

## Quick Start (Windows)

1. **Place `go2rtc.exe` in `bin\go2rtc.exe`** (see Prerequisites above).
2. **Build `feathernvr.exe`:**
   ```powershell
   .\scripts\build.ps1
   ```
3. **Import existing cameras from Blue Iris (optional):**
   Directly from the Windows Registry:
   ```powershell
   .\scripts\import-blueiris.ps1 -MaxGB 50
   ```
   Or from an exported Blue Iris `.reg` file:
   ```powershell
   .\feathernvr.exe -import-blueiris .\blueiris-export.reg -import-out config.yaml -max-gb 50
   ```
   *(Pass `-Force` / `-force` to overwrite an existing `config.yaml`, creating a `.bak` backup.)*
4. **Or discover ONVIF cameras on your LAN:**
   ```powershell
   .\feathernvr.exe -discover
   ```
5. **Manual setup (if not importing from Blue Iris):**
   ```powershell
   Copy-Item .\configs\config.example.yaml .\config.yaml
   ```
   Edit `config.yaml` with your Hikvision DVR channels, Hikvision PTZ, and generic ONVIF cameras.
6. **Open firewall ports (Administrator PowerShell) so phones on your LAN can view Web UI & WebRTC:**
   ```powershell
   .\scripts\setup-firewall.ps1
   ```
7. **Run FeatherNVR:**
   ```powershell
   .\scripts\run.ps1
   ```
   Open the printed LAN URL (`http://<your-pc-ip>:8080`) on your phone or desktop browser. You can adjust the **Storage Cap (GB)** anytime from the **Status** tab in the Web UI.

## Camera Tips (Hikvision DVR, Hikvision PTZ & Generic ONVIF)

- **Codec recommendation for mobile compatibility:** Set camera **sub-streams** to **H.264** (not H.265) so live grids play smoothly via WebRTC/MSE across both Android Chrome and iOS Safari. Set **I-frame interval (keyframe interval)** to **1–2 seconds** (e.g., 15 fps with I-frame interval 30) for fast live startup and accurate pre-roll buffers.
- **Hikvision DVR analog HD channels:**
  - Channel URLs are typically `rtsp://user:pass@DVR_IP:554/Streaming/Channels/101` (channel 1 main), `102` (channel 1 sub), `201`/`202` (channel 2), etc.
  - Use `motion.mode: "isapi_or_pixel"` (or `"hikvision_isapi"` with VMD enabled on the DVR).
- **Generic ONVIF cameras (e.g., Keview):**
  - Use `motion.mode: "onvif_or_pixel"` so FeatherNVR tries ONVIF PullPoint events first and automatically switches to 2-fps sub-stream pixel diff if a camera's firmware doesn't implement ONVIF PullPoint events.

## Run Automatically at Windows Boot

In an elevated (Administrator) PowerShell:
```powershell
.\scripts\install-service.ps1
Start-ScheduledTask -TaskName FeatherNVR
```
