package recorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"feathernvr/internal/config"
	"feathernvr/internal/fmp4"
	"feathernvr/internal/storage"
)

func makeBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func makeFullBox(typ string, version byte, flags uint32, body []byte) []byte {
	p := make([]byte, 4+len(body))
	p[0] = version
	p[1] = byte(flags >> 16)
	p[2] = byte(flags >> 8)
	p[3] = byte(flags)
	copy(p[4:], body)
	return makeBox(typ, p)
}

func buildSynthInit() ([]byte, []byte) {
	ftyp := makeBox("ftyp", []byte("iso5\x00\x00\x02\x00iso5iso6mp41"))
	tkhdBody := make([]byte, 80)
	binary.BigEndian.PutUint32(tkhdBody[8:12], 1)
	tkhd := makeFullBox("tkhd", 0, 3, tkhdBody)

	mdhdBody := make([]byte, 20)
	binary.BigEndian.PutUint32(mdhdBody[8:12], 90000)
	mdhd := makeFullBox("mdhd", 0, 0, mdhdBody)

	hdlrBody := make([]byte, 12)
	copy(hdlrBody[4:8], "vide")
	hdlr := makeFullBox("hdlr", 0, 0, hdlrBody)

	mdia := makeBox("mdia", append(mdhd, hdlr...))
	trak := makeBox("trak", append(tkhd, mdia...))
	moov := makeBox("moov", trak)
	return ftyp, moov
}

func buildSynthMoof(seq uint32, keyframe bool, dts uint64) []byte {
	mfhdBody := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdBody, seq)
	mfhd := makeFullBox("mfhd", 0, 0, mfhdBody)

	flags := uint32(0x01010000)
	if keyframe {
		flags = 0x02000000
	}
	tfhdBody := make([]byte, 16)
	binary.BigEndian.PutUint32(tfhdBody[0:4], 1)
	binary.BigEndian.PutUint32(tfhdBody[4:8], 6000)
	binary.BigEndian.PutUint32(tfhdBody[8:12], 16)
	binary.BigEndian.PutUint32(tfhdBody[12:16], flags)
	tfhd := makeFullBox("tfhd", 0, 0x020038, tfhdBody)

	tfdtBody := make([]byte, 8)
	binary.BigEndian.PutUint64(tfdtBody, dts)
	tfdt := makeFullBox("tfdt", 1, 0, tfdtBody)

	trunBody := make([]byte, 8)
	binary.BigEndian.PutUint32(trunBody[0:4], 1)
	binary.BigEndian.PutUint32(trunBody[4:8], 64)
	trun := makeFullBox("trun", 0, 0x000001, trunBody)

	traf := makeBox("traf", append(append(tfhd, tfdt...), trun...))
	return makeBox("moof", append(mfhd, traf...))
}

