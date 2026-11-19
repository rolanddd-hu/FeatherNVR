package storage

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"feathernvr/internal/config"
)

// RetentionManager reconciles disk recordings with SQLite and enforces the FIFO cap.
type RetentionManager struct {
	db       *DB
	cfg      config.StorageConfig
	logger   *log.Logger
	notifyCh chan struct{}
	mu       sync.Mutex
}

// NewRetentionManager creates a RetentionManager for the given DB and storage settings.
func NewRetentionManager(db *DB, cfg config.StorageConfig, logger *log.Logger) *RetentionManager {
	if logger == nil {
		logger = log.Default()
	}
	return &RetentionManager{
		db:       db,
		cfg:      cfg,
		logger:   logger,
		notifyCh: make(chan struct{}, 1),
	}
}

// Notify signals the background retention loop to check disk usage soon.
func (r *RetentionManager) Notify() {
	select {
	case r.notifyCh <- struct{}{}:
	default:
	}
}

// Run runs periodic and event-triggered retention enforcement until ctx is canceled.
func (r *RetentionManager) Run(ctx context.Context) {
	interval := time.Duration(r.cfg.SweepIntervalSec) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.notifyCh:
			if _, _, err := r.Enforce(ctx); err != nil && ctx.Err() == nil {
				r.logger.Printf("[storage] retention enforce error: %v", err)
			}
		case <-ticker.C:
			if _, _, err := r.Enforce(ctx); err != nil && ctx.Err() == nil {
				r.logger.Printf("[storage] periodic retention error: %v", err)
			}
		}
	}
}

// Reconcile synchronizes SQLite rows with actual .mp4 files in storage.Dir and cleans stray .tmp files.
func (r *RetentionManager) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(r.cfg.Dir, 0o755); err != nil {
		return err
	}

	dbPaths, err := r.db.AllFilePaths(ctx)
	if err != nil {
		return err
	}

	// Remove DB rows whose files are missing on disk.
	for rel, id := range dbPaths {
		abs := filepath.Join(r.cfg.Dir, filepath.FromSlash(rel))
		if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
			if delErr := r.db.DeleteRecording(ctx, id); delErr != nil {
				r.logger.Printf("[storage] reconcile delete missing row %d (%s): %v", id, rel, delErr)
			} else {
				delete(dbPaths, rel)
			}
		}
	}

	// Walk disk to clean *.tmp files and register any valid orphaned .mp4 clips.
	err = filepath.WalkDir(r.cfg.Dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}

		lower := strings.ToLower(d.Name())
		if strings.HasSuffix(lower, ".tmp") {
			_ = os.Remove(path)
			return nil
		}
		if !strings.HasSuffix(lower, ".mp4") {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() == 0 {
			_ = os.Remove(path)
			return nil
		}

		relOS, err := filepath.Rel(r.cfg.Dir, path)
		if err != nil {
			return nil
		}
		rel := filepath.ToSlash(relOS)
		if _, known := dbPaths[rel]; known {
			return nil
		}

		camID := strings.Split(rel, "/")[0]
		if camID == "" || camID == rel {
			camID = "unknown"
		}
		startTime := parseClipTimestamp(d.Name(), info.ModTime())
		endTime := info.ModTime()
		if endTime.Before(startTime) {
			endTime = startTime
		}

		thumbRel := ""
		thumbCandidate := strings.TrimSuffix(path, filepath.Ext(path)) + ".jpg"
		if st, err := os.Stat(thumbCandidate); err == nil && !st.IsDir() && st.Size() > 0 {
			if tr, err := filepath.Rel(r.cfg.Dir, thumbCandidate); err == nil {
				thumbRel = filepath.ToSlash(tr)
			}
		}

		rec := &Recording{
			CameraID:      camID,
			CameraName:    camID,
			StartTime:     startTime.Unix(),
			EndTime:       endTime.Unix(),
			DurationSec:   endTime.Sub(startTime).Seconds(),
			FilePath:      rel,
			ThumbPath:     thumbRel,
			SizeBytes:     info.Size(),
			TriggerSource: "recovered",
		}
		if _, err := r.db.InsertRecording(ctx, rec); err != nil {
			r.logger.Printf("[storage] reconcile insert orphan %s: %v", rel, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	r.pruneEmptyDateDirs()
	return nil
}

func parseClipTimestamp(filename string, fallback time.Time) time.Time {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	if len(base) >= 15 {
		if ts, err := time.ParseInLocation("20060102_150405", base[:15], time.Local); err == nil {
			return ts
		}
	}
	return fallback
}

func (r *RetentionManager) maxBytesLocked() int64 {
	return int64(r.cfg.MaxGB * 1024 * 1024 * 1024)
}

// MaxBytes returns the configured storage cap in bytes.
func (r *RetentionManager) MaxBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxBytesLocked()
}

// SetMaxGB updates the live storage cap in GB and triggers retention evaluation.
func (r *RetentionManager) SetMaxGB(maxGB float64) {
	if maxGB <= 0 {
		return
	}
	r.mu.Lock()
	r.cfg.MaxGB = maxGB
	r.mu.Unlock()
	r.Notify()
}

// Settings returns the current maxGB, high watermark %, and low watermark %.
func (r *RetentionManager) Settings() (maxGB, highPct, lowPct float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.MaxGB, r.cfg.HighWatermarkPct, r.cfg.LowWatermarkPct
}

// Enforce deletes oldest recordings FIFO whenever stored bytes exceed the high watermark
// (or max cap) until usage falls at or below the low watermark (and max cap).
func (r *RetentionManager) Enforce(ctx context.Context) (deleted int, freedBytes int64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	maxBytes := r.maxBytesLocked()
	if maxBytes <= 0 {
		return 0, 0, nil
	}
	highBytes := int64(float64(maxBytes) * r.cfg.HighWatermarkPct / 100.0)
	lowBytes := int64(float64(maxBytes) * r.cfg.LowWatermarkPct / 100.0)
	if lowBytes > maxBytes {
		lowBytes = maxBytes
	}

	stats, err := r.db.TotalStats(ctx)
	if err != nil {
		return 0, 0, err
	}
	if stats.TotalBytes <= highBytes && stats.TotalBytes <= maxBytes {
		return 0, 0, nil
	}

	skipped := make(map[int64]bool)
	for stats.TotalBytes > lowBytes || stats.TotalBytes > maxBytes {
		if ctx.Err() != nil {
			return deleted, freedBytes, ctx.Err()
		}

		batch, err := r.db.OldestRecordings(ctx, 100)
		if err != nil {
			return deleted, freedBytes, err
		}
		if len(batch) == 0 {
			break
		}

		madeProgress := false
		for _, rec := range batch {
			if stats.TotalBytes <= lowBytes && stats.TotalBytes <= maxBytes {
				break
			}
			if skipped[rec.ID] {
				continue
			}

			mp4Abs := filepath.Join(r.cfg.Dir, filepath.FromSlash(rec.FilePath))
			if rmErr := os.Remove(mp4Abs); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				r.logger.Printf("[storage] cannot remove %s (skipping for now): %v", mp4Abs, rmErr)
				skipped[rec.ID] = true
				continue
			}
			if rec.ThumbPath != "" {
				thumbAbs := filepath.Join(r.cfg.Dir, filepath.FromSlash(rec.ThumbPath))
				_ = os.Remove(thumbAbs)
			}
			if delErr := r.db.DeleteRecording(ctx, rec.ID); delErr != nil {
				r.logger.Printf("[storage] delete row %d error: %v", rec.ID, delErr)
				skipped[rec.ID] = true
				continue
			}

			stats.TotalBytes -= rec.SizeBytes
			if stats.TotalBytes < 0 {
				stats.TotalBytes = 0
			}
			deleted++
			freedBytes += rec.SizeBytes
			madeProgress = true
		}

		if !madeProgress {
			break
		}
	}

	if deleted > 0 {
		r.pruneEmptyDateDirs()
		r.logger.Printf("[storage] FIFO retention removed %d clip(s), freed %.2f MB", deleted, float64(freedBytes)/(1024*1024))
	}
	return deleted, freedBytes, nil
}

