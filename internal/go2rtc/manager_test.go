package go2rtc

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
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

func makeTestBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func makeTestFullBox(typ string, version byte, flags uint32, body []byte) []byte {
	p := make([]byte, 4+len(body))
	p[0] = version
	copy(p[4:], body)
	return makeTestBox(typ, p)
}

func buildProbeInitBytes() []byte {
	ftyp := makeTestBox("ftyp", []byte("iso5\x00\x00\x02\x00iso5iso6"))
	tkhdBody := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhdBody[8:12], 1)
	tkhd := makeTestFullBox("tkhd", 0, 3, tkhdBody)
	mdhdBody := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdBody[8:12], 90000)
	mdhd := makeTestFullBox("mdhd", 0, 0, mdhdBody)
	hdlrBody := make([]byte, 12)
	copy(hdlrBody[4:8], "vide")
	hdlr := makeTestFullBox("hdlr", 0, 0, hdlrBody)
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[24:26], 1920)
	binary.BigEndian.PutUint16(entry[26:28], 1080)
	hev1 := makeTestBox("hvc1", entry)
	stsdBody := make([]byte, 4+len(hev1))
	binary.BigEndian.PutUint32(stsdBody[0:4], 1)
	copy(stsdBody[4:], hev1)
	stsd := makeTestFullBox("stsd", 0, 0, stsdBody)
	minf := makeTestBox("minf", makeTestBox("stbl", stsd))
	mdia := makeTestBox("mdia", append(append(mdhd, hdlr...), minf...))
	moov := makeTestBox("moov", makeTestBox("trak", append(tkhd, mdia...)))
	var buf bytes.Buffer
	buf.Write(ftyp)
	buf.Write(moov)
	return buf.Bytes()
}

func TestUpsertDeleteAndTestStream(t *testing.T) {
	var mu sync.Mutex
	var upsertedName, upsertedSrc, deletedSrc string
	initBytes := buildProbeInitBytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/streams" && r.Method == http.MethodPut:
			mu.Lock()
			upsertedName = r.URL.Query().Get("name")
			upsertedSrc = r.URL.Query().Get("src")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/streams" && r.Method == http.MethodDelete:
			mu.Lock()
			deletedSrc = r.URL.Query().Get("src")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/stream.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(initBytes)
		case r.URL.Path == "/api/frame.jpeg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("fakejpegdata"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	mgr := New(&config.Config{
		Go2RTC: config.Go2RTCConfig{APIPort: port},
	}, nil, nil)

	res, err := mgr.TestStream(context.Background(), "rtsp://127.0.0.1:554/stream1")
	if err != nil {
		t.Fatalf("TestStream unexpected err: %v", err)
	}
	if !res.OK || res.Width != 1920 || res.Height != 1080 || res.Codec != "H.265" || res.SnapshotB64 == "" {
		t.Fatalf("unexpected TestStream result: %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	if upsertedName == "" || upsertedSrc != "rtsp://127.0.0.1:554/stream1#backchannel=0" {
		t.Fatalf("unexpected upsert call: name=%q src=%q", upsertedName, upsertedSrc)
	}
	if deletedSrc != upsertedName {
		t.Fatalf("expected temp probe %q to be deleted, deletedSrc=%q", upsertedName, deletedSrc)
	}
	_ = io.Discard
}
