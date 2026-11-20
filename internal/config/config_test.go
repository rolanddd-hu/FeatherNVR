package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseDefaultsAndOverrides(t *testing.T) {
	yamlData := `
server:
  listen: ":9090"

hikvision_dvrs:
  - id: dvr1
    host: 192.168.1.50
    username: admin
    password: secret

cameras:
  - id: gate_ptz
    name: Gate PTZ
    main_rtsp: rtsp://admin:pw@192.168.1.60:554/Streaming/Channels/101
    sub_rtsp: rtsp://admin:pw@192.168.1.60:554/Streaming/Channels/102
    pre_record_sec: 0
    post_record_sec: 15
    motion:
      mode: onvif
      onvif_url: http://192.168.1.60/onvif/device_service
      username: admin
      password: pw

  - id: analog_ch1
    main_rtsp: rtsp://admin:secret@192.168.1.50:554/Streaming/Channels/101
    motion:
      mode: hikvision_isapi
      dvr: dvr1
      channel: 1

  - id: keview_yard
    main_rtsp: rtsp://admin:pw@192.168.1.70:554/stream1
    motion:
      onvif_url: http://192.168.1.70:8899/onvif/device_service
      ignore_zones:
        - [0.0, 0.0, 1.0, 0.15]
`

	cfg, err := Parse([]byte(yamlData))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if cfg.Server.Listen != ":9090" {
		t.Fatalf("unexpected listen: %q", cfg.Server.Listen)
	}
	if cfg.Go2RTC.APIPort != 1984 || cfg.Go2RTC.RTSPPort != 8554 || cfg.Go2RTC.WebRTCPort != 8555 {
		t.Fatalf("unexpected go2rtc defaults: %+v", cfg.Go2RTC)
	}
	if cfg.Storage.MaxGB != 100 || cfg.Storage.HighWatermarkPct != 98 || cfg.Storage.LowWatermarkPct != 92 {
		t.Fatalf("unexpected storage defaults: %+v", cfg.Storage)
	}
	if cfg.HikvisionDVRs[0].Port != 80 {
		t.Fatalf("expected default DVR port 80, got %d", cfg.HikvisionDVRs[0].Port)
	}

	cam0 := cfg.Cameras[0]
	if !cam0.IsEnabled() {
		t.Fatalf("expected cam0 enabled by default")
	}
	if cam0.EffectivePreRecordSec(cfg.Recording) != 0 {
		t.Fatalf("expected explicit 0 pre_record_sec override, got %d", cam0.EffectivePreRecordSec(cfg.Recording))
	}
	if cam0.EffectivePostRecordSec(cfg.Recording) != 15 {
		t.Fatalf("expected 15 post_record_sec override")
	}
	if cam0.EffectiveMaxClipSec(cfg.Recording) != 300 {
		t.Fatalf("expected default 300 max_clip_sec")
	}

	cam1 := cfg.Cameras[1]
	if cam1.Name != "analog_ch1" {
		t.Fatalf("expected name fallback to id, got %q", cam1.Name)
	}
	if cam1.EffectiveSubRTSP() != cam1.MainRTSP {
		t.Fatalf("expected sub_rtsp fallback to main_rtsp")
	}

	cam2 := cfg.Cameras[2]
	if cam2.Motion.Mode != MotionModeONVIFOrPixel {
		t.Fatalf("expected default mode onvif_or_pixel when onvif_url is set, got %q", cam2.Motion.Mode)
	}
	if cam2.Motion.FPS != 2 || cam2.Motion.Width != 160 || cam2.Motion.Height != 90 || cam2.Motion.Threshold != 18 || cam2.Motion.MinAreaPercent != 0.35 {
		t.Fatalf("unexpected motion pixel defaults: %+v", cam2.Motion)
	}
}

func TestParseValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "invalid id",
			yaml: `
cameras:
  - id: "bad camera!"
    main_rtsp: rtsp://127.0.0.1/1
`,
			want: "invalid camera id",
		},
		{
			name: "duplicate id",
			yaml: `
cameras:
  - id: cam1
    main_rtsp: rtsp://127.0.0.1/1
  - id: cam1
    main_rtsp: rtsp://127.0.0.1/2
`,
			want: "duplicate camera id",
		},
		{
			name: "missing main_rtsp",
			yaml: `
cameras:
  - id: cam1
`,
			want: "main_rtsp cannot be empty",
		},
		{
			name: "onvif without url",
			yaml: `
cameras:
  - id: cam1
    main_rtsp: rtsp://127.0.0.1/1
    motion:
      mode: onvif
`,
			want: "requires motion.onvif_url",
		},
		{
			name: "isapi unknown dvr",
			yaml: `
cameras:
  - id: cam1
    main_rtsp: rtsp://127.0.0.1/1
    motion:
      mode: hikvision_isapi
      dvr: missing
      channel: 1
`,
			want: "not found in hikvision_dvrs",
		},
		{
			name: "invalid ignore zone",
			yaml: `
cameras:
  - id: cam1
    main_rtsp: rtsp://127.0.0.1/1
    motion:
      mode: pixel
      ignore_zones:
        - [0.5, 0.5, 0.2, 0.8]
`,
			want: "ignore_zones",
		},
		{
			name: "disabled invalid camera ignored",
			yaml: `
cameras:
  - id: cam1
    enabled: false
`,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestISAPIOrPixelAndSaveStorageCap(t *testing.T) {
	raw := `# Top comment preserved
hikvision_dvrs:
  - id: dvr1
    host: 198.51.100.10
    username: admin
    password: secret

storage:
  dir: recordings
  max_gb: 50 # cap comment

cameras:
  - id: dvr_ch1
    name: Cam 1
    main_rtsp: rtsp://admin:secret@198.51.100.10:554/Streaming/Channels/101
    motion:
      mode: isapi_or_pixel
      dvr: dvr1
      channel: 1
`
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse isapi_or_pixel failed: %v", err)
	}
	if cfg.Cameras[0].Motion.Mode != MotionModeISAPIOrPixel {
		t.Fatalf("unexpected mode: %q", cfg.Cameras[0].Motion.Mode)
	}

	tmpPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := SaveStorageCap(tmpPath, 75.5); err != nil {
		t.Fatalf("SaveStorageCap failed: %v", err)
	}

	updatedBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	if !strings.Contains(string(updatedBytes), "# Top comment preserved") {
		t.Fatalf("expected top comment to be preserved, got:\n%s", string(updatedBytes))
	}

	reloaded, err := Load(tmpPath)
	if err != nil {
		t.Fatalf("Load updated config: %v", err)
	}
	if reloaded.Storage.MaxGB != 75.5 {
		t.Fatalf("expected reloaded max_gb=75.5, got %v", reloaded.Storage.MaxGB)
	}
	if len(reloaded.Cameras) != 1 || reloaded.Cameras[0].ID != "dvr_ch1" {
		t.Fatalf("unexpected reloaded cameras: %+v", reloaded.Cameras)
	}
}

func TestPTZConfigValidation(t *testing.T) {
	validYAML := `
cameras:
  - id: ptz1
    main_rtsp: rtsp://127.0.0.1/1
    motion:
      mode: pixel
    ptz:
      enabled: true
      type: hikvision_isapi
      url: http://192.0.2.20:80
      username: admin
      password: pw
`
	cfg, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("expected valid PTZ config, got: %v", err)
	}
	if cfg.Cameras[0].PTZ.Channel != 1 {
		t.Fatalf("expected default PTZ channel 1, got %d", cfg.Cameras[0].PTZ.Channel)
	}

	badType := `
cameras:
  - id: ptz1
    main_rtsp: rtsp://127.0.0.1/1
    motion:
      mode: pixel
    ptz:
      enabled: true
      type: pelco
      url: http://192.0.2.20:80
`
	if _, err := Parse([]byte(badType)); err == nil || !strings.Contains(err.Error(), "invalid ptz.type") {
		t.Fatalf("expected invalid ptz.type error, got %v", err)
	}

	missingURL := `
cameras:
  - id: ptz1
    main_rtsp: rtsp://127.0.0.1/1
    motion:
      mode: pixel
    ptz:
      enabled: true
      type: onvif
`
	if _, err := Parse([]byte(missingURL)); err == nil || !strings.Contains(err.Error(), "ptz.url cannot be empty") {
		t.Fatalf("expected missing ptz.url error, got %v", err)
	}
}

