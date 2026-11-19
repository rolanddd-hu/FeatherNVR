package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Recording represents a single recorded motion clip stored in SQLite.
type Recording struct {
	ID            int64   `json:"id"`
	CameraID      string  `json:"camera_id"`
	CameraName    string  `json:"camera_name"`
	StartTime     int64   `json:"start_time"`
	EndTime       int64   `json:"end_time"`
	DurationSec   float64 `json:"duration_sec"`
	FilePath      string  `json:"file_path"`
	ThumbPath     string  `json:"thumb_path"`
	SizeBytes     int64   `json:"size_bytes"`
	TriggerSource string  `json:"trigger_source"`
}

// RecordingDay summarizes all motion clips recorded for one camera on a local calendar day.
type RecordingDay struct {
	CameraID         string  `json:"camera_id"`
	CameraName       string  `json:"camera_name"`
	Date             string  `json:"date"`
	ClipCount        int     `json:"clip_count"`
	TotalDurationSec float64 `json:"total_duration_sec"`
	TotalSizeBytes   int64   `json:"total_size_bytes"`
	FirstStartTime   int64   `json:"first_start_time"`
	LastEndTime      int64   `json:"last_end_time"`
	ThumbPath        string  `json:"thumb_path"`
}

// Stats summarizes total stored clip usage in SQLite.
type Stats struct {
	Count      int64 `json:"count"`
	TotalBytes int64 `json:"total_bytes"`
	OldestUnix int64 `json:"oldest_unix"`
	NewestUnix int64 `json:"newest_unix"`
}

// DB wraps a SQLite connection in WAL mode for recording metadata.
type DB struct {
	sql *sql.DB
}

// Open opens (or creates) the SQLite database at dbPath and initializes its schema.
func Open(dbPath string) (*DB, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
	}

	sqldb, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}
	// Single serialized writer connection prevents SQLITE_BUSY under multi-camera load.
	sqldb.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
	}
	for _, p := range pragmas {
		if _, err := sqldb.Exec(p); err != nil {
			_ = sqldb.Close()
			return nil, fmt.Errorf("sqlite %s: %w", p, err)
		}
	}

	const schema = `
CREATE TABLE IF NOT EXISTS recordings (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	camera_id TEXT NOT NULL,
	camera_name TEXT NOT NULL,
	start_time INTEGER NOT NULL,
	end_time INTEGER NOT NULL,
	duration_sec REAL NOT NULL,
	file_path TEXT NOT NULL UNIQUE,
	thumb_path TEXT NOT NULL DEFAULT '',
	size_bytes INTEGER NOT NULL,
	trigger_source TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_recordings_start ON recordings(start_time ASC);
CREATE INDEX IF NOT EXISTS idx_recordings_cam_start ON recordings(camera_id, start_time DESC);
CREATE TABLE IF NOT EXISTS kv_settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`
	if _, err := sqldb.Exec(schema); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("init sqlite schema: %w", err)
	}

	return &DB{sql: sqldb}, nil
}

// Close closes the underlying SQLite connection.
func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

// InsertRecording inserts a recording row and returns its assigned ID.
func (d *DB) InsertRecording(ctx context.Context, rec *Recording) (int64, error) {
	res, err := d.sql.ExecContext(ctx, `
		INSERT INTO recordings (
			camera_id, camera_name, start_time, end_time, duration_sec,
			file_path, thumb_path, size_bytes, trigger_source
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.CameraID,
		rec.CameraName,
		rec.StartTime,
		rec.EndTime,
		rec.DurationSec,
		rec.FilePath,
		rec.ThumbPath,
		rec.SizeBytes,
		rec.TriggerSource,
	)
	if err != nil {
		return 0, fmt.Errorf("insert recording: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	rec.ID = id
	return id, nil
}

// UpdateThumbPath sets thumb_path for an existing recording row.
func (d *DB) UpdateThumbPath(ctx context.Context, id int64, thumbPath string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE recordings SET thumb_path = ? WHERE id = ?`, thumbPath, id)
	return err
}

