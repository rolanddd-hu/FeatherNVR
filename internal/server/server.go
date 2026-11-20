package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"feathernvr/internal/config"
	"feathernvr/internal/motion"
	"feathernvr/internal/ptz"
	"feathernvr/internal/recorder"
	"feathernvr/internal/storage"
	"feathernvr/internal/web"
)

// Server serves the web UI, REST API, safe media Range server, and go2rtc proxy.
type Server struct {
	cfg           *config.Config
	configPath    string
	db            *storage.DB
	ptzMgr        *ptz.Manager
	ptzTimers     ptzTimers
	camCtrl       CameraController
	retention     *storage.RetentionManager
	recorders     *recorder.Manager
	go2rtcBaseURL *url.URL
	storageAbs    string
	startedAt     time.Time
	logger        *log.Logger
	handler       http.Handler
	httpServer    *http.Server
}

// New constructs the HTTP Server with all routes and optional Basic Auth middleware.
func New(
	cfg *config.Config,
	db *storage.DB,
	retention *storage.RetentionManager,
	recorders *recorder.Manager,
	go2rtcBaseURL *url.URL,
	logger *log.Logger,
) (*Server, error) {
	if logger == nil {
		logger = log.Default()
	}
	storageAbs, err := filepath.Abs(cfg.Storage.Dir)
	if err != nil {
		return nil, fmt.Errorf("resolve storage abs path: %w", err)
	}

	s := &Server{
		cfg:           cfg,
		db:            db,
		retention:     retention,
		recorders:     recorders,
		go2rtcBaseURL: go2rtcBaseURL,
		storageAbs:    storageAbs,
		startedAt:     time.Now(),
		logger:        logger,
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	var h http.Handler = mux
	if cfg.Server.Username != "" {
		h = s.basicAuthMiddleware(h)
	}
	s.handler = h
	s.httpServer = &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // required for WebSockets and streaming proxies
	}
	return s, nil
}

// SetConfigPath sets the config file path used to persist runtime settings updates.
func (s *Server) SetConfigPath(path string) {
	s.configPath = path
}

// Handler returns the root HTTP handler (useful for httptest in unit tests).
func (s *Server) Handler() http.Handler {
	return s.handler
}

// ListenAndServe starts the HTTP server until Shutdown is called.
func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	if s.go2rtcBaseURL != nil {
		target := s.go2rtcBaseURL
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = pr.In.Host
			},
			FlushInterval: -1,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		}
		mux.Handle("/go2rtc/", http.StripPrefix("/go2rtc", proxy))
	}

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/settings/storage", s.handleGetStorageSettings)
	mux.HandleFunc("PUT /api/settings/storage", s.handlePutStorageSettings)
	mux.HandleFunc("GET /api/settings/layout", s.handleGetLayoutSettings)
	mux.HandleFunc("PUT /api/settings/layout", s.handlePutLayoutSettings)
	mux.HandleFunc("GET /api/recordings", s.handleListRecordings)
	mux.HandleFunc("GET /api/recordings/days", s.handleListRecordingDays)
	mux.HandleFunc("GET /api/recordings/day", s.handleGetRecordingDay)
	mux.HandleFunc("DELETE /api/recordings/{id}", s.handleDeleteRecording)
	mux.HandleFunc("GET /api/cameras", s.handleListCameras)
	mux.HandleFunc("POST /api/cameras", s.handleCreateCamera)
	mux.HandleFunc("POST /api/cameras/probe", s.handleProbeCamera)
	mux.HandleFunc("POST /api/cameras/test", s.handleTestCamera)
	mux.HandleFunc("GET /api/cameras/{id}", s.handleGetCamera)
	mux.HandleFunc("PUT /api/cameras/{id}", s.handleUpdateCamera)
	mux.HandleFunc("DELETE /api/cameras/{id}", s.handleDeleteCamera)
	mux.HandleFunc("POST /api/cameras/{id}/trigger", s.handleTriggerCamera)
	mux.HandleFunc("POST /api/cameras/{id}/ptz/move", s.handlePTZMove)
	mux.HandleFunc("POST /api/cameras/{id}/ptz/stop", s.handlePTZStop)
	mux.HandleFunc("GET /api/cameras/{id}/ptz/presets", s.handlePTZPresets)
	mux.HandleFunc("POST /api/cameras/{id}/ptz/presets/{preset}", s.handlePTZGotoPreset)
	mux.HandleFunc("GET /api/discover", s.handleDiscover)
	mux.HandleFunc("GET /media/", s.handleMedia)
	mux.HandleFunc("HEAD /media/", s.handleMedia)

	staticFS := http.FileServerFS(web.StaticFS())
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		staticFS.ServeHTTP(w, r)
	}))
}

