package recorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
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