// UpdateThumbPathByFilePath sets thumb_path for a recording matched by its unique relative file_path.
func (d *DB) UpdateThumbPathByFilePath(ctx context.Context, filePath, thumbPath string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE recordings SET thumb_path = ? WHERE file_path = ?`, thumbPath, filePath)
	return err
}

// ListRecordings returns recordings newest-first filtered by optional cameraID and date (YYYY-MM-DD).
func (d *DB) ListRecordings(ctx context.Context, cameraID string, date string, limit, offset int) ([]Recording, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	var conds []string
	var args []any

	if strings.TrimSpace(cameraID) != "" {
		conds = append(conds, "camera_id = ?")
		args = append(args, strings.TrimSpace(cameraID))
	}
	if strings.TrimSpace(date) != "" {
		dayStart, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(date), time.Local)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid date %q (expected YYYY-MM-DD): %w", date, err)
		}
		dayEnd := dayStart.Add(24 * time.Hour)
		conds = append(conds, "start_time >= ? AND start_time < ?")
		args = append(args, dayStart.Unix(), dayEnd.Unix())
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	var total int64
	if err := d.sql.QueryRowContext(ctx, "SELECT COUNT(*) FROM recordings "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count recordings: %w", err)
	}

	queryArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, camera_id, camera_name, start_time, end_time, duration_sec,
		       file_path, thumb_path, size_bytes, trigger_source
		FROM recordings `+where+`
		ORDER BY start_time DESC, id DESC
		LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list recordings: %w", err)
	}
	defer rows.Close()

	var out []Recording
	for rows.Next() {
		var r Recording
		if err := rows.Scan(
			&r.ID, &r.CameraID, &r.CameraName, &r.StartTime, &r.EndTime, &r.DurationSec,
			&r.FilePath, &r.ThumbPath, &r.SizeBytes, &r.TriggerSource,
		); err != nil {
			return nil, 0, fmt.Errorf("scan recording: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// GetRecording retrieves one recording by ID.
func (d *DB) GetRecording(ctx context.Context, id int64) (*Recording, error) {
	var r Recording
	err := d.sql.QueryRowContext(ctx, `
		SELECT id, camera_id, camera_name, start_time, end_time, duration_sec,
		       file_path, thumb_path, size_bytes, trigger_source
		FROM recordings WHERE id = ?`, id).Scan(
		&r.ID, &r.CameraID, &r.CameraName, &r.StartTime, &r.EndTime, &r.DurationSec,
		&r.FilePath, &r.ThumbPath, &r.SizeBytes, &r.TriggerSource,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("get recording %d: %w", id, err)
	}
	return &r, nil
}

// DeleteRecording deletes a recording row by ID.
func (d *DB) DeleteRecording(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, id)
	return err
}

// OldestRecordings returns up to limit oldest recordings ordered by start_time ASC.
func (d *DB) OldestRecordings(ctx context.Context, limit int) ([]Recording, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, camera_id, camera_name, start_time, end_time, duration_sec,
		       file_path, thumb_path, size_bytes, trigger_source
		FROM recordings
		ORDER BY start_time ASC, id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query oldest recordings: %w", err)
	}
	defer rows.Close()

	var out []Recording
	for rows.Next() {
		var r Recording
		if err := rows.Scan(
			&r.ID, &r.CameraID, &r.CameraName, &r.StartTime, &r.EndTime, &r.DurationSec,
			&r.FilePath, &r.ThumbPath, &r.SizeBytes, &r.TriggerSource,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TotalStats returns aggregate count, bytes, oldest, and newest timestamps.
func (d *DB) TotalStats(ctx context.Context) (Stats, error) {
	var s Stats
	err := d.sql.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size_bytes), 0), COALESCE(MIN(start_time), 0), COALESCE(MAX(start_time), 0)
		FROM recordings`).Scan(&s.Count, &s.TotalBytes, &s.OldestUnix, &s.NewestUnix)
	if err != nil {
		return Stats{}, fmt.Errorf("total stats: %w", err)
	}
	return s, nil
}

// AllFilePaths returns a map of relative file_path -> recording ID.
func (d *DB) AllFilePaths(ctx context.Context) (map[string]int64, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, file_path FROM recordings`)
	if err != nil {
		return nil, fmt.Errorf("query all file paths: %w", err)
	}
	defer rows.Close()

	m := make(map[string]int64)
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		m[p] = id
	}
	return m, rows.Err()
}

// GetSetting retrieves a string setting from kv_settings, returning "" when not set.
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var val string
	err := d.sql.QueryRowContext(ctx, `SELECT value FROM kv_settings WHERE key = ?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get setting %q: %w", key, err)
	}
	return val, nil
}

// SetSetting stores or overwrites a string setting in kv_settings.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO kv_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}