// DeleteRecordingByID removes a single recording's files and database row.
func (r *RetentionManager) DeleteRecordingByID(ctx context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec, err := r.db.GetRecording(ctx, id)
	if err != nil {
		return err
	}
	mp4Abs := filepath.Join(r.cfg.Dir, filepath.FromSlash(rec.FilePath))
	if rmErr := os.Remove(mp4Abs); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return rmErr
	}
	if rec.ThumbPath != "" {
		thumbAbs := filepath.Join(r.cfg.Dir, filepath.FromSlash(rec.ThumbPath))
		_ = os.Remove(thumbAbs)
	}
	if err := r.db.DeleteRecording(ctx, id); err != nil {
		return err
	}
	r.pruneEmptyDateDirs()
	return nil
}

// pruneEmptyDateDirs removes empty leaf date directories (<storage.Dir>/<cam>/<date>) without
// deleting the root storage dir, camera directories, or today's active recording folder.
func (r *RetentionManager) pruneEmptyDateDirs() {
	today := time.Now().Format("2006-01-02")
	camEntries, err := os.ReadDir(r.cfg.Dir)
	if err != nil {
		return
	}
	for _, ce := range camEntries {
		if !ce.IsDir() {
			continue
		}
		camDir := filepath.Join(r.cfg.Dir, ce.Name())
		dateEntries, err := os.ReadDir(camDir)
		if err != nil {
			continue
		}
		for _, de := range dateEntries {
			if !de.IsDir() || de.Name() == today {
				continue
			}
			dateDir := filepath.Join(camDir, de.Name())
			items, err := os.ReadDir(dateDir)
			if err == nil && len(items) == 0 {
				_ = os.Remove(dateDir)
			}
		}
	}
}