func (s *Server) basicAuthMiddleware(next http.Handler) http.Handler {
	expectedUser := []byte(s.cfg.Server.Username)
	expectedPass := []byte(s.cfg.Server.Password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), expectedUser) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), expectedPass) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="FeatherNVR"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusStorageResponse struct {
	UsedBytes      int64   `json:"used_bytes"`
	MaxBytes       int64   `json:"max_bytes"`
	Percent        float64 `json:"percent"`
	RecordingCount int64   `json:"recording_count"`
	OldestTime     int64   `json:"oldest_time"`
	NewestTime     int64   `json:"newest_time"`
}

type statusResponse struct {
	UptimeSec int64                   `json:"uptime_sec"`
	Storage   statusStorageResponse   `json:"storage"`
	Cameras   []recorder.CameraStatus `json:"cameras"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.TotalStats(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	maxBytes := s.retention.MaxBytes()
	pct := 0.0
	if maxBytes > 0 {
		pct = (float64(stats.TotalBytes) * 100.0) / float64(maxBytes)
	}

	var cams []recorder.CameraStatus
	if s.recorders != nil {
		cams = s.recorders.Statuses()
	} else {
		cams = []recorder.CameraStatus{}
	}

	resp := statusResponse{
		UptimeSec: int64(time.Since(s.startedAt).Seconds()),
		Storage: statusStorageResponse{
			UsedBytes:      stats.TotalBytes,
			MaxBytes:       maxBytes,
			Percent:        pct,
			RecordingCount: stats.Count,
			OldestTime:     stats.OldestUnix,
			NewestTime:     stats.NewestUnix,
		},
		Cameras: cams,
	}
	writeJSON(w, http.StatusOK, resp)
}

type recordingItem struct {
	storage.Recording
	VideoURL string `json:"video_url"`
	ThumbURL string `json:"thumb_url"`
}

type recordingsResponse struct {
	Recordings []recordingItem `json:"recordings"`
	Total      int64           `json:"total"`
	Limit      int             `json:"limit"`
	Offset     int             `json:"offset"`
}

type recordingDayItem struct {
	storage.RecordingDay
	ThumbURL string `json:"thumb_url"`
}

type recordingDaysResponse struct {
	Days   []recordingDayItem `json:"days"`
	Total  int64              `json:"total"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

type recordingDayDetailResponse struct {
	CameraID   string          `json:"camera_id"`
	CameraName string          `json:"camera_name"`
	Date       string          `json:"date"`
	DayStart   int64           `json:"day_start"`
	DayEnd     int64           `json:"day_end"`
	Clips      []recordingItem `json:"clips"`
}

func parsePagination(q url.Values) (int, int) {
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

func toRecordingItems(recs []storage.Recording) []recordingItem {
	items := make([]recordingItem, 0, len(recs))
	for _, rec := range recs {
		item := recordingItem{
			Recording: rec,
			VideoURL:  "/media/" + strings.TrimPrefix(rec.FilePath, "/"),
		}
		if rec.ThumbPath != "" {
			item.ThumbURL = "/media/" + strings.TrimPrefix(rec.ThumbPath, "/")
		}
		items = append(items, item)
	}
	return items
}

func (s *Server) handleListRecordings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	camera := q.Get("camera")
	date := q.Get("date")
	limit, offset := parsePagination(q)

	recs, total, err := s.db.ListRecordings(r.Context(), camera, date, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, recordingsResponse{
		Recordings: toRecordingItems(recs),
		Total:      total,
		Limit:      limit,
		Offset:     offset,
	})
}

func (s *Server) handleListRecordingDays(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	camera := q.Get("camera")
	date := q.Get("date")
	limit, offset := parsePagination(q)

	days, total, err := s.db.ListRecordingDays(r.Context(), camera, date, limit, offset)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	items := make([]recordingDayItem, 0, len(days))
	for _, d := range days {
		item := recordingDayItem{RecordingDay: d}
		if d.ThumbPath != "" {
			item.ThumbURL = "/media/" + strings.TrimPrefix(d.ThumbPath, "/")
		}
		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, recordingDaysResponse{
		Days:   items,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

func (s *Server) handleGetRecordingDay(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	camera := strings.TrimSpace(q.Get("camera"))
	date := strings.TrimSpace(q.Get("date"))
	if camera == "" || date == "" {
		writeJSONError(w, http.StatusBadRequest, "camera and date query parameters are required")
		return
	}

	clips, dayStart, dayEnd, err := s.db.ListDayRecordings(r.Context(), camera, date)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	camName := camera
	for _, c := range clips {
		if c.CameraName != "" {
			camName = c.CameraName
			break
		}
	}
	if camName == camera && s.cfg != nil {
		for _, c := range s.cfg.Cameras {
			if c.ID == camera && c.Name != "" {
				camName = c.Name
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, recordingDayDetailResponse{
		CameraID:   camera,
		CameraName: camName,
		Date:       date,
		DayStart:   dayStart,
		DayEnd:     dayEnd,
		Clips:      toRecordingItems(clips),
	})
}

func (s *Server) handleDeleteRecording(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid recording id")
		return
	}

	if err := s.retention.DeleteRecordingByID(r.Context(), id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "recording not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": id})
}

func (s *Server) handleTriggerCamera(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.recorders == nil || !s.recorders.TriggerManual(id) {
		writeJSONError(w, http.StatusNotFound, "camera not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"triggered": id})
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	devices, err := motion.Discover(r.Context(), 3*time.Second)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

type storageSettingsResponse struct {
	MaxGB            float64 `json:"max_gb"`
	HighWatermarkPct float64 `json:"high_watermark_pct"`
	LowWatermarkPct  float64 `json:"low_watermark_pct"`
	MaxBytes         int64   `json:"max_bytes"`
	Persisted        bool    `json:"persisted,omitempty"`
}

func (s *Server) handleGetStorageSettings(w http.ResponseWriter, _ *http.Request) {
	maxGB, highPct, lowPct := s.retention.Settings()
	writeJSON(w, http.StatusOK, storageSettingsResponse{
		MaxGB:            maxGB,
		HighWatermarkPct: highPct,
		LowWatermarkPct:  lowPct,
		MaxBytes:         s.retention.MaxBytes(),
	})
}

func (s *Server) handlePutStorageSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MaxGB float64 `json:"max_gb"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if math.IsNaN(body.MaxGB) || math.IsInf(body.MaxGB, 0) || body.MaxGB <= 0 || body.MaxGB > 1000000 {
		writeJSONError(w, http.StatusBadRequest, "max_gb must be > 0 and <= 1000000")
		return
	}

	persisted := false
	if s.configPath != "" {
		if err := config.SaveStorageCap(s.configPath, body.MaxGB); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		persisted = true
	}

	s.retention.SetMaxGB(body.MaxGB)
	maxGB, highPct, lowPct := s.retention.Settings()
	writeJSON(w, http.StatusOK, storageSettingsResponse{
		MaxGB:            maxGB,
		HighWatermarkPct: highPct,
		LowWatermarkPct:  lowPct,
		MaxBytes:         s.retention.MaxBytes(),
		Persisted:        persisted,
	})
}

func (s *Server) handleGetLayoutSettings(w http.ResponseWriter, r *http.Request) {
	raw, err := s.db.GetSetting(r.Context(), "camera_order")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	order := []string{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &order); err != nil || order == nil {
			order = []string{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"camera_order": order})
}

func (s *Server) handlePutLayoutSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CameraOrder []string `json:"camera_order"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	seen := make(map[string]struct{}, len(body.CameraOrder))
	clean := make([]string, 0, len(body.CameraOrder))
	for _, rawID := range body.CameraOrder {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
		if len(clean) >= 500 {
			break
		}
	}

	encoded, err := json.Marshal(clean)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.db.SetSetting(r.Context(), "camera_order", string(encoded)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"camera_order": clean})
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	rawRel := strings.TrimPrefix(r.URL.Path, "/media/")
	if rawRel == "" || strings.ContainsRune(rawRel, '\\') || strings.ContainsRune(rawRel, 0) {
		http.NotFound(w, r)
		return
	}
	for _, seg := range strings.Split(rawRel, "/") {
		if seg == ".." {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}

	cleaned := strings.TrimPrefix(path.Clean("/"+rawRel), "/")
	if cleaned == "" || cleaned == "." {
		http.NotFound(w, r)
		return
	}

	abs := filepath.Join(s.storageAbs, filepath.FromSlash(cleaned))
	relCheck, err := filepath.Rel(s.storageAbs, abs)
	if err != nil || relCheck == "." || strings.HasPrefix(relCheck, "..") || filepath.IsAbs(relCheck) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	if strings.HasSuffix(strings.ToLower(abs), ".jpg") {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeFile(w, r, abs)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