// ListRecordingDays aggregates recordings into one summary per (camera_id, local date YYYY-MM-DD).
func (d *DB) ListRecordingDays(ctx context.Context, cameraID, date string, limit, offset int) ([]RecordingDay, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	var conds []string
	var args []any

	if cam := strings.TrimSpace(cameraID); cam != "" {
		conds = append(conds, "camera_id = ?")
		args = append(args, cam)
	}
	if dStr := strings.TrimSpace(date); dStr != "" {
		dayStart, err := time.ParseInLocation("2006-01-02", dStr, time.Local)
		if err != nil {
			return nil, 0, fmt.Errorf("invalid date %q (expected YYYY-MM-DD): %w", date, err)
		}
		dayEnd := dayStart.AddDate(0, 0, 1)
		conds = append(conds, "start_time >= ? AND start_time < ?")
		args = append(args, dayStart.Unix(), dayEnd.Unix())
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	rows, err := d.sql.QueryContext(ctx, `
		SELECT camera_id, camera_name, start_time, end_time, duration_sec, thumb_path, size_bytes
		FROM recordings `+where+`
		ORDER BY start_time DESC, id DESC`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list recording days: %w", err)
	}
	defer rows.Close()

	type dayKey struct {
		camID string
		date  string
	}
	groups := make(map[dayKey]*RecordingDay)
	var list []*RecordingDay

	for rows.Next() {
		var (
			camID, camName, thumb string
			startTime, endTime    int64
			durSec                float64
			sizeBytes             int64
		)
		if err := rows.Scan(&camID, &camName, &startTime, &endTime, &durSec, &thumb, &sizeBytes); err != nil {
			return nil, 0, fmt.Errorf("scan recording day row: %w", err)
		}
		localDate := time.Unix(startTime, 0).In(time.Local).Format("2006-01-02")
		key := dayKey{camID: camID, date: localDate}

		end := endTime
		if end < startTime {
			end = startTime
		}

		g, ok := groups[key]
		if !ok {
			g = &RecordingDay{
				CameraID:       camID,
				CameraName:     camName,
				Date:           localDate,
				FirstStartTime: startTime,
				LastEndTime:    end,
				ThumbPath:      thumb,
			}
			groups[key] = g
			list = append(list, g)
		} else {
			if startTime < g.FirstStartTime {
				g.FirstStartTime = startTime
			}
			if end > g.LastEndTime {
				g.LastEndTime = end
			}
			// Rows are ordered newest-first, so fill missing name/thumb from the next newest clip.
			if g.CameraName == "" && camName != "" {
				g.CameraName = camName
			}
			if g.ThumbPath == "" && thumb != "" {
				g.ThumbPath = thumb
			}
		}
		g.ClipCount++
		g.TotalDurationSec += durSec
		g.TotalSizeBytes += sizeBytes
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Date != b.Date {
			return a.Date > b.Date
		}
		if a.LastEndTime != b.LastEndTime {
			return a.LastEndTime > b.LastEndTime
		}
		return a.CameraID < b.CameraID
	})

	total := int64(len(list))
	if offset >= len(list) {
		return []RecordingDay{}, total, nil
	}
	endIdx := offset + limit
	if endIdx > len(list) {
		endIdx = len(list)
	}

	out := make([]RecordingDay, endIdx-offset)
	for i := offset; i < endIdx; i++ {
		out[i-offset] = *list[i]
	}
	return out, total, nil
}

// ListDayRecordings returns all recordings for cameraID on date (YYYY-MM-DD local) in chronological order,
// along with the Unix bounds [dayStart, dayEnd).
func (d *DB) ListDayRecordings(ctx context.Context, cameraID, date string) ([]Recording, int64, int64, error) {
	cam := strings.TrimSpace(cameraID)
	if cam == "" {
		return nil, 0, 0, fmt.Errorf("camera is required")
	}
	dStr := strings.TrimSpace(date)
	dayStartTime, err := time.ParseInLocation("2006-01-02", dStr, time.Local)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("invalid date %q (expected YYYY-MM-DD): %w", date, err)
	}
	dayStart := dayStartTime.Unix()
	dayEnd := dayStartTime.AddDate(0, 0, 1).Unix()

	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, camera_id, camera_name, start_time, end_time, duration_sec,
		       file_path, thumb_path, size_bytes, trigger_source
		FROM recordings
		WHERE camera_id = ? AND start_time >= ? AND start_time < ?
		ORDER BY start_time ASC, id ASC`, cam, dayStart, dayEnd)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("list day recordings: %w", err)
	}
	defer rows.Close()

	clips := []Recording{}
	for rows.Next() {
		var r Recording
		if err := rows.Scan(
			&r.ID, &r.CameraID, &r.CameraName, &r.StartTime, &r.EndTime, &r.DurationSec,
			&r.FilePath, &r.ThumbPath, &r.SizeBytes, &r.TriggerSource,
		); err != nil {
			return nil, 0, 0, fmt.Errorf("scan day recording: %w", err)
		}
		clips = append(clips, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	return clips, dayStart, dayEnd, nil
}
