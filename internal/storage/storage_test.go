package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"feathernvr/internal/config"
)

func TestDBCRUDAndFiltering(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	day1 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 11, 19, 14, 0, 0, 0, time.Local)

	recs := []Recording{
		{CameraID: "cam1", CameraName: "Front", StartTime: day1.Unix(), EndTime: day1.Unix() + 10, DurationSec: 10, FilePath: "cam1/2026-11-18/a.mp4", ThumbPath: "cam1/2026-11-18/a.jpg", SizeBytes: 1000, TriggerSource: "onvif"},
		{CameraID: "cam1", CameraName: "Front", StartTime: day2.Unix(), EndTime: day2.Unix() + 20, DurationSec: 20, FilePath: "cam1/2026-11-19/b.mp4", ThumbPath: "cam1/2026-11-19/b.jpg", SizeBytes: 2000, TriggerSource: "pixel"},
		{CameraID: "cam2", CameraName: "Yard", StartTime: day2.Unix() + 5, EndTime: day2.Unix() + 15, DurationSec: 10, FilePath: "cam2/2026-11-19/c.mp4", SizeBytes: 1500, TriggerSource: "manual"},
	}

	for i := range recs {
		if _, err := db.InsertRecording(ctx, &recs[i]); err != nil {
			t.Fatalf("InsertRecording %d: %v", i, err)
		}
	}

	list, total, err := db.ListRecordings(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListRecordings: %v", err)
	}
	if total != 3 || len(list) != 3 {
		t.Fatalf("expected 3 recordings, got total=%d len=%d", total, len(list))
	}
	if list[0].CameraID != "cam2" {
		t.Fatalf("expected newest recording first (cam2), got %s", list[0].CameraID)
	}

	cam1Day2, totalCam1Day2, err := db.ListRecordings(ctx, "cam1", "2026-11-19", 10, 0)
	if err != nil {
		t.Fatalf("ListRecordings filtered: %v", err)
	}
	if totalCam1Day2 != 1 || len(cam1Day2) != 1 || cam1Day2[0].FilePath != "cam1/2026-11-19/b.mp4" {
		t.Fatalf("unexpected filtered result: %+v", cam1Day2)
	}

	stats, err := db.TotalStats(ctx)
	if err != nil {
		t.Fatalf("TotalStats: %v", err)
	}
	if stats.Count != 3 || stats.TotalBytes != 4500 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestRetentionFIFOEvictionAndDirPruning(t *testing.T) {
	root := t.TempDir()
	recDir := filepath.Join(root, "recordings")
	db, err := Open(filepath.Join(root, "nvr.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	// Cap ~ 1000 bytes (MaxGB = 1000 / 1024^3), High=90% (900B), Low=60% (600B).
	cfg := config.StorageConfig{
		Dir:              recDir,
		MaxGB:            1000.0 / (1024.0 * 1024.0 * 1024.0),
		HighWatermarkPct: 90,
		LowWatermarkPct:  60,
		SweepIntervalSec: 60,
	}
	rm := NewRetentionManager(db, cfg, nil)
	ctx := context.Background()

	// Create 4 files x 300 bytes = 1200 bytes across two date folders.
	for i := 0; i < 4; i++ {
		dateStr := fmt.Sprintf("2026-11-%02d", 10+i)
		relMP4 := fmt.Sprintf("cam1/%s/clip_%d.mp4", dateStr, i)
		relJPG := fmt.Sprintf("cam1/%s/clip_%d.jpg", dateStr, i)
		absMP4 := filepath.Join(recDir, filepath.FromSlash(relMP4))
		absJPG := filepath.Join(recDir, filepath.FromSlash(relJPG))
		if err := os.MkdirAll(filepath.Dir(absMP4), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(absMP4, make([]byte, 300), 0o644); err != nil {
			t.Fatalf("write mp4: %v", err)
		}
		if err := os.WriteFile(absJPG, make([]byte, 40), 0o644); err != nil {
			t.Fatalf("write jpg: %v", err)
		}
		_, err := db.InsertRecording(ctx, &Recording{
			CameraID:   "cam1",
			CameraName: "cam1",
			StartTime:  int64(1000 + i*100),
			EndTime:    int64(1010 + i*100),
			FilePath:   relMP4,
			ThumbPath:  relJPG,
			SizeBytes:  300,
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	deleted, freed, err := rm.Enforce(ctx)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	// 1200 -> delete 2 oldest (600 freed) -> 600 bytes <= lowBytes (600).
	if deleted != 2 || freed != 600 {
		t.Fatalf("expected deleted=2 freed=600, got deleted=%d freed=%d", deleted, freed)
	}

	// Verify oldest two clips and their empty date directories were removed.
	if _, err := os.Stat(filepath.Join(recDir, "cam1", "2026-11-10")); !os.IsNotExist(err) {
		t.Fatalf("expected empty oldest date dir to be pruned")
	}
	if _, err := os.Stat(filepath.Join(recDir, "cam1", "2026-11-11")); !os.IsNotExist(err) {
		t.Fatalf("expected second oldest date dir to be pruned")
	}
	// Verify newest two remain.
	remaining, total, err := db.ListRecordings(ctx, "", "", 10, 0)
	if err != nil || total != 2 || len(remaining) != 2 {
		t.Fatalf("expected 2 remaining rows, got total=%d err=%v", total, err)
	}
	if remaining[0].FilePath != "cam1/2026-11-13/clip_3.mp4" || remaining[1].FilePath != "cam1/2026-11-12/clip_2.mp4" {
		t.Fatalf("unexpected remaining clips: %+v", remaining)
	}
}

func TestPruneEmptyDateDirsPreservesToday(t *testing.T) {
	root := t.TempDir()
	recDir := filepath.Join(root, "recordings")
	db, err := Open(filepath.Join(root, "nvr.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	today := time.Now().Format("2006-01-02")
	past := "2020-01-01"
	todayDir := filepath.Join(recDir, "cam1", today)
	pastDir := filepath.Join(recDir, "cam1", past)
	if err := os.MkdirAll(todayDir, 0o755); err != nil {
		t.Fatalf("mkdir today: %v", err)
	}
	if err := os.MkdirAll(pastDir, 0o755); err != nil {
		t.Fatalf("mkdir past: %v", err)
	}

	rm := NewRetentionManager(db, config.StorageConfig{Dir: recDir}, nil)
	rm.pruneEmptyDateDirs()

	if _, err := os.Stat(todayDir); err != nil {
		t.Fatalf("expected today's empty date dir %q to be preserved: %v", todayDir, err)
	}
	if _, err := os.Stat(pastDir); !os.IsNotExist(err) {
		t.Fatalf("expected past empty date dir %q to be pruned", pastDir)
	}
}

func TestRetentionSetMaxGBLiveUpdate(t *testing.T) {
	root := t.TempDir()
	recDir := filepath.Join(root, "recordings")
	db, err := Open(filepath.Join(root, "nvr.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	cfg := config.StorageConfig{
		Dir:              recDir,
		MaxGB:            1.0,
		HighWatermarkPct: 90,
		LowWatermarkPct:  50,
		SweepIntervalSec: 60,
	}
	rm := NewRetentionManager(db, cfg, nil)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		rel := fmt.Sprintf("cam1/2026-11-18/clip_%d.mp4", i)
		abs := filepath.Join(recDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, make([]byte, 400), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := db.InsertRecording(ctx, &Recording{
			CameraID:   "cam1",
			CameraName: "cam1",
			StartTime:  int64(1000 + i),
			EndTime:    int64(1010 + i),
			FilePath:   rel,
			SizeBytes:  400,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// Lower live cap to ~900 bytes (1200 bytes currently stored -> must evict).
	newCapGB := 900.0 / (1024.0 * 1024.0 * 1024.0)
	rm.SetMaxGB(newCapGB)
	gotGB, _, _ := rm.Settings()
	if gotGB != newCapGB {
		t.Fatalf("Settings() maxGB=%v, want %v", gotGB, newCapGB)
	}

	deleted, freed, err := rm.Enforce(ctx)
	if err != nil {
		t.Fatalf("Enforce: %v", err)
	}
	if deleted != 2 || freed != 800 {
		t.Fatalf("expected deleted=2 freed=800 after SetMaxGB, got deleted=%d freed=%d", deleted, freed)
	}
}

func TestKVSettings(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	val, err := db.GetSetting(ctx, "camera_order")
	if err != nil {
		t.Fatalf("GetSetting missing: %v", err)
	}
	if val != "" {
		t.Fatalf("expected empty string on missing key, got %q", val)
	}

	if err := db.SetSetting(ctx, "camera_order", `["cam2","cam1"]`); err != nil {
		t.Fatalf("SetSetting insert: %v", err)
	}
	got, err := db.GetSetting(ctx, "camera_order")
	if err != nil || got != `["cam2","cam1"]` {
		t.Fatalf("GetSetting got=%q err=%v", got, err)
	}

	if err := db.SetSetting(ctx, "camera_order", `["cam3"]`); err != nil {
		t.Fatalf("SetSetting overwrite: %v", err)
	}
	got, err = db.GetSetting(ctx, "camera_order")
	if err != nil || got != `["cam3"]` {
		t.Fatalf("GetSetting after overwrite got=%q err=%v", got, err)
	}
}

func TestListRecordingDaysAndDayRecordings(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	day1 := time.Date(2026, 11, 18, 9, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 11, 19, 8, 0, 0, 0, time.Local)

	recs := []Recording{
		// cam1 on day1 (1 clip)
		{CameraID: "cam1", CameraName: "Front", StartTime: day1.Unix(), EndTime: day1.Unix() + 12, DurationSec: 12, FilePath: "cam1/2026-11-18/a.mp4", ThumbPath: "cam1/2026-11-18/a.jpg", SizeBytes: 1000, TriggerSource: "pixel"},
		// cam1 on day2 (2 clips, newer clip lacks thumb so newest non-empty thumb is from clip 1!)
		{CameraID: "cam1", CameraName: "Front", StartTime: day2.Unix(), EndTime: day2.Unix() + 15, DurationSec: 15, FilePath: "cam1/2026-11-19/b1.mp4", ThumbPath: "cam1/2026-11-19/b1.jpg", SizeBytes: 2000, TriggerSource: "pixel"},
		{CameraID: "cam1", CameraName: "Front Renamed", StartTime: day2.Unix() + 600, EndTime: day2.Unix() + 625, DurationSec: 25, FilePath: "cam1/2026-11-19/b2.mp4", ThumbPath: "", SizeBytes: 3000, TriggerSource: "onvif"},
		// cam2 on day2 (1 clip, newer than cam1 day2 lastEnd)
		{CameraID: "cam2", CameraName: "Yard", StartTime: day2.Unix() + 1200, EndTime: day2.Unix() + 1210, DurationSec: 10, FilePath: "cam2/2026-11-19/c.mp4", ThumbPath: "cam2/2026-11-19/c.jpg", SizeBytes: 1500, TriggerSource: "manual"},
	}
	for i := range recs {
		if _, err := db.InsertRecording(ctx, &recs[i]); err != nil {
			t.Fatalf("InsertRecording %d: %v", i, err)
		}
	}

	days, total, err := db.ListRecordingDays(ctx, "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListRecordingDays: %v", err)
	}
	if total != 3 || len(days) != 3 {
		t.Fatalf("expected 3 day groups, got total=%d len=%d (%+v)", total, len(days), days)
	}
	// Sorted by Date DESC, LastEndTime DESC: cam2 day2, cam1 day2, cam1 day1.
	if days[0].CameraID != "cam2" || days[0].Date != "2026-11-19" {
		t.Fatalf("unexpected days[0]: %+v", days[0])
	}
	g := days[1]
	if g.CameraID != "cam1" || g.Date != "2026-11-19" ||
		g.ClipCount != 2 || g.TotalDurationSec != 40 || g.TotalSizeBytes != 5000 ||
		g.FirstStartTime != day2.Unix() || g.LastEndTime != day2.Unix()+625 ||
		g.ThumbPath != "cam1/2026-11-19/b1.jpg" || g.CameraName != "Front Renamed" {
		t.Fatalf("unexpected cam1 day2 aggregate: %+v", g)
	}

	// Pagination & filters
	paged, ptotal, err := db.ListRecordingDays(ctx, "", "2026-11-19", 1, 1)
	if err != nil {
		t.Fatalf("ListRecordingDays filtered/paged: %v", err)
	}
	if ptotal != 2 || len(paged) != 1 || paged[0].CameraID != "cam1" {
		t.Fatalf("unexpected paged result: total=%d len=%d (%+v)", ptotal, len(paged), paged)
	}

	// Chronological ListDayRecordings
	clips, dayStart, dayEnd, err := db.ListDayRecordings(ctx, "cam1", "2026-11-19")
	if err != nil {
		t.Fatalf("ListDayRecordings: %v", err)
	}
	if len(clips) != 2 || clips[0].FilePath != "cam1/2026-11-19/b1.mp4" || clips[1].FilePath != "cam1/2026-11-19/b2.mp4" {
		t.Fatalf("unexpected chronological clips: %+v", clips)
	}
	wantStart := time.Date(2026, 11, 19, 0, 0, 0, 0, time.Local)
	wantEnd := wantStart.AddDate(0, 0, 1)
	if dayStart != wantStart.Unix() || dayEnd != wantEnd.Unix() {
		t.Fatalf("unexpected day bounds [%d,%d), want [%d,%d)", dayStart, dayEnd, wantStart.Unix(), wantEnd.Unix())
	}
}
