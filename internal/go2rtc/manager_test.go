package go2rtc

import (
	"testing"

	"gopkg.in/yaml.v3"

	"feathernvr/internal/config"
)

func TestSubMP4URL(t *testing.T) {
	m := New(&config.Config{
		Go2RTC: config.Go2RTCConfig{APIPort: 1984},
	}, nil, nil)

	withSub := config.CameraConfig{
		ID:       "cam1",
		MainRTSP: "rtsp://127.0.0.1/main",
		SubRTSP:  "rtsp://127.0.0.1/sub",
	}
	if got, want := m.SubMP4URL(withSub), "http://127.0.0.1:1984/api/stream.mp4?src=cam1_sub"; got != want {
		t.Fatalf("with sub: got %q, want %q", got, want)
	}

	mainOnly := config.CameraConfig{
		ID:       "cam2",
		MainRTSP: "rtsp://127.0.0.1/main",
	}
	if got, want := m.SubMP4URL(mainOnly), "http://127.0.0.1:1984/api/stream.mp4?src=cam2_main"; got != want {
		t.Fatalf("main only: got %q, want %q", got, want)
	}
}

func TestRenderConfigYAML(t *testing.T) {
	disabled := false
	cfg := &config.Config{
		FFmpegBin: `C:\ffmpeg\ffmpeg.exe`,
		Go2RTC: config.Go2RTCConfig{
			APIPort:    11984,
			RTSPPort:   18554,
			WebRTCPort: 18555,
		},
		Cameras: []config.CameraConfig{
			{
				ID:       "ptz1",
				MainRTSP: "rtsp://user:pass@192.168.1.10/Streaming/Channels/101",
				SubRTSP:  "rtsp://user:pass@192.168.1.10/Streaming/Channels/102",
			},
			{
				ID:       "main_only",
				MainRTSP: "rtsp://user:pass@192.168.1.11/stream1",
			},
			{
				ID:       "hashed_cam",
				MainRTSP: "rtsp://user:pass@192.168.1.13/stream1#video=copy",
			},
			{
				ID:       "virtual_cam",
				MainRTSP: "exec:ffmpeg -f lavfi -i testsrc -f rtsp {output}",
			},
			{
				ID:       "off_cam",
				Enabled:  &disabled,
				MainRTSP: "rtsp://user:pass@192.168.1.12/stream1",
			},
		},
	}
	cfg.ApplyDefaults()

	mgr := New(cfg, nil, nil)
	data, err := mgr.RenderConfigYAML()
	if err != nil {
		t.Fatalf("RenderConfigYAML failed: %v", err)
	}

	var parsed struct {
		API struct {
			Listen string `yaml:"listen"`
			Origin string `yaml:"origin"`
		} `yaml:"api"`
		RTSP    struct{ Listen string } `yaml:"rtsp"`
		WebRTC  struct{ Listen string } `yaml:"webrtc"`
		FFmpeg  struct{ Bin string }    `yaml:"ffmpeg"`
		Streams map[string]string       `yaml:"streams"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal generated yaml: %v", err)
	}

	if parsed.API.Listen != "127.0.0.1:11984" {
		t.Fatalf("unexpected api.listen: %q", parsed.API.Listen)
	}
	if parsed.API.Origin != "*" {
		t.Fatalf("unexpected api.origin: %q", parsed.API.Origin)
	}
	if parsed.RTSP.Listen != "127.0.0.1:18554" {
		t.Fatalf("unexpected rtsp.listen: %q", parsed.RTSP.Listen)
	}
	if parsed.WebRTC.Listen != ":18555" {
		t.Fatalf("unexpected webrtc.listen: %q", parsed.WebRTC.Listen)
	}
	if parsed.FFmpeg.Bin != `C:\ffmpeg\ffmpeg.exe` {
		t.Fatalf("unexpected ffmpeg.bin: %q", parsed.FFmpeg.Bin)
	}
	if len(parsed.Streams) != 5 {
		t.Fatalf("expected 5 streams, got %d: %+v", len(parsed.Streams), parsed.Streams)
	}
	if parsed.Streams["ptz1_main"] != "rtsp://user:pass@192.168.1.10/Streaming/Channels/101#backchannel=0" ||
		parsed.Streams["ptz1_sub"] != "rtsp://user:pass@192.168.1.10/Streaming/Channels/102#backchannel=0" {
		t.Fatalf("expected #backchannel=0 on ptz1 rtsp streams, got: %+v", parsed.Streams)
	}
	if parsed.Streams["main_only_main"] != "rtsp://user:pass@192.168.1.11/stream1#backchannel=0" {
		t.Fatalf("unexpected main_only_main: %q", parsed.Streams["main_only_main"])
	}
	if parsed.Streams["hashed_cam_main"] != "rtsp://user:pass@192.168.1.13/stream1#video=copy" {
		t.Fatalf("existing hash fragment should be preserved, got: %q", parsed.Streams["hashed_cam_main"])
	}
	if parsed.Streams["virtual_cam_main"] != "exec:ffmpeg -f lavfi -i testsrc -f rtsp {output}" {
		t.Fatalf("non-rtsp stream should remain untouched, got: %q", parsed.Streams["virtual_cam_main"])
	}
	if _, ok := parsed.Streams["main_only_sub"]; ok {
		t.Fatalf("did not expect main_only_sub when sub_rtsp is empty")
	}
	if _, ok := parsed.Streams["off_cam_main"]; ok {
		t.Fatalf("disabled camera should not be in streams")
	}

	if got := mgr.SubRTSPURL("ptz1"); got != "rtsp://127.0.0.1:18554/ptz1_sub" {
		t.Fatalf("SubRTSPURL(ptz1) = %q", got)
	}
	if got := mgr.SubRTSPURL("main_only"); got != "rtsp://127.0.0.1:18554/main_only_main" {
		t.Fatalf("SubRTSPURL(main_only) = %q", got)
	}
	if got := mgr.MainMP4URL("ptz1"); got != "http://127.0.0.1:11984/api/stream.mp4?src=ptz1_main" {
		t.Fatalf("MainMP4URL(ptz1) = %q", got)
	}
	if got := mgr.FrameJPEGURL("ptz1_sub"); got != "http://127.0.0.1:11984/api/frame.jpeg?src=ptz1_sub" {
		t.Fatalf("FrameJPEGURL = %q", got)
	}
}
