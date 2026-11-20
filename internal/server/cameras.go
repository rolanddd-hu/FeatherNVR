package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode"

	"feathernvr/internal/config"
	"feathernvr/internal/go2rtc"
	"feathernvr/internal/motion"
)

const redactedPassMask = "********"

// CameraController provides runtime camera CRUD, stream testing, and ONVIF probing.
type CameraController interface {
	ListCameras() []config.CameraConfig
	GetCamera(id string) (config.CameraConfig, bool)
	ListDVRs() []config.HikvisionDVRConfig
	AddCamera(ctx context.Context, cam config.CameraConfig) (config.CameraConfig, error)
	UpdateCamera(ctx context.Context, cam config.CameraConfig) (config.CameraConfig, error)
	DeleteCamera(ctx context.Context, id string) error
	TestStream(ctx context.Context, rawURL string) (*go2rtc.StreamTestResult, error)
	ProbeCamera(ctx context.Context, req motion.ProbeRequest) (*motion.ProbeResult, error)
}

// SetCameraController attaches the live camera management controller.
func (s *Server) SetCameraController(ctrl CameraController) {
	s.camCtrl = ctrl
}

type motionDTO struct {
	Mode           string       `json:"mode"`
	ONVIFURL       string       `json:"onvif_url,omitempty"`
	Username       string       `json:"username,omitempty"`
	HasPassword    bool         `json:"has_password,omitempty"`
	DVR            string       `json:"dvr,omitempty"`
	Channel        int          `json:"channel,omitempty"`
	FPS            int          `json:"fps"`
	Width          int          `json:"width"`
	Height         int          `json:"height"`
	Threshold      uint8        `json:"threshold"`
	MinAreaPercent float64      `json:"min_area_percent"`
	MaxAreaPercent float64      `json:"max_area_percent"`
	MinFrames      int          `json:"min_frames"`
	IgnoreZones    [][4]float64 `json:"ignore_zones,omitempty"`
}

type ptzDTO struct {
	Enabled      bool   `json:"enabled"`
	Type         string `json:"type,omitempty"`
	URL          string `json:"url,omitempty"`
	Username     string `json:"username,omitempty"`
	HasPassword  bool   `json:"has_password,omitempty"`
	ProfileToken string `json:"profile_token,omitempty"`
	Channel      int    `json:"channel,omitempty"`
}

type cameraDTO struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Enabled       bool      `json:"enabled"`
	MainRTSP      string    `json:"main_rtsp"`
	SubRTSP       string    `json:"sub_rtsp"`
	Username      string    `json:"username,omitempty"`
	HasMainPass   bool      `json:"has_main_password,omitempty"`
	HasSubPass    bool      `json:"has_sub_password,omitempty"`
	LiveStream    string    `json:"live_stream"`
	Resolution    string    `json:"resolution,omitempty"`
	AspectRatio   string    `json:"aspect_ratio,omitempty"`
	PreRecordSec  *int      `json:"pre_record_sec,omitempty"`
	PostRecordSec *int      `json:"post_record_sec,omitempty"`
	MaxClipSec    *int      `json:"max_clip_sec,omitempty"`
	Motion        motionDTO `json:"motion"`
	PTZ           ptzDTO    `json:"ptz"`
}

type dvrSummaryDTO struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port int    `json:"port"`
}

