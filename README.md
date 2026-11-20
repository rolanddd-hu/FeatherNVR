# FeatherNVR

A lightweight, zero-transcode NVR built in Go + `go2rtc` for **Windows, Linux, and macOS** designed to replace heavy setups like Blue Iris when you only need RTSP/ONVIF cameras, motion-triggered recording with a rolling FIFO storage cap, and a fast mobile/desktop web interface on your LAN.

## Features

- **Out-of-the-box setup:** Release archives bundle `go2rtc` in `bin/`, and running FeatherNVR without an existing `config.yaml` automatically generates a starter config so you can open `http://localhost:8080` and click **+ Add Camera** (with LAN ONVIF scan, ONVIF Auto-Detect, and live stream Test preview).
- **Zero video re-encoding:** Main streams are copied directly from `go2rtc`'s fMP4 stream into crash-safe fragmented MP4 clips (`ftyp + moov + moof/mdat`) with an in-memory GOP ring buffer for pre-motion recording and a global rolling FIFO storage cap (editable live from the **Status** tab).
- **Flexible low-CPU motion detection:** Supports `hikvision_isapi`, `isapi_or_pixel`, `onvif`, `onvif_or_pixel`, and `pixel` (grayscale sub-stream diff with ignore zones and live score monitoring).
- **Web UI for desktop & mobile:**
  - **Fit All** full-viewport grid plus `Auto` / `1`–`4` column modes across both **Live** and **Recordings**.
  - **Edit Layout** mode to drag-and-swap (or tap-to-swap) camera tiles, persisted server-side in SQLite.
  - **Right-click context menu** (and Live modal **Settings** button) to edit per-camera streams, aspect ratio, motion, recording overrides, and PTZ live without restarting.
  - **PTZ controls** (D-pad, zoom, speed slider, presets) for Hikvision ISAPI and ONVIF PTZ cameras.
  - **Day-grouped Recordings** with a **24h / Fit Clips** zoomable timeline, seamless next-clip pre-buffering, `0.25x`–`8x` speeds, and frame-by-frame stepping (`,` / `.`).
  - **Large View** and pure stream-only **Fullscreen** modes with desktop mouse-wheel video zoom/pan (`1:1`–`8x`) and click-to-toggle/close.
- **Blue Iris importer:** Imports cameras, credentials, DVR channels, and PTZ configuration directly from the Windows Registry (on Windows) or from an exported `.reg` file (on any OS).

## Why It Uses Minimal CPU

- **Single RTSP connection per camera:** `go2rtc` ingests each camera once and fans it out to live WebRTC/MSE/HLS viewers, the recorder, and any pixel detector.
- **Hardware/camera-offloaded motion first:** Uses Hikvision DVR ISAPI `alertStream` or ONVIF `PullPoint` subscriptions at near-zero CPU when supported, falling back cleanly to 2-fps sub-stream pixel diff only when needed.

## Quick Start (Prebuilt GitHub Release — Recommended)

Prebuilt release archives for **Windows (`amd64`, `arm64`)**, **Linux (`amd64`, `arm64`)**, and **macOS (`amd64`, `arm64`)** already bundle `feathernvr` and `go2rtc`:

1. Download and extract the archive for your OS from **GitHub Releases**.
2. Launch FeatherNVR:
   - **Windows:** run `.\feathernvr.exe` (or `.\scripts\run.ps1`)
   - **Linux / macOS:** run `./feathernvr` (or `./scripts/run.sh`)
3. On first start, a starter `config.yaml` is created automatically. Open the printed URL (default `http://127.0.0.1:8080`) and click **+ Add Camera**.
4. *(Optional)* Install `ffmpeg` on `PATH` if you use `pixel` motion detection or FFmpeg thumbnail fallback.

## Building From Source

- **Go 1.27+** (pure-Go `modernc.org/sqlite`, builds everywhere with `CGO_ENABLED=0`).
- **`go2rtc`:** Place `go2rtc.exe` (Windows) or `go2rtc` (Linux/macOS) in `bin/` (or anywhere on `PATH`). Download from <https://github.com/AlexxIT/go2rtc/releases>.
- **`ffmpeg` (optional):** Only required for `pixel` / fallback motion detection and generating clip thumbnails (auto-detected in `bin/`, `PATH`, or standard install paths, or set `ffmpeg_bin` in `config.yaml`).

1. **Place `go2rtc` in `bin/`** (see above).
2. **Build FeatherNVR:**
   ```powershell
   # Windows
   .\scripts\build.ps1
   ```
   ```sh
   # Linux / macOS
   CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o feathernvr ./cmd/feathernvr
   ```
3. **Import existing cameras from Blue Iris (optional):**
   Directly from the Windows Registry (Windows):
   ```powershell
   .\scripts\import-blueiris.ps1 -MaxGB 50
   ```
   Or from an exported Blue Iris `.reg` file (cross-platform):
   ```powershell
   .\feathernvr.exe -import-blueiris .\blueiris-export.reg -import-out config.yaml -max-gb 50
   ```
   *(Pass `-Force` / `-force` to overwrite an existing `config.yaml`, creating a `.bak` backup.)*
4. **Or discover ONVIF cameras on your LAN:**
   ```powershell
   .\feathernvr.exe -discover
   ```
5. **Manual setup (if not using auto-generated `config.yaml` or Web UI `+ Add Camera`):**
   ```powershell
   Copy-Item .\configs\config.example.yaml .\config.yaml
   ```
6. **Open firewall ports on Windows (Administrator PowerShell) so phones on your LAN can view Web UI & WebRTC:**
   ```powershell
   .\scripts\setup-firewall.ps1
   ```
7. **Run FeatherNVR:**
   ```powershell
   .\scripts\run.ps1
   ```
   Open the printed LAN URL (`http://<your-pc-ip>:8080`) on your phone or desktop browser.

## Cutting a Release via Git

Push a version tag matching `v*` to trigger [release.yml](.github/workflows/release.yml):

```sh
git tag v0.1.0
git push origin v0.1.0
```

GitHub Actions will verify tests, cross-compile all 6 OS/arch packages with bundled `go2rtc`, generate `checksums.txt`, and publish the GitHub Release automatically.

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
