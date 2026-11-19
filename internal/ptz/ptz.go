package ptz

import (
	"context"
	"fmt"
	"sync"

	"feathernvr/internal/config"
)

// Preset describes a saved PTZ preset position on a camera.
type Preset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Controller abstracts PTZ movement and preset operations for a camera.
type Controller interface {
	Move(ctx context.Context, pan, tilt, zoom float64) error
	Stop(ctx context.Context) error
	ListPresets(ctx context.Context) ([]Preset, error)
	GotoPreset(ctx context.Context, presetID string) error
}

// NewController constructs a PTZ Controller for the given camera PTZ config.
func NewController(cfg config.PTZConfig) (Controller, error) {
	switch cfg.Type {
	case "hikvision_isapi":
		return NewHikvisionController(cfg)
	case "onvif":
		return NewONVIFController(cfg)
	default:
		return nil, fmt.Errorf("unsupported ptz type %q", cfg.Type)
	}
}

func clampUnit(v float64) float64 {
	if v < -1.0 {
		return -1.0
	}
	if v > 1.0 {
		return 1.0
	}
	return v
}

// Manager stores active PTZ controllers indexed by camera ID.
type Manager struct {
	mu          sync.RWMutex
	controllers map[string]Controller
}

// NewManager creates an empty PTZ Manager.
func NewManager() *Manager {
	return &Manager{
		controllers: make(map[string]Controller),
	}
}

// Register stores a PTZ controller for camID.
func (m *Manager) Register(camID string, ctrl Controller) {
	m.mu.Lock()
	m.controllers[camID] = ctrl
	m.mu.Unlock()
}

// Get retrieves the PTZ controller for camID.
func (m *Manager) Get(camID string) (Controller, bool) {
	m.mu.RLock()
	ctrl, ok := m.controllers[camID]
	m.mu.RUnlock()
	return ctrl, ok
}

// Has reports whether camID has a registered PTZ controller.
func (m *Manager) Has(camID string) bool {
	_, ok := m.Get(camID)
	return ok
}