func TestRecorderPreBufferTriggerAndRebase(t *testing.T) {
	root := t.TempDir()
	recDir := filepath.Join(root, "recordings")
	db, err := storage.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	storCfg := config.StorageConfig{
		Dir:              recDir,
		MaxGB:            10,
		HighWatermarkPct: 98,
		LowWatermarkPct:  92,
		SweepIntervalSec: 60,
	}
	retention := storage.NewRetentionManager(db, storCfg, nil)

	ftyp, moov := buildSynthInit()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/frame.jpeg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0xFF, 0xD9})
		case "/api/stream.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			fl, _ := w.(http.Flusher)
			_, _ = w.Write(ftyp)
			_, _ = w.Write(moov)
			if fl != nil {
				fl.Flush()
			}
			// Send pre-roll fragments with non-zero initial DTS (900000+) at 100ms cadence.
			for seq := uint32(1); seq <= 15; seq++ {
				select {
				case <-r.Context().Done():
					return
				default:
				}
				isKey := seq%3 == 1
				moof := buildSynthMoof(seq+50, isKey, 900000+uint64(seq)*6000)
				mdat := makeBox("mdat", []byte("synthetic-frame-data"))
				_, _ = w.Write(moof)
				_, _ = w.Write(mdat)
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}))
	defer srv.Close()

	pre := 2
	post := 1
	maxClip := 30
	cam := config.CameraConfig{
		ID:            "test_cam",
		Name:          "Test Cam",
		MainRTSP:      "rtsp://dummy",
		PreRecordSec:  &pre,
		PostRecordSec: &post,
		MaxClipSec:    &maxClip,
	}
	def := config.RecordingConfig{PreRecordSec: 5, PostRecordSec: 10, MaxClipSec: 300, MaxBufferMB: 8}

	rec := NewCameraRecorder(
		cam, def, storCfg,
		srv.URL+"/api/stream.mp4?src=test_cam_main",
		srv.URL+"/api/frame.jpeg?src=test_cam_main",
		"test_cam_main",
		db, retention, nil, nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec.Run(ctx)
	}()

	// Wait for stream to come online and accumulate pre-roll fragments.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := rec.Status(); st.Online {
			gops, _ := rec.ring.Stats()
			if gops >= 1 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !rec.Status().Online {
		cancel()
		<-done
		t.Fatalf("recorder did not become online")
	}

	// Trigger recording; cooldown is 1s, then stop and verify clip exists.
	rec.Trigger("onvif")
	if !rec.Status().Recording {
		t.Fatalf("expected recording=true immediately after Trigger")
	}

	waitClip := time.Now().Add(4 * time.Second)
	var list []storage.Recording
	for time.Now().Before(waitClip) {
		var total int64
		list, total, err = db.ListRecordings(context.Background(), "test_cam", "", 10, 0)
		if err == nil && total >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	<-done

	if len(list) == 0 {
		t.Fatalf("expected at least 1 saved clip in SQLite")
	}

	clipBytes, err := os.ReadFile(filepath.Join(recDir, filepath.FromSlash(list[0].FilePath)))
	if err != nil {
		t.Fatalf("read saved clip: %v", err)
	}

	// Parse saved clip boxes and confirm ftyp+moov + first moof DTS rebased to 0!
	br := bytes.NewReader(clipBytes)
	bFtyp, err := fmp4.ReadBox(br)
	if err != nil || bFtyp.Type != "ftyp" {
		t.Fatalf("expected ftyp first, got %v %v", bFtyp.Type, err)
	}
	bMoov, err := fmp4.ReadBox(br)
	if err != nil || bMoov.Type != "moov" {
		t.Fatalf("expected moov second, got %v %v", bMoov.Type, err)
	}
	initInfo, err := fmp4.ParseInit(bFtyp.Raw, bMoov.Raw)
	if err != nil {
		t.Fatalf("ParseInit on saved clip: %v", err)
	}
	bMoof1, err := fmp4.ReadBox(br)
	if err != nil || bMoof1.Type != "moof" {
		t.Fatalf("expected first moof, got %v %v", bMoof1.Type, err)
	}
	frag1, err := fmp4.ParseMoof(bMoof1.Raw, initInfo)
	if err != nil {
		t.Fatalf("ParseMoof on saved clip: %v", err)
	}
	if !frag1.IsKeyframe || frag1.DTS != 0 {
		t.Fatalf("expected first fragment in clip to be keyframe with rebased DTS=0, got keyframe=%v dts=%d", frag1.IsKeyframe, frag1.DTS)
	}
}

func TestNeedsAudioRemux(t *testing.T) {
	cases := []struct {
		name       string
		hasAudio   bool
		sampleType string
		want       bool
	}{
		{name: "video_only", hasAudio: false, sampleType: "", want: false},
		{name: "native_aac_mp4a", hasAudio: true, sampleType: "mp4a", want: false},
		{name: "native_aac_uppercase", hasAudio: true, sampleType: "MP4A", want: false},
		{name: "flac_needs_remux", hasAudio: true, sampleType: "fLaC", want: true},
		{name: "opus_needs_remux", hasAudio: true, sampleType: "Opus", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsAudioRemux(tc.hasAudio, tc.sampleType); got != tc.want {
				t.Fatalf("needsAudioRemux(%v, %q) = %v, want %v", tc.hasAudio, tc.sampleType, got, tc.want)
			}
		})
	}
}

