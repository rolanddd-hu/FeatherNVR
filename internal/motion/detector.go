package motion

import (
	"context"
	"sync"
	"time"
)

// Event represents a motion state update from a detector.
type Event struct {
	CameraID string
	Active   bool
	Source   string
	Score    float64
	Time     time.Time
}

// Stats holds live diagnostic state for a camera's motion detector.
type Stats struct {
	Mode      string    `json:"motion_mode"`
	Active    bool      `json:"motion_active"`
	Connected bool      `json:"motion_connected"`
	LastEvent time.Time `json:"last_motion,omitempty"`
	LastScore float64   `json:"motion_score"`
	LastError string    `json:"motion_error,omitempty"`
}

// Detector monitors one camera (or hub channel) and emits motion events.
type Detector interface {
	Run(ctx context.Context, emit func(Event))
	Stats() Stats
}

// StatsTracker provides thread-safe Stats storage for detector implementations.
type StatsTracker struct {
	mu    sync.RWMutex
	stats Stats
}

// NewStatsTracker initializes a StatsTracker with the given mode name.
func NewStatsTracker(mode string) *StatsTracker {
	return &StatsTracker{stats: Stats{Mode: mode}}
}

// SetMode updates the active detector mode label.
func (s *StatsTracker) SetMode(mode string) {
	s.mu.Lock()
	s.stats.Mode = mode
	s.mu.Unlock()
}

// SetConnected marks the detector source connected/disconnected and records any error.
func (s *StatsTracker) SetConnected(connected bool, errMsg string) {
	s.mu.Lock()
	s.stats.Connected = connected
	if connected {
		s.stats.LastError = ""
	} else if errMsg != "" {
		s.stats.LastError = errMsg
	}
	s.mu.Unlock()
}

// SetError records the latest error without necessarily dropping connection state.
func (s *StatsTracker) SetError(errMsg string) {
	s.mu.Lock()
	s.stats.LastError = errMsg
	s.mu.Unlock()
}

// SetScore updates the latest motion score even when motion is not triggered.
func (s *StatsTracker) SetScore(score float64) {
	s.mu.Lock()
	s.stats.LastScore = score
	s.mu.Unlock()
}

// RecordEvent updates active state, score, and timestamp from an emitted event.
func (s *StatsTracker) RecordEvent(ev Event) {
	s.mu.Lock()
	s.stats.Active = ev.Active
	s.stats.LastScore = ev.Score
	if ev.Active {
		s.stats.LastEvent = ev.Time
	}
	s.mu.Unlock()
}

// Snapshot returns a value copy of the current Stats.
func (s *StatsTracker) Snapshot() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// NoneDetector is used when motion detection is disabled for a camera.
type NoneDetector struct {
	tracker *StatsTracker
}

// NewNoneDetector creates a no-op detector.
func NewNoneDetector() *NoneDetector {
	return &NoneDetector{tracker: NewStatsTracker("none")}
}

// Run blocks until ctx is canceled.
func (d *NoneDetector) Run(ctx context.Context, _ func(Event)) {
	<-ctx.Done()
}

// Stats returns the static disabled detector stats.
func (d *NoneDetector) Stats() Stats {
	return d.tracker.Snapshot()
}