type cameraPayload struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Enabled       *bool  `json:"enabled"`
	MainRTSP      string `json:"main_rtsp"`
	SubRTSP       string `json:"sub_rtsp"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	LiveStream    string `json:"live_stream"`
	Resolution    string `json:"resolution"`
	AspectRatio   string `json:"aspect_ratio"`
	PreRecordSec  *int   `json:"pre_record_sec"`
	PostRecordSec *int   `json:"post_record_sec"`
	MaxClipSec    *int   `json:"max_clip_sec"`
	Motion        struct {
		Mode           string       `json:"mode"`
		ONVIFURL       string       `json:"onvif_url"`
		Username       string       `json:"username"`
		Password       string       `json:"password"`
		KeepPassword   *bool        `json:"keep_password"`
		DVR            string       `json:"dvr"`
		Channel        int          `json:"channel"`
		FPS            int          `json:"fps"`
		Width          int          `json:"width"`
		Height         int          `json:"height"`
		Threshold      uint8        `json:"threshold"`
		MinAreaPercent float64      `json:"min_area_percent"`
		MaxAreaPercent float64      `json:"max_area_percent"`
		MinFrames      int          `json:"min_frames"`
		IgnoreZones    [][4]float64 `json:"ignore_zones"`
	} `json:"motion"`
	PTZ struct {
		Enabled      bool   `json:"enabled"`
		Type         string `json:"type"`
		URL          string `json:"url"`
		Username     string `json:"username"`
		Password     string `json:"password"`
		KeepPassword *bool  `json:"keep_password"`
		ProfileToken string `json:"profile_token"`
		Channel      int    `json:"channel"`
	} `json:"ptz"`
}

func toCameraDTO(cam config.CameraConfig) cameraDTO {
	mainRed, mainUser, hasMainPass := redactRTSP(cam.MainRTSP)
	subRed, subUser, hasSubPass := redactRTSP(cam.SubRTSP)
	user := mainUser
	if user == "" {
		user = subUser
	}
	if user == "" {
		user = cam.Motion.Username
	}

	return cameraDTO{
		ID:            cam.ID,
		Name:          cam.Name,
		Enabled:       cam.IsEnabled(),
		MainRTSP:      mainRed,
		SubRTSP:       subRed,
		Username:      user,
		HasMainPass:   hasMainPass,
		HasSubPass:    hasSubPass,
		LiveStream:    cam.EffectiveLiveStream(),
		Resolution:    cam.Resolution,
		AspectRatio:   cam.AspectRatio,
		PreRecordSec:  cam.PreRecordSec,
		PostRecordSec: cam.PostRecordSec,
		MaxClipSec:    cam.MaxClipSec,
		Motion: motionDTO{
			Mode:           cam.Motion.Mode,
			ONVIFURL:       cam.Motion.ONVIFURL,
			Username:       cam.Motion.Username,
			HasPassword:    cam.Motion.Password != "",
			DVR:            cam.Motion.DVR,
			Channel:        cam.Motion.Channel,
			FPS:            cam.Motion.FPS,
			Width:          cam.Motion.Width,
			Height:         cam.Motion.Height,
			Threshold:      cam.Motion.Threshold,
			MinAreaPercent: cam.Motion.MinAreaPercent,
			MaxAreaPercent: cam.Motion.MaxAreaPercent,
			MinFrames:      cam.Motion.MinFrames,
			IgnoreZones:    cam.Motion.IgnoreZones,
		},
		PTZ: ptzDTO{
			Enabled:      cam.PTZ.Enabled,
			Type:         cam.PTZ.Type,
			URL:          cam.PTZ.URL,
			Username:     cam.PTZ.Username,
			HasPassword:  cam.PTZ.Password != "",
			ProfileToken: cam.PTZ.ProfileToken,
			Channel:      cam.PTZ.Channel,
		},
	}
}

func redactRTSP(raw string) (display string, user string, hasPass bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw, "", false
	}
	user = u.User.Username()
	_, hasPass = u.User.Password()
	if !hasPass {
		return raw, user, false
	}
	u.User = nil
	base := u.String()
	if idx := strings.Index(base, "://"); idx >= 0 {
		uPart := url.PathEscape(user)
		if uPart == "" {
			uPart = "user"
		}
		return base[:idx+3] + uPart + ":" + redactedPassMask + "@" + base[idx+3:], user, true
	}
	return raw, user, true
}

func extractRTSPCreds(raw string) (user, pass string, hasPass bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return "", "", false
	}
	user = u.User.Username()
	pass, hasPass = u.User.Password()
	return user, pass, hasPass
}

func resolveRTSP(submittedURL, fallbackUser, fallbackPass, existingURL string) string {
	s := strings.TrimSpace(submittedURL)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "rtsp://") && !strings.HasPrefix(lower, "rtsps://") {
		return s
	}

	u, err := url.Parse(s)
	if err != nil {
		return s
	}

	existUser, existPass, existHasPass := extractRTSPCreds(existingURL)
	if u.User != nil {
		urlUser := u.User.Username()
		urlPass, urlHasPass := u.User.Password()
		if urlPass == redactedPassMask {
			targetUser := strings.TrimSpace(fallbackUser)
			if targetUser == "" {
				targetUser = urlUser
			}
			if targetUser == "" {
				targetUser = existUser
			}
			targetPass := fallbackPass
			if targetPass == "" && existHasPass {
				targetPass = existPass
			}
			if targetUser != "" {
				if targetPass != "" {
					u.User = url.UserPassword(targetUser, targetPass)
				} else {
					u.User = url.User(targetUser)
				}
			} else {
				u.User = nil
			}
			return u.String()
		}
		// If URL already had real credentials and user supplied a new shared password alongside redacted/same user, keep explicit URL pass unless redacted.
		_ = urlHasPass
		return u.String()
	}

	// No userinfo in submitted URL: inject shared username/password if available.
	targetUser := strings.TrimSpace(fallbackUser)
	if targetUser == "" && fallbackPass != "" {
		targetUser = existUser
	}
	if targetUser != "" {
		targetPass := fallbackPass
		if targetPass == "" && existHasPass {
			targetPass = existPass
		}
		if targetPass != "" {
			u.User = url.UserPassword(targetUser, targetPass)
		} else {
			u.User = url.User(targetUser)
		}
		return u.String()
	}
	return s
}

func buildCameraFromPayload(p cameraPayload, existing *config.CameraConfig, idOverride string, existingCams []config.CameraConfig) config.CameraConfig {
	id := strings.TrimSpace(idOverride)
	if id == "" {
		id = strings.TrimSpace(p.ID)
	}
	if id == "" {
		id = uniqueCameraSlug(p.Name, existingCams)
	}

	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = id
	}

	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	} else if existing != nil {
		enabled = existing.IsEnabled()
	}

	var existMain, existSub, existMotionPass, existPTZPass string
	var existMotionUser, existPTZUser string
	if existing != nil {
		existMain = existing.MainRTSP
		existSub = existing.SubRTSP
		existMotionUser = existing.Motion.Username
		existMotionPass = existing.Motion.Password
		existPTZUser = existing.PTZ.Username
		existPTZPass = existing.PTZ.Password
	}

	sharedUser := strings.TrimSpace(p.Username)
	sharedPass := p.Password

	mainRTSP := resolveRTSP(p.MainRTSP, sharedUser, sharedPass, existMain)
	subRTSP := resolveRTSP(p.SubRTSP, sharedUser, sharedPass, existSub)

	motionUser := strings.TrimSpace(p.Motion.Username)
	motionPass := p.Motion.Password
	if motionUser == "" && sharedUser != "" && (p.Motion.Mode == config.MotionModeONVIF || p.Motion.Mode == config.MotionModeONVIFOrPixel) {
		motionUser = sharedUser
	}
	if motionPass == "" {
		keep := p.Motion.KeepPassword == nil || *p.Motion.KeepPassword
		if existing != nil && keep && motionUser == existMotionUser {
			motionPass = existMotionPass
		} else if sharedPass != "" && (motionUser == sharedUser || sharedUser == "") {
			motionPass = sharedPass
		}
	}

	ptzUser := strings.TrimSpace(p.PTZ.Username)
	ptzPass := p.PTZ.Password
	if p.PTZ.Enabled && ptzUser == "" && sharedUser != "" {
		ptzUser = sharedUser
	}
	if p.PTZ.Enabled && ptzPass == "" {
		keep := p.PTZ.KeepPassword == nil || *p.PTZ.KeepPassword
		if existing != nil && keep && ptzUser == existPTZUser {
			ptzPass = existPTZPass
		} else if sharedPass != "" && (ptzUser == sharedUser || sharedUser == "") {
			ptzPass = sharedPass
		}
	}

	return config.CameraConfig{
		ID:            id,
		Name:          name,
		Enabled:       &enabled,
		MainRTSP:      mainRTSP,
		SubRTSP:       subRTSP,
		LiveStream:    strings.ToLower(strings.TrimSpace(p.LiveStream)),
		Resolution:    strings.TrimSpace(p.Resolution),
		AspectRatio:   strings.ToLower(strings.TrimSpace(p.AspectRatio)),
		PreRecordSec:  p.PreRecordSec,
		PostRecordSec: p.PostRecordSec,
		MaxClipSec:    p.MaxClipSec,
		Motion: config.MotionConfig{
			Mode:           p.Motion.Mode,
			ONVIFURL:       strings.TrimSpace(p.Motion.ONVIFURL),
			Username:       motionUser,
			Password:       motionPass,
			DVR:            strings.TrimSpace(p.Motion.DVR),
			Channel:        p.Motion.Channel,
			FPS:            p.Motion.FPS,
			Width:          p.Motion.Width,
			Height:         p.Motion.Height,
			Threshold:      p.Motion.Threshold,
			MinAreaPercent: p.Motion.MinAreaPercent,
			MaxAreaPercent: p.Motion.MaxAreaPercent,
			MinFrames:      p.Motion.MinFrames,
			IgnoreZones:    p.Motion.IgnoreZones,
		},
		PTZ: config.PTZConfig{
			Enabled:      p.PTZ.Enabled,
			Type:         strings.TrimSpace(p.PTZ.Type),
			URL:          strings.TrimSpace(p.PTZ.URL),
			Username:     ptzUser,
			Password:     ptzPass,
			ProfileToken: strings.TrimSpace(p.PTZ.ProfileToken),
			Channel:      p.PTZ.Channel,
		},
	}
}

func uniqueCameraSlug(name string, existing []config.CameraConfig) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
		} else if !prevUnderscore && b.Len() > 0 && (r == ' ' || r == '_' || r == '-' || unicode.IsPunct(r)) {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	base := strings.Trim(b.String(), "_")
	if base == "" {
		base = "camera"
	}

	used := make(map[string]struct{}, len(existing))
	for _, c := range existing {
		used[c.ID] = struct{}{}
	}
	if _, ok := used[base]; !ok {
		return base
	}
	for n := 2; n < 10000; n++ {
		cand := fmt.Sprintf("%s_%d", base, n)
		if _, ok := used[cand]; !ok {
			return cand
		}
	}
	return base
}

func (s *Server) handleListCameras(w http.ResponseWriter, _ *http.Request) {
	var cams []config.CameraConfig
	var dvrs []config.HikvisionDVRConfig
	if s.camCtrl != nil {
		cams = s.camCtrl.ListCameras()
		dvrs = s.camCtrl.ListDVRs()
	} else if s.cfg != nil {
		cams = s.cfg.Cameras
		dvrs = s.cfg.HikvisionDVRs
	}

	camDTOs := make([]cameraDTO, 0, len(cams))
	for _, c := range cams {
		camDTOs = append(camDTOs, toCameraDTO(c))
	}
	dvrDTOs := make([]dvrSummaryDTO, 0, len(dvrs))
	for _, d := range dvrs {
		dvrDTOs = append(dvrDTOs, dvrSummaryDTO{ID: d.ID, Host: d.Host, Port: d.Port})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"cameras": camDTOs,
		"dvrs":    dvrDTOs,
	})
}

func (s *Server) handleGetCamera(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var (
		cam config.CameraConfig
		ok  bool
	)
	if s.camCtrl != nil {
		cam, ok = s.camCtrl.GetCamera(id)
	} else if s.cfg != nil {
		for _, c := range s.cfg.Cameras {
			if c.ID == id {
				cam = c
				ok = true
				break
			}
		}
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "camera not found")
		return
	}
	writeJSON(w, http.StatusOK, toCameraDTO(cam))
}

func (s *Server) handleCreateCamera(w http.ResponseWriter, r *http.Request) {
	if s.camCtrl == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "camera controller unavailable")
		return
	}
	var payload cameraPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	cam := buildCameraFromPayload(payload, nil, "", s.camCtrl.ListCameras())
	saved, err := s.camCtrl.AddCamera(r.Context(), cam)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toCameraDTO(saved))
}

func (s *Server) handleUpdateCamera(w http.ResponseWriter, r *http.Request) {
	if s.camCtrl == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "camera controller unavailable")
		return
	}
	id := r.PathValue("id")
	existing, ok := s.camCtrl.GetCamera(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "camera not found")
		return
	}

	var payload cameraPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 128*1024)).Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	cam := buildCameraFromPayload(payload, &existing, id, nil)
	saved, err := s.camCtrl.UpdateCamera(r.Context(), cam)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "camera not found")
			return
		}
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toCameraDTO(saved))
}

func (s *Server) handleDeleteCamera(w http.ResponseWriter, r *http.Request) {
	if s.camCtrl == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "camera controller unavailable")
		return
	}
	id := r.PathValue("id")
	if err := s.camCtrl.DeleteCamera(r.Context(), id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "camera not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *Server) handleProbeCamera(w http.ResponseWriter, r *http.Request) {
	if s.camCtrl == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "camera controller unavailable")
		return
	}
	var body struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
		CameraID string `json:"camera_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	pass := body.Password
	user := strings.TrimSpace(body.Username)
	if body.CameraID != "" {
		if existing, ok := s.camCtrl.GetCamera(body.CameraID); ok {
			existUser, existPass, hasPass := extractRTSPCreds(existing.MainRTSP)
			if !hasPass {
				existUser, existPass, hasPass = extractRTSPCreds(existing.SubRTSP)
			}
			if user == "" {
				if existing.Motion.Username != "" {
					user = existing.Motion.Username
				} else {
					user = existUser
				}
			}
			if pass == "" {
				if existing.Motion.Password != "" {
					pass = existing.Motion.Password
				} else if hasPass {
					pass = existPass
				}
			}
		}
	}

	res, err := s.camCtrl.ProbeCamera(r.Context(), motion.ProbeRequest{
		Host:     body.Host,
		Port:     body.Port,
		Username: user,
		Password: pass,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTestCamera(w http.ResponseWriter, r *http.Request) {
	if s.camCtrl == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "camera controller unavailable")
		return
	}
	var body struct {
		RTSPURL    string `json:"rtsp_url"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		CameraID   string `json:"camera_id"`
		StreamKind string `json:"stream_kind"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32*1024)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	var existingURL string
	if body.CameraID != "" {
		if existing, ok := s.camCtrl.GetCamera(body.CameraID); ok {
			if strings.EqualFold(strings.TrimSpace(body.StreamKind), "sub") {
				existingURL = existing.SubRTSP
			}
			if existingURL == "" {
				existingURL = existing.MainRTSP
			}
		}
	}

	resolved := resolveRTSP(body.RTSPURL, body.Username, body.Password, existingURL)
	if resolved == "" {
		writeJSONError(w, http.StatusBadRequest, "rtsp_url is required")
		return
	}

	res, err := s.camCtrl.TestStream(r.Context(), resolved)
	if err != nil {
		writeJSON(w, http.StatusOK, go2rtc.StreamTestResult{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
