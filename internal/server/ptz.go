package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"feathernvr/internal/ptz"
)

type ptzTimers struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
}

func (p *ptzTimers) cancel(camID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if t, ok := p.timers[camID]; ok {
		t.Stop()
		delete(p.timers, camID)
	}
	p.mu.Unlock()
}

func (p *ptzTimers) schedule(camID string, d time.Duration, fn func()) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.timers == nil {
		p.timers = make(map[string]*time.Timer)
	}
	if old, ok := p.timers[camID]; ok {
		old.Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		p.mu.Lock()
		active := p.timers[camID] == t
		if active {
			delete(p.timers, camID)
		}
		p.mu.Unlock()
		if active {
			fn()
		}
	})
	p.timers[camID] = t
}

// SetPTZManager attaches the PTZ controller manager used by PTZ API endpoints.
func (s *Server) SetPTZManager(mgr *ptz.Manager) {
	s.ptzMgr = mgr
}

func (s *Server) lookupPTZ(camID string) (ptz.Controller, bool) {
	if s.ptzMgr == nil {
		return nil, false
	}
	return s.ptzMgr.Get(camID)
}

func (s *Server) handlePTZMove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctrl, ok := s.lookupPTZ(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "ptz not enabled for camera")
		return
	}

	var req struct {
		Pan        float64 `json:"pan"`
		Tilt       float64 `json:"tilt"`
		Zoom       float64 `json:"zoom"`
		DurationMS int     `json:"duration_ms"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json payload")
		return
	}

	s.ptzTimers.cancel(id)
	if err := ctrl.Move(r.Context(), req.Pan, req.Tilt, req.Zoom); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	if req.DurationMS > 0 {
		ms := req.DurationMS
		if ms < 50 {
			ms = 50
		}
		if ms > 5000 {
			ms = 5000
		}
		s.ptzTimers.schedule(id, time.Duration(ms)*time.Millisecond, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = ctrl.Stop(ctx)
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePTZStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctrl, ok := s.lookupPTZ(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "ptz not enabled for camera")
		return
	}

	s.ptzTimers.cancel(id)
	if err := ctrl.Stop(r.Context()); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePTZPresets(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctrl, ok := s.lookupPTZ(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "ptz not enabled for camera")
		return
	}

	presets, err := ctrl.ListPresets(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	if presets == nil {
		presets = []ptz.Preset{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"presets": presets})
}

func (s *Server) handlePTZGotoPreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctrl, ok := s.lookupPTZ(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "ptz not enabled for camera")
		return
	}

	preset := strings.TrimSpace(r.PathValue("preset"))
	if preset == "" {
		writeJSONError(w, http.StatusBadRequest, "preset id is required")
		return
	}

	s.ptzTimers.cancel(id)
	if err := ctrl.GotoPreset(r.Context(), preset); err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preset": preset})
}