func buildSyntheticFLACFixture(t *testing.T, ffmpegBin, movflags string) []byte {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "fixture.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=160x90:r=10:d=2",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=8000:duration=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "5",
		"-c:a", "flac", "-strict", "-2",
		"-movflags", movflags,
		"-f", "mp4", "-y", outPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic fixture failed (%v): %s", err, string(out))
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read synthetic fixture: %v", err)
	}
	return data
}

func runRecorderWithFixture(t *testing.T, ffmpegBin string, streamBytes []byte) (string, *fmp4.InitInfo, storage.Recording) {
	t.Helper()
	root := t.TempDir()
	recDir := filepath.Join(root, "recordings")
	db, err := storage.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	storCfg := config.StorageConfig{
		Dir:              recDir,
		MaxGB:            10,
		HighWatermarkPct: 98,
		LowWatermarkPct:  92,
		SweepIntervalSec: 60,
	}
	retention := storage.NewRetentionManager(db, storCfg, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/frame.jpeg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0xFF, 0xD9})
		case "/api/stream.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			fl, _ := w.(http.Flusher)
			_, _ = w.Write(streamBytes)
			if fl != nil {
				fl.Flush()
			}
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	pre := 2
	post := 1
	maxClip := 30
	cam := config.CameraConfig{
		ID:            "test_audio_cam",
		Name:          "Test Audio Cam",
		MainRTSP:      "rtsp://dummy",
		PreRecordSec:  &pre,
		PostRecordSec: &post,
		MaxClipSec:    &maxClip,
	}
	def := config.RecordingConfig{PreRecordSec: 5, PostRecordSec: 10, MaxClipSec: 300, MaxBufferMB: 8}

	rec := NewCameraRecorder(
		cam, def, storCfg,
		srv.URL+"/api/stream.mp4?src=test_audio_cam_main&mp4=all",
		srv.URL+"/api/frame.jpeg?src=test_audio_cam_main",
		"test_audio_cam_main",
		db, retention, nil, nil,
	)
	rec.SetFFmpeg(ffmpegBin, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec.Run(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := rec.Status(); st.Online {
			if gops, _ := rec.ring.Stats(); gops >= 1 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !rec.Status().Online {
		cancel()
		<-done
		t.Fatalf("recorder did not become online")
	}

	rec.Trigger("manual")
	if !rec.Status().Recording {
		cancel()
		<-done
		t.Fatalf("expected recording=true immediately after Trigger")
	}

	waitClip := time.Now().Add(10 * time.Second)
	var list []storage.Recording
	for time.Now().Before(waitClip) {
		var total int64
		list, total, err = db.ListRecordings(context.Background(), cam.ID, "", 10, 0)
		if err == nil && total >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	<-done

	if len(list) == 0 {
		t.Fatalf("expected at least 1 saved clip in SQLite")
	}

	clipAbs := filepath.Join(recDir, filepath.FromSlash(list[0].FilePath))
	st, err := os.Stat(clipAbs)
	if err != nil {
		t.Fatalf("stat saved clip %s: %v", clipAbs, err)
	}
	if list[0].SizeBytes != st.Size() {
		t.Fatalf("SQLite size_bytes=%d does not match disk size=%d", list[0].SizeBytes, st.Size())
	}
	if _, tmpErr := os.Stat(clipAbs + ".tmp"); !os.IsNotExist(tmpErr) {
		t.Fatalf("expected no lingering .tmp file, stat err=%v", tmpErr)
	}

	clipBytes, err := os.ReadFile(clipAbs)
	if err != nil {
		t.Fatalf("read saved clip: %v", err)
	}
	br := bytes.NewReader(clipBytes)
	var ftypRaw, moovRaw []byte
	for len(ftypRaw) == 0 || len(moovRaw) == 0 {
		box, boxErr := fmp4.ReadBox(br)
		if boxErr != nil {
			if boxErr == io.EOF {
				break
			}
			t.Fatalf("read box from saved clip: %v", boxErr)
		}
		switch box.Type {
		case "ftyp":
			ftypRaw = box.Raw
		case "moov":
			moovRaw = box.Raw
		}
	}
	if len(ftypRaw) == 0 || len(moovRaw) == 0 {
		t.Fatalf("saved clip missing ftyp or moov (ftyp=%d moov=%d)", len(ftypRaw), len(moovRaw))
	}
	initInfo, err := fmp4.ParseInit(ftypRaw, moovRaw)
	if err != nil {
		t.Fatalf("ParseInit on saved clip: %v", err)
	}
	return clipAbs, initInfo, list[0]
}

func TestRecorderRemuxesFLACToAAC(t *testing.T) {
	ffmpegBin := config.DetectFFmpeg()
	if ffmpegBin == "" {
		t.Skip("ffmpeg not found on system")
	}

	fixture := buildSyntheticFLACFixture(t, ffmpegBin, "frag_keyframe+empty_moov+default_base_moof")
	_, initInfo, _ := runRecorderWithFixture(t, ffmpegBin, fixture)

	if initInfo.VideoTrackID == 0 {
		t.Fatalf("expected VideoTrackID != 0")
	}
	if initInfo.AudioTrackID == 0 {
		t.Fatalf("expected AudioTrackID != 0 after FLAC->AAC remux")
	}
	if initInfo.AudioSampleType != "mp4a" {
		t.Fatalf("expected AudioSampleType=%q, got %q", "mp4a", initInfo.AudioSampleType)
	}
	if initInfo.AudioTimescale() != 16000 {
		t.Fatalf("expected AudioTimescale=16000, got %d", initInfo.AudioTimescale())
	}
}

func TestRecorderStripsEmptyFLACAudioTrack(t *testing.T) {
	ffmpegBin := config.DetectFFmpeg()
	if ffmpegBin == "" {
		t.Skip("ffmpeg not found on system")
	}

	fixture := buildSyntheticFLACFixture(t, ffmpegBin, "frag_keyframe+empty_moov+default_base_moof+separate_moof")
	br := bytes.NewReader(fixture)
	var out bytes.Buffer
	var ftypRaw, moovRaw []byte
	var initInfo *fmp4.InitInfo
	var pendingMoof []byte
	keptVideo := 0
	droppedAudio := 0

	for {
		box, err := fmp4.ReadBox(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read fixture box: %v", err)
		}
		switch box.Type {
		case "ftyp":
			ftypRaw = box.Raw
			out.Write(box.Raw)
		case "moov":
			moovRaw = box.Raw
			out.Write(box.Raw)
			initInfo, err = fmp4.ParseInit(ftypRaw, moovRaw)
			if err != nil {
				t.Fatalf("ParseInit fixture: %v", err)
			}
		case "moof":
			pendingMoof = box.Raw
		case "mdat":
			if initInfo == nil || len(pendingMoof) == 0 {
				pendingMoof = nil
				continue
			}
			frag, err := fmp4.ParseMoof(pendingMoof, initInfo)
			if err != nil {
				t.Fatalf("ParseMoof fixture: %v", err)
			}
			if frag.IsAudio && !frag.IsVideo {
				droppedAudio++
				pendingMoof = nil
				continue
			}
			out.Write(pendingMoof)
			out.Write(box.Raw)
			keptVideo++
			pendingMoof = nil
		}
	}
	if keptVideo == 0 || droppedAudio == 0 {
		t.Fatalf("expected separate video and audio fragments in fixture, got keptVideo=%d droppedAudio=%d", keptVideo, droppedAudio)
	}

	_, savedInit, _ := runRecorderWithFixture(t, ffmpegBin, out.Bytes())
	if savedInit.VideoTrackID == 0 {
		t.Fatalf("expected VideoTrackID != 0")
	}
	if savedInit.AudioTrackID != 0 {
		t.Fatalf("expected empty fLaC audio track to be stripped (AudioTrackID=0), got AudioTrackID=%d (%q)",
			savedInit.AudioTrackID, savedInit.AudioSampleType)
	}
}