func TestLiveStreamAspectRatioAndSaveCameras(t *testing.T) {
	raw := `# Top comment stays
storage:
  dir: recordings
  max_gb: 50

cameras:
  - id: cam1
    name: Cam 1
    main_rtsp: rtsp://127.0.0.1/main
    sub_rtsp: rtsp://127.0.0.1/sub
    live_stream: MAIN
    aspect_ratio: Fill
    resolution: 1920x1080
    motion:
      mode: pixel
`
	cfg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse valid live_stream/aspect_ratio: %v", err)
	}
	if cfg.Cameras[0].LiveStream != "main" || cfg.Cameras[0].EffectiveLiveStream() != "main" {
		t.Fatalf("expected effective live_stream main, got %q / %q", cfg.Cameras[0].LiveStream, cfg.Cameras[0].EffectiveLiveStream())
	}
	if cfg.Cameras[0].AspectRatio != "fill" {
		t.Fatalf("expected aspect_ratio fill, got %q", cfg.Cameras[0].AspectRatio)
	}

	camNoSub := CameraConfig{ID: "c2", MainRTSP: "rtsp://127.0.0.1/m", LiveStream: "sub"}
	if camNoSub.EffectiveLiveStream() != "main" {
		t.Fatalf("expected fallback to main when sub_rtsp empty, got %q", camNoSub.EffectiveLiveStream())
	}

	badLive := `
cameras:
  - id: cam1
    main_rtsp: rtsp://127.0.0.1/main
    live_stream: ultra
`
	if _, err := Parse([]byte(badLive)); err == nil || !strings.Contains(err.Error(), "invalid live_stream") {
		t.Fatalf("expected invalid live_stream error, got %v", err)
	}

	badAspect := `
cameras:
  - id: cam1
    main_rtsp: rtsp://127.0.0.1/main
    aspect_ratio: 21:9
`
	if _, err := Parse([]byte(badAspect)); err == nil || !strings.Contains(err.Error(), "invalid aspect_ratio") {
		t.Fatalf("expected invalid aspect_ratio error, got %v", err)
	}

	tmpPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(tmpPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	newCams := []CameraConfig{
		cfg.Cameras[0],
		{
			ID:         "cam2",
			Name:       "Cam 2",
			MainRTSP:   "rtsp://127.0.0.1/main2",
			SubRTSP:    "rtsp://127.0.0.1/sub2",
			LiveStream: "sub",
			Motion:     MotionConfig{Mode: MotionModePixel},
		},
	}
	if err := SaveCameras(tmpPath, newCams); err != nil {
		t.Fatalf("SaveCameras failed: %v", err)
	}

	savedBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(savedBytes), "# Top comment stays") {
		t.Fatalf("expected top comment preserved:\n%s", string(savedBytes))
	}

	reloaded, err := Load(tmpPath)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if reloaded.Storage.MaxGB != 50 || len(reloaded.Cameras) != 2 || reloaded.Cameras[1].ID != "cam2" {
		t.Fatalf("unexpected reloaded config: storage=%+v cameras=%+v", reloaded.Storage, reloaded.Cameras)
	}
}

func TestEnsureDefaultConfigAndPlatformDefaults(t *testing.T) {
	wantGo2RTC := filepath.FromSlash("bin/go2rtc")
	if runtime.GOOS == "windows" {
		wantGo2RTC = filepath.FromSlash("bin/go2rtc.exe")
	}
	if got := DefaultGo2RTCBin(); got != wantGo2RTC {
		t.Fatalf("DefaultGo2RTCBin()=%q, want %q", got, wantGo2RTC)
	}

	cfgPath := filepath.Join(t.TempDir(), "nested", "config.yaml")
	created, err := EnsureDefaultConfig(cfgPath)
	if err != nil {
		t.Fatalf("EnsureDefaultConfig failed: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true on first call")
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load starter config failed: %v", err)
	}
	if len(cfg.Cameras) != 0 || cfg.Server.Listen != ":8080" || filepath.FromSlash(cfg.Go2RTC.Bin) != wantGo2RTC {
		t.Fatalf("unexpected loaded starter config: %+v", cfg)
	}

	if err := os.WriteFile(cfgPath, []byte("server:\n  listen: \":9999\"\n"), 0o600); err != nil {
		t.Fatalf("overwrite sentinel: %v", err)
	}
	createdAgain, err := EnsureDefaultConfig(cfgPath)
	if err != nil {
		t.Fatalf("EnsureDefaultConfig second call failed: %v", err)
	}
	if createdAgain {
		t.Fatalf("expected created=false when config already exists")
	}
	reloaded, err := Load(cfgPath)
	if err != nil || reloaded.Server.Listen != ":9999" {
		t.Fatalf("expected existing file preserved, got %+v err=%v", reloaded, err)
	}

	if err := SaveCameras(cfgPath, []CameraConfig{{
		ID:       "starter_cam",
		Name:     "Starter Cam",
		MainRTSP: "rtsp://127.0.0.1/main",
	}}); err != nil {
		t.Fatalf("SaveCameras on starter config failed: %v", err)
	}
	withCam, err := Load(cfgPath)
	if err != nil || len(withCam.Cameras) != 1 || withCam.Cameras[0].ID != "starter_cam" {
		t.Fatalf("unexpected cameras after SaveCameras: %+v err=%v", withCam, err)
	}
}
