package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"feathernvr/internal/config"
	"feathernvr/internal/go2rtc"
	"feathernvr/internal/motion"
	"feathernvr/internal/ptz"
	"feathernvr/internal/recorder"
	"feathernvr/internal/storage"
)

type mockPTZController struct {
	mu          sync.Mutex
	moves       [][3]float64
	stops       int
	presets     []ptz.Preset
	gotoPresets []string
}

func (m *mockPTZController) Move(ctx context.Context, pan, tilt, zoom float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.moves = append(m.moves, [3]float64{pan, tilt, zoom})
	return nil
}

func (m *mockPTZController) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops++
	return nil
}

func (m *mockPTZController) ListPresets(ctx context.Context) ([]ptz.Preset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ptz.Preset, len(m.presets))
	copy(out, m.presets)
	return out, nil
}

func (m *mockPTZController) GotoPreset(ctx context.Context, presetID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gotoPresets = append(m.gotoPresets, presetID)
	return nil
}

func TestPTZEndpoints(t *testing.T) {
	srv, _, _ := setupTestServer(t, "", "", nil)

	// No PTZ manager yet -> 404
	rr404 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr404, httptest.NewRequest(http.MethodGet, "/api/cameras/cam1/ptz/presets", nil))
	if rr404.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no PTZ manager set, got %d", rr404.Code)
	}

	mgr := ptz.NewManager()
	mock := &mockPTZController{
		presets: []ptz.Preset{{ID: "1", Name: "Home"}, {ID: "2", Name: "Gate"}},
	}
	mgr.Register("ptz_cam", mock)
	srv.SetPTZManager(mgr)

	// Non-PTZ camera returns 404
	rrMissing := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrMissing, httptest.NewRequest(http.MethodPost, "/api/cameras/other_cam/ptz/move", strings.NewReader(`{"pan":0.5}`)))
	if rrMissing.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on non-PTZ camera, got %d", rrMissing.Code)
	}

	// Move without duration_ms
	rrMove := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrMove, httptest.NewRequest(http.MethodPost, "/api/cameras/ptz_cam/ptz/move", strings.NewReader(`{"pan":0.5,"tilt":-0.25,"zoom":0.1}`)))
	if rrMove.Code != http.StatusOK {
		t.Fatalf("expected 200 on PTZ move, got %d: %s", rrMove.Code, rrMove.Body.String())
	}

	// Stop
	rrStop := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrStop, httptest.NewRequest(http.MethodPost, "/api/cameras/ptz_cam/ptz/stop", nil))
	if rrStop.Code != http.StatusOK {
		t.Fatalf("expected 200 on PTZ stop, got %d", rrStop.Code)
	}

	// Move with short duration_ms auto-stop
	rrTimed := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrTimed, httptest.NewRequest(http.MethodPost, "/api/cameras/ptz_cam/ptz/move", strings.NewReader(`{"pan":-0.5,"duration_ms":50}`)))
	if rrTimed.Code != http.StatusOK {
		t.Fatalf("expected 200 on timed PTZ move, got %d", rrTimed.Code)
	}
	deadline := time.Now().Add(1 * time.Second)
	for {
		mock.mu.Lock()
		stops := mock.stops
		mock.mu.Unlock()
		if stops >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	// List presets
	rrPresets := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrPresets, httptest.NewRequest(http.MethodGet, "/api/cameras/ptz_cam/ptz/presets", nil))
	if rrPresets.Code != http.StatusOK {
		t.Fatalf("expected 200 on presets, got %d", rrPresets.Code)
	}
	var presResp struct {
		Presets []ptz.Preset `json:"presets"`
	}
	if err := json.NewDecoder(rrPresets.Body).Decode(&presResp); err != nil {
		t.Fatalf("decode presets: %v", err)
	}
	if len(presResp.Presets) != 2 || presResp.Presets[0].ID != "1" || presResp.Presets[1].Name != "Gate" {
		t.Fatalf("unexpected presets response: %+v", presResp.Presets)
	}

	// Goto preset
	rrGoto := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrGoto, httptest.NewRequest(http.MethodPost, "/api/cameras/ptz_cam/ptz/presets/2", nil))
	if rrGoto.Code != http.StatusOK {
		t.Fatalf("expected 200 on goto preset, got %d", rrGoto.Code)
	}

	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.moves) != 2 || mock.moves[0] != [3]float64{0.5, -0.25, 0.1} {
		t.Fatalf("unexpected recorded moves: %+v", mock.moves)
	}
	if mock.stops < 2 {
		t.Fatalf("expected >= 2 stops (manual + auto-stop), got %d", mock.stops)
	}
	if len(mock.gotoPresets) != 1 || mock.gotoPresets[0] != "2" {
		t.Fatalf("unexpected gotoPresets: %+v", mock.gotoPresets)
	}
}

func setupTestServer(t *testing.T, authUser, authPass string, go2rtcTarget *url.URL) (*Server, *storage.DB, string) {
	t.Helper()
	root := t.TempDir()
	recDir := filepath.Join(root, "recordings")
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		t.Fatalf("mkdir recDir: %v", err)
	}
	db, err := storage.Open(filepath.Join(root, "nvr.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen:   ":0",
			Username: authUser,
			Password: authPass,
		},
		Storage: config.StorageConfig{
			Dir:              recDir,
			MaxGB:            10,
			HighWatermarkPct: 98,
			LowWatermarkPct:  92,
			SweepIntervalSec: 300,
		},
		Recording: config.RecordingConfig{
			PreRecordSec:  5,
			PostRecordSec: 10,
			MaxClipSec:    300,
			MaxBufferMB:   8,
		},
		Cameras: []config.CameraConfig{
			{
				ID:       "cam_secret",
				Name:     "Driveway",
				MainRTSP: "rtsp://admin:SUPER_SECRET_PASS@192.168.1.99/stream1",
				SubRTSP:  "rtsp://admin:SUPER_SECRET_PASS@192.168.1.99/stream2",
				Motion: config.MotionConfig{
					Mode:     config.MotionModeONVIF,
					ONVIFURL: "http://192.168.1.99/onvif/device_service",
					Username: "admin",
					Password: "SUPER_SECRET_ONVIF_PASSWORD",
				},
			},
		},
	}
	cfg.ApplyDefaults()

	retention := storage.NewRetentionManager(db, cfg.Storage, nil)
	mgr := recorder.NewManager()
	mgr.Register(recorder.NewCameraRecorder(
		cfg.Cameras[0], cfg.Recording, cfg.Storage,
		"http://127.0.0.1:1/mp4", "http://127.0.0.1:1/jpg",
		"cam_secret_sub", db, retention, nil, nil,
	))

	srv, err := New(cfg, db, retention, mgr, go2rtcTarget, nil)
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	return srv, db, recDir
}

func TestAPIStatusAndNoCredentialLeak(t *testing.T) {
	srv, _, _ := setupTestServer(t, "", "", nil)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "SUPER_SECRET") || strings.Contains(body, "rtsp://") {
		t.Fatalf("credentials leaked in /api/status response: %s", body)
	}

	var parsed statusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if len(parsed.Cameras) != 1 || parsed.Cameras[0].ID != "cam_secret" || parsed.Cameras[0].StreamSub != "cam_secret_sub" {
		t.Fatalf("unexpected cameras payload: %+v", parsed.Cameras)
	}
	if parsed.Storage.MaxBytes <= 0 {
		t.Fatalf("expected positive max_bytes")
	}
}

func TestRecordingsTriggerMediaRangeAndTraversal(t *testing.T) {
	srv, db, recDir := setupTestServer(t, "", "", nil)

	// Create a clip file + secret file outside recDir.
	outsideSecret := filepath.Join(filepath.Dir(recDir), "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("TOP_SECRET"), 0o644); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}

	relMP4 := "cam_secret/2026-11-19/clip.mp4"
	absMP4 := filepath.Join(recDir, filepath.FromSlash(relMP4))
	if err := os.MkdirAll(filepath.Dir(absMP4), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	if err := os.WriteFile(absMP4, payload, 0o644); err != nil {
		t.Fatalf("write mp4: %v", err)
	}

	rec := &storage.Recording{
		CameraID:      "cam_secret",
		CameraName:    "Driveway",
		StartTime:     1700000000,
		EndTime:       1700000010,
		DurationSec:   10,
		FilePath:      relMP4,
		SizeBytes:     int64(len(payload)),
		TriggerSource: "onvif",
	}
	id, err := db.InsertRecording(context.Background(), rec)
	if err != nil {
		t.Fatalf("insert recording: %v", err)
	}

	// GET /api/recordings
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/recordings?camera=cam_secret", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/recordings status=%d", rr.Code)
	}
	var recResp recordingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &recResp); err != nil {
		t.Fatalf("unmarshal recordings: %v", err)
	}
	if recResp.Total != 1 || len(recResp.Recordings) != 1 || recResp.Recordings[0].VideoURL != "/media/"+relMP4 {
		t.Fatalf("unexpected recordings response: %+v", recResp)
	}

	// GET /media/... with Range: bytes=0-9 -> 206 Partial Content
	rangeReq := httptest.NewRequest(http.MethodGet, "/media/"+relMP4, nil)
	rangeReq.Header.Set("Range", "bytes=0-9")
	rrRange := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrRange, rangeReq)
	if rrRange.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", rrRange.Code)
	}
	if !bytes.Equal(rrRange.Body.Bytes(), []byte("0123456789")) {
		t.Fatalf("unexpected range bytes: %q", rrRange.Body.String())
	}

	// Path traversal rejection
	for _, bad := range []string{"/media/../secret.txt", "/media/cam_secret/../../secret.txt", `/media/..\secret.txt`} {
		rrBad := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rrBad, httptest.NewRequest(http.MethodGet, bad, nil))
		if rrBad.Code == http.StatusOK {
			t.Fatalf("path traversal %q unexpectedly succeeded", bad)
		}
	}

	// POST /api/cameras/{id}/trigger
	rrTrig := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrTrig, httptest.NewRequest(http.MethodPost, "/api/cameras/cam_secret/trigger", nil))
	if rrTrig.Code != http.StatusOK {
		t.Fatalf("expected trigger 200, got %d", rrTrig.Code)
	}
	rrTrigMissing := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrTrigMissing, httptest.NewRequest(http.MethodPost, "/api/cameras/unknown/trigger", nil))
	if rrTrigMissing.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown trigger, got %d", rrTrigMissing.Code)
	}

	// DELETE /api/recordings/{id}
	rrDel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrDel, httptest.NewRequest(http.MethodDelete, "/api/recordings/"+itoa(id), nil))
	if rrDel.Code != http.StatusOK {
		t.Fatalf("expected delete 200, got %d: %s", rrDel.Code, rrDel.Body.String())
	}
	if _, err := os.Stat(absMP4); !os.IsNotExist(err) {
		t.Fatalf("expected deleted clip file to be removed from disk")
	}
}

func TestGo2RTCProxyAndBasicAuthAndStaticUI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/video-rtc.js" {
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = io.WriteString(w, "export class VideoRTC {}")
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	srv, _, _ := setupTestServer(t, "nvr", "hunter2", upURL)

	// Unauthenticated request rejected
	rrUnauth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrUnauth, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rrUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", rrUnauth.Code)
	}

	// Authenticated proxy request
	reqProxy := httptest.NewRequest(http.MethodGet, "/go2rtc/video-rtc.js", nil)
	reqProxy.SetBasicAuth("nvr", "hunter2")
	rrProxy := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrProxy, reqProxy)
	if rrProxy.Code != http.StatusOK || !strings.Contains(rrProxy.Body.String(), "class VideoRTC") {
		t.Fatalf("unexpected proxy response: %d %q", rrProxy.Code, rrProxy.Body.String())
	}

	// Authenticated embedded index.html
	reqRoot := httptest.NewRequest(http.MethodGet, "/", nil)
	reqRoot.SetBasicAuth("nvr", "hunter2")
	rrRoot := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrRoot, reqRoot)
	if rrRoot.Code != http.StatusOK || !strings.Contains(rrRoot.Body.String(), "FeatherNVR") {
		t.Fatalf("unexpected static root response: %d", rrRoot.Code)
	}
}

func TestStorageSettingsGetAndPut(t *testing.T) {
	srv, _, recDir := setupTestServer(t, "", "", nil)

	cfgPath := filepath.Join(filepath.Dir(recDir), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("storage:\n  max_gb: 10\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	srv.SetConfigPath(cfgPath)

	// GET current settings
	rrGet := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrGet, httptest.NewRequest(http.MethodGet, "/api/settings/storage", nil))
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET storage settings status=%d", rrGet.Code)
	}
	var got storageSettingsResponse
	if err := json.Unmarshal(rrGet.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal storage settings: %v", err)
	}
	if got.MaxGB != 10 {
		t.Fatalf("expected initial max_gb=10, got %v", got.MaxGB)
	}

	// PUT updated storage cap
	reqPut := httptest.NewRequest(http.MethodPut, "/api/settings/storage", strings.NewReader(`{"max_gb": 50}`))
	reqPut.Header.Set("Content-Type", "application/json")
	rrPut := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrPut, reqPut)
	if rrPut.Code != http.StatusOK {
		t.Fatalf("PUT storage settings status=%d body=%s", rrPut.Code, rrPut.Body.String())
	}
	var putResp storageSettingsResponse
	if err := json.Unmarshal(rrPut.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("unmarshal put response: %v", err)
	}
	if putResp.MaxGB != 50 || !putResp.Persisted {
		t.Fatalf("unexpected PUT response: %+v", putResp)
	}
	if srv.retention.MaxBytes() != int64(50)*1024*1024*1024 {
		t.Fatalf("expected live MaxBytes updated to 50GB, got %d", srv.retention.MaxBytes())
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if reloaded.Storage.MaxGB != 50 {
		t.Fatalf("expected persisted max_gb=50, got %v", reloaded.Storage.MaxGB)
	}

	// Reject invalid cap
	rrBad := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrBad, httptest.NewRequest(http.MethodPut, "/api/settings/storage", strings.NewReader(`{"max_gb": -5}`)))
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on negative max_gb, got %d", rrBad.Code)
	}
}

func TestRecordingDaysAndDayEndpoints(t *testing.T) {
	srv, db, _ := setupTestServer(t, "", "", nil)
	ctx := context.Background()

	// Use explicit local dates via Unix timestamps so test is timezone-independent.
	baseDay := mustParseLocalDate(t, "2026-11-19")
	prevDay := mustParseLocalDate(t, "2026-11-18")
	seed := []storage.Recording{
		{CameraID: "cam1", CameraName: "Front", StartTime: baseDay + 3600, EndTime: baseDay + 3620, DurationSec: 20, FilePath: "cam1/2026-11-19/c2.mp4", ThumbPath: "cam1/2026-11-19/c2.jpg", SizeBytes: 2000, TriggerSource: "pixel"},
		{CameraID: "cam1", CameraName: "Front", StartTime: baseDay + 600, EndTime: baseDay + 615, DurationSec: 15, FilePath: "cam1/2026-11-19/c1.mp4", ThumbPath: "cam1/2026-11-19/c1.jpg", SizeBytes: 1500, TriggerSource: "onvif"},
		{CameraID: "cam2", CameraName: "Gate", StartTime: prevDay + 1200, EndTime: prevDay + 1210, DurationSec: 10, FilePath: "cam2/2026-11-18/g1.mp4", ThumbPath: "cam2/2026-11-18/g1.jpg", SizeBytes: 1000, TriggerSource: "manual"},
	}
	for i := range seed {
		if _, err := db.InsertRecording(ctx, &seed[i]); err != nil {
			t.Fatalf("InsertRecording %d: %v", i, err)
		}
	}

	rrDays := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrDays, httptest.NewRequest(http.MethodGet, "/api/recordings/days", nil))
	if rrDays.Code != http.StatusOK {
		t.Fatalf("GET /api/recordings/days status=%d body=%s", rrDays.Code, rrDays.Body.String())
	}
	var daysResp recordingDaysResponse
	if err := json.Unmarshal(rrDays.Body.Bytes(), &daysResp); err != nil {
		t.Fatalf("unmarshal days: %v", err)
	}
	if daysResp.Total != 2 || len(daysResp.Days) != 2 {
		t.Fatalf("expected 2 camera-day entries, got %+v", daysResp)
	}
	if daysResp.Days[0].CameraID != "cam1" || daysResp.Days[0].ClipCount != 2 || daysResp.Days[0].ThumbURL != "/media/cam1/2026-11-19/c2.jpg" {
		t.Fatalf("unexpected days[0]: %+v", daysResp.Days[0])
	}

	// Single day chronological fetch
	rrDay := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrDay, httptest.NewRequest(http.MethodGet, "/api/recordings/day?camera=cam1&date=2026-11-19", nil))
	if rrDay.Code != http.StatusOK {
		t.Fatalf("GET /api/recordings/day status=%d body=%s", rrDay.Code, rrDay.Body.String())
	}
	var dayResp recordingDayDetailResponse
	if err := json.Unmarshal(rrDay.Body.Bytes(), &dayResp); err != nil {
		t.Fatalf("unmarshal day: %v", err)
	}
	if dayResp.CameraID != "cam1" || len(dayResp.Clips) != 2 ||
		dayResp.Clips[0].FilePath != "cam1/2026-11-19/c1.mp4" ||
		dayResp.Clips[1].FilePath != "cam1/2026-11-19/c2.mp4" {
		t.Fatalf("unexpected day clips: %+v", dayResp)
	}
	if dayResp.DayStart != baseDay || dayResp.DayEnd <= dayResp.DayStart {
		t.Fatalf("unexpected day bounds: start=%d end=%d", dayResp.DayStart, dayResp.DayEnd)
	}

	// Missing/invalid params -> 400
	for _, u := range []string{"/api/recordings/day?camera=cam1", "/api/recordings/day?date=2026-11-19", "/api/recordings/day?camera=cam1&date=bad"} {
		rrErr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rrErr, httptest.NewRequest(http.MethodGet, u, nil))
		if rrErr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d", u, rrErr.Code)
		}
	}
}

func TestLayoutSettingsEndpoint(t *testing.T) {
	srv, _, _ := setupTestServer(t, "", "", nil)

	// Default -> []
	rrGet := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrGet, httptest.NewRequest(http.MethodGet, "/api/settings/layout", nil))
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/settings/layout status=%d", rrGet.Code)
	}
	var got struct {
		CameraOrder []string `json:"camera_order"`
	}
	if err := json.Unmarshal(rrGet.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal layout: %v", err)
	}
	if got.CameraOrder == nil || len(got.CameraOrder) != 0 {
		t.Fatalf("expected empty non-null array, got %#v", got.CameraOrder)
	}

	// PUT order (including duplicate + whitespace to verify normalization)
	rrPut := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrPut, httptest.NewRequest(http.MethodPut, "/api/settings/layout", strings.NewReader(`{"camera_order":[" cam2 ","cam1","cam2",""]}`)))
	if rrPut.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings/layout status=%d body=%s", rrPut.Code, rrPut.Body.String())
	}

	// Read back persisted order
	rrGet2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrGet2, httptest.NewRequest(http.MethodGet, "/api/settings/layout", nil))
	if rrGet2.Code != http.StatusOK {
		t.Fatalf("GET2 status=%d", rrGet2.Code)
	}
	if err := json.Unmarshal(rrGet2.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal layout 2: %v", err)
	}
	if len(got.CameraOrder) != 2 || got.CameraOrder[0] != "cam2" || got.CameraOrder[1] != "cam1" {
		t.Fatalf("expected [cam2 cam1], got %+v", got.CameraOrder)
	}

	// Bad JSON -> 400
	rrBad := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrBad, httptest.NewRequest(http.MethodPut, "/api/settings/layout", strings.NewReader(`{not json`)))
	if rrBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad json, got %d", rrBad.Code)
	}
}

type mockCameraController struct {
	mu          sync.Mutex
	cameras     []config.CameraConfig
	dvrs        []config.HikvisionDVRConfig
	lastTestURL string
	lastProbe   motion.ProbeRequest
}

func (m *mockCameraController) ListCameras() []config.CameraConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]config.CameraConfig, len(m.cameras))
	copy(out, m.cameras)
	return out
}

func (m *mockCameraController) GetCamera(id string) (config.CameraConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cameras {
		if c.ID == id {
			return c, true
		}
	}
	return config.CameraConfig{}, false
}

func (m *mockCameraController) ListDVRs() []config.HikvisionDVRConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]config.HikvisionDVRConfig, len(m.dvrs))
	copy(out, m.dvrs)
	return out
}

func (m *mockCameraController) AddCamera(ctx context.Context, cam config.CameraConfig) (config.CameraConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cameras = append(m.cameras, cam)
	return cam, nil
}

func (m *mockCameraController) UpdateCamera(ctx context.Context, cam config.CameraConfig) (config.CameraConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.cameras {
		if m.cameras[i].ID == cam.ID {
			m.cameras[i] = cam
			return cam, nil
		}
	}
	return config.CameraConfig{}, os.ErrNotExist
}

func (m *mockCameraController) DeleteCamera(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.cameras {
		if m.cameras[i].ID == id {
			m.cameras = append(m.cameras[:i], m.cameras[i+1:]...)
			return nil
		}
	}
	return os.ErrNotExist
}

func (m *mockCameraController) TestStream(ctx context.Context, rawURL string) (*go2rtc.StreamTestResult, error) {
	m.mu.Lock()
	m.lastTestURL = rawURL
	m.mu.Unlock()
	return &go2rtc.StreamTestResult{
		OK:        true,
		LatencyMS: 42,
		Width:     1920,
		Height:    1080,
		Codec:     "h264",
	}, nil
}

func (m *mockCameraController) ProbeCamera(ctx context.Context, req motion.ProbeRequest) (*motion.ProbeResult, error) {
	m.mu.Lock()
	m.lastProbe = req
	m.mu.Unlock()
	return &motion.ProbeResult{
		Name:                "Acme CamPro",
		Manufacturer:        "Acme",
		Model:               "CamPro",
		ONVIFURL:            "http://" + req.Host + "/onvif/device_service",
		SuggestedMotionMode: config.MotionModeONVIF,
		MainRTSP:            "rtsp://" + req.Host + "/main",
		SubRTSP:             "rtsp://" + req.Host + "/sub",
		PTZEnabled:          true,
		PTZType:             "onvif",
		PTZURL:              "http://" + req.Host + "/onvif/ptz_service",
		PTZProfileToken:     "prof1",
		Profiles: []motion.ProbeProfile{
			{Name: "Main", Token: "prof1", Width: 1920, Height: 1080, Codec: "H264", FPS: 25, RTSPURL: "rtsp://" + req.Host + "/main"},
			{Name: "Sub", Token: "prof2", Width: 640, Height: 360, Codec: "H264", FPS: 15, RTSPURL: "rtsp://" + req.Host + "/sub"},
		},
	}, nil
}

func TestCameraCRUDProbeTestAndRedaction(t *testing.T) {
	srv, _, _ := setupTestServer(t, "", "", nil)
	enabledTrue := true
	mock := &mockCameraController{
		cameras: []config.CameraConfig{
			{
				ID:         "existing_cam",
				Name:       "Existing",
				Enabled:    &enabledTrue,
				MainRTSP:   "rtsp://admin:TOP_SECRET_RTSP@192.168.1.10/stream1",
				SubRTSP:    "rtsp://admin:TOP_SECRET_RTSP@192.168.1.10/stream2",
				LiveStream: "sub",
				Motion: config.MotionConfig{
					Mode:     config.MotionModeONVIF,
					ONVIFURL: "http://192.168.1.10/onvif/device_service",
					Username: "admin",
					Password: "TOP_SECRET_ONVIF",
				},
				PTZ: config.PTZConfig{
					Enabled:      true,
					Type:         "onvif",
					URL:          "http://192.168.1.10/onvif/device_service",
					Username:     "admin",
					Password:     "TOP_SECRET_PTZ",
					ProfileToken: "p1",
				},
			},
		},
		dvrs: []config.HikvisionDVRConfig{{ID: "dvr1", Host: "192.168.1.200", Port: 80}},
	}
	srv.SetCameraController(mock)

	// 1. GET /api/cameras and GET /api/cameras/{id} must redact passwords
	rrList := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrList, httptest.NewRequest(http.MethodGet, "/api/cameras", nil))
	if rrList.Code != http.StatusOK {
		t.Fatalf("GET /api/cameras status=%d body=%s", rrList.Code, rrList.Body.String())
	}
	if strings.Contains(rrList.Body.String(), "TOP_SECRET") {
		t.Fatalf("plaintext password leaked in GET /api/cameras: %s", rrList.Body.String())
	}
	if !strings.Contains(rrList.Body.String(), "********") {
		t.Fatalf("expected redacted password marker in GET /api/cameras: %s", rrList.Body.String())
	}

	rrGet := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrGet, httptest.NewRequest(http.MethodGet, "/api/cameras/existing_cam", nil))
	if rrGet.Code != http.StatusOK {
		t.Fatalf("GET /api/cameras/existing_cam status=%d body=%s", rrGet.Code, rrGet.Body.String())
	}
	if strings.Contains(rrGet.Body.String(), "TOP_SECRET") {
		t.Fatalf("plaintext password leaked in GET /api/cameras/{id}: %s", rrGet.Body.String())
	}

	// 2. PUT preserving existing RTSP/Motion/PTZ passwords when ******** / blank is sent
	putPayload := `{
		"name": "Existing Renamed",
		"main_rtsp": "rtsp://admin:********@192.168.1.10/stream1",
		"sub_rtsp": "rtsp://admin:********@192.168.1.10/stream2",
		"username": "admin",
		"password": "",
		"live_stream": "main",
		"aspect_ratio": "fill",
		"resolution": "1920x1080",
		"motion": {"mode": "onvif", "onvif_url": "http://192.168.1.10/onvif/device_service", "username": "admin", "password": "", "keep_password": true},
		"ptz": {"enabled": true, "type": "onvif", "url": "http://192.168.1.10/onvif/device_service", "username": "admin", "password": "", "keep_password": true, "profile_token": "p1"}
	}`
	rrPut := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrPut, httptest.NewRequest(http.MethodPut, "/api/cameras/existing_cam", strings.NewReader(putPayload)))
	if rrPut.Code != http.StatusOK {
		t.Fatalf("PUT /api/cameras/existing_cam status=%d body=%s", rrPut.Code, rrPut.Body.String())
	}
	updated, ok := mock.GetCamera("existing_cam")
	if !ok {
		t.Fatalf("expected existing_cam in controller")
	}
	if updated.MainRTSP != "rtsp://admin:TOP_SECRET_RTSP@192.168.1.10/stream1" ||
		updated.SubRTSP != "rtsp://admin:TOP_SECRET_RTSP@192.168.1.10/stream2" {
		t.Fatalf("expected RTSP password preserved, got main=%q sub=%q", updated.MainRTSP, updated.SubRTSP)
	}
	if updated.Motion.Password != "TOP_SECRET_ONVIF" || updated.PTZ.Password != "TOP_SECRET_PTZ" {
		t.Fatalf("expected Motion/PTZ passwords preserved, got motion=%q ptz=%q", updated.Motion.Password, updated.PTZ.Password)
	}
	if updated.LiveStream != "main" || updated.AspectRatio != "fill" || updated.Resolution != "1920x1080" {
		t.Fatalf("expected updated display settings, got %+v", updated)
	}

	// 3. POST /api/cameras/test resolves ******** to stored password
	testPayload := `{"camera_id":"existing_cam","stream_kind":"main","rtsp_url":"rtsp://admin:********@192.168.1.10/stream1","username":"admin","password":""}`
	rrTest := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrTest, httptest.NewRequest(http.MethodPost, "/api/cameras/test", strings.NewReader(testPayload)))
	if rrTest.Code != http.StatusOK {
		t.Fatalf("POST /api/cameras/test status=%d body=%s", rrTest.Code, rrTest.Body.String())
	}
	if mock.lastTestURL != "rtsp://admin:TOP_SECRET_RTSP@192.168.1.10/stream1" {
		t.Fatalf("expected resolved RTSP password in TestStream call, got %q", mock.lastTestURL)
	}

	// 4. POST /api/cameras creates camera with auto-slug ID and credential injection
	createPayload := `{
		"name": "Back Yard Cam!",
		"main_rtsp": "rtsp://192.168.1.55/stream1",
		"sub_rtsp": "rtsp://192.168.1.55/stream2",
		"username": "camuser",
		"password": "NEW_CAM_PASS",
		"live_stream": "sub",
		"motion": {"mode": "pixel"},
		"ptz": {"enabled": false}
	}`
	rrCreate := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrCreate, httptest.NewRequest(http.MethodPost, "/api/cameras", strings.NewReader(createPayload)))
	if rrCreate.Code != http.StatusCreated {
		t.Fatalf("POST /api/cameras status=%d body=%s", rrCreate.Code, rrCreate.Body.String())
	}
	if strings.Contains(rrCreate.Body.String(), "NEW_CAM_PASS") {
		t.Fatalf("plaintext password leaked in POST /api/cameras response: %s", rrCreate.Body.String())
	}
	var createdDTO cameraDTO
	if err := json.Unmarshal(rrCreate.Body.Bytes(), &createdDTO); err != nil {
		t.Fatalf("unmarshal created DTO: %v", err)
	}
	if createdDTO.ID != "back_yard_cam" {
		t.Fatalf("expected auto-slug id back_yard_cam, got %q", createdDTO.ID)
	}
	storedCreated, ok := mock.GetCamera("back_yard_cam")
	if !ok {
		t.Fatalf("created camera missing in controller")
	}
	if storedCreated.MainRTSP != "rtsp://camuser:NEW_CAM_PASS@192.168.1.55/stream1" {
		t.Fatalf("expected injected credentials in stored MainRTSP, got %q", storedCreated.MainRTSP)
	}

	// 5. POST /api/cameras/probe forwards credentials and returns detected profiles
	probePayload := `{"host":"192.168.1.55","port":80,"username":"camuser","password":"NEW_CAM_PASS"}`
	rrProbe := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrProbe, httptest.NewRequest(http.MethodPost, "/api/cameras/probe", strings.NewReader(probePayload)))
	if rrProbe.Code != http.StatusOK {
		t.Fatalf("POST /api/cameras/probe status=%d body=%s", rrProbe.Code, rrProbe.Body.String())
	}
	if mock.lastProbe.Host != "192.168.1.55" || mock.lastProbe.Username != "camuser" || mock.lastProbe.Password != "NEW_CAM_PASS" {
		t.Fatalf("unexpected probe request forwarded: %+v", mock.lastProbe)
	}
	if !strings.Contains(rrProbe.Body.String(), `"main_rtsp":"rtsp://192.168.1.55/main"`) {
		t.Fatalf("unexpected probe response: %s", rrProbe.Body.String())
	}

	// 6. DELETE /api/cameras/{id} removes camera and subsequent GET returns 404
	rrDel := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrDel, httptest.NewRequest(http.MethodDelete, "/api/cameras/back_yard_cam", nil))
	if rrDel.Code != http.StatusOK {
		t.Fatalf("DELETE /api/cameras/back_yard_cam status=%d body=%s", rrDel.Code, rrDel.Body.String())
	}
	rrAfter := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrAfter, httptest.NewRequest(http.MethodGet, "/api/cameras/back_yard_cam", nil))
	if rrAfter.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", rrAfter.Code)
	}
}

func mustParseLocalDate(t *testing.T, dateStr string) int64 {
	t.Helper()
	// Imported time already exists in server_test.go.
	dt, err := time.ParseInLocation("2006-01-02", dateStr, time.Local)
	if err != nil {
		t.Fatalf("parse local date: %v", err)
	}
	return dt.Unix()
}

func itoa(n int64) string {
	return string(bytes.TrimSpace([]byte(jsonNumber(n))))
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestRecordingFrameEndpoint(t *testing.T) {
	srv, db, recDir := setupTestServer(t, "", "", nil)
	ctx := context.Background()

	// Invalid IDs -> 400
	for _, u := range []string{"/api/recordings/abc/frame", "/api/recordings/0/frame"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, u, nil))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d", u, rr.Code)
		}
	}

	// Invalid t query parameter -> 400
	for _, u := range []string{"/api/recordings/1/frame?t=-1", "/api/recordings/1/frame?t=bad", "/api/recordings/1/frame?t=NaN"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, u, nil))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d", u, rr.Code)
		}
	}

	// Non-existent recording ID -> 404
	rrMissing := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrMissing, httptest.NewRequest(http.MethodGet, "/api/recordings/99999/frame?t=0", nil))
	if rrMissing.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing recording, got %d", rrMissing.Code)
	}

	// Path traversal in recording file_path -> 403
	outsideSecret := filepath.Join(filepath.Dir(recDir), "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("SECRET"), 0o644); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	badID, err := db.InsertRecording(ctx, &storage.Recording{
		CameraID:    "cam_secret",
		CameraName:  "Driveway",
		StartTime:   1700000000,
		EndTime:     1700000010,
		DurationSec: 10,
		FilePath:    "../secret.txt",
		SizeBytes:   6,
	})
	if err != nil {
		t.Fatalf("insert traversal recording: %v", err)
	}
	rrTrav := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrTrav, httptest.NewRequest(http.MethodGet, "/api/recordings/"+strconv.FormatInt(badID, 10)+"/frame?t=0", nil))
	if rrTrav.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for traversal recording path, got %d", rrTrav.Code)
	}

	ffmpegBin := config.DetectFFmpeg()
	if ffmpegBin == "" {
		t.Skip("ffmpeg not found on system")
	}
	srv.cfg.FFmpegBin = ffmpegBin

	relMP4 := "cam_secret/2026-11-23/preview.mp4"
	absMP4 := filepath.Join(recDir, filepath.FromSlash(relMP4))
	if err := os.MkdirAll(filepath.Dir(absMP4), 0o755); err != nil {
		t.Fatalf("mkdir clip dir: %v", err)
	}
	genCmd := exec.Command(ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=320x180:r=10:d=2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "5",
		"-movflags", "+faststart", "-f", "mp4", "-y", absMP4,
	)
	if out, genErr := genCmd.CombinedOutput(); genErr != nil {
		t.Fatalf("generate synthetic mp4 failed (%v): %s", genErr, string(out))
	}
	st, err := os.Stat(absMP4)
	if err != nil {
		t.Fatalf("stat synthetic mp4: %v", err)
	}

	recID, err := db.InsertRecording(ctx, &storage.Recording{
		CameraID:      "cam_secret",
		CameraName:    "Driveway",
		StartTime:     1700000100,
		EndTime:       1700000102,
		DurationSec:   2.0,
		FilePath:      relMP4,
		SizeBytes:     st.Size(),
		TriggerSource: "manual",
	})
	if err != nil {
		t.Fatalf("insert valid recording: %v", err)
	}

	frameURL := "/api/recordings/" + strconv.FormatInt(recID, 10) + "/frame?t=1.0"
	rrFrame := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrFrame, httptest.NewRequest(http.MethodGet, frameURL, nil))
	if rrFrame.Code != http.StatusOK {
		t.Fatalf("expected 200 on frame extract, got %d: %s", rrFrame.Code, rrFrame.Body.String())
	}
	if ct := rrFrame.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("expected Content-Type image/jpeg, got %q", ct)
	}
	jpg := rrFrame.Body.Bytes()
	if len(jpg) < 4 || jpg[0] != 0xFF || jpg[1] != 0xD8 {
		t.Fatalf("expected valid JPEG SOI header, got %d bytes", len(jpg))
	}

	// Remove source MP4 to prove the second identical request is served from frameCache.
	if err := os.Remove(absMP4); err != nil {
		t.Fatalf("remove synthetic mp4: %v", err)
	}
	rrCached := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrCached, httptest.NewRequest(http.MethodGet, frameURL, nil))
	if rrCached.Code != http.StatusOK {
		t.Fatalf("expected 200 from frameCache after mp4 removal, got %d", rrCached.Code)
	}
	if !bytes.Equal(rrCached.Body.Bytes(), jpg) {
		t.Fatalf("cached JPEG bytes did not match initial response")
	}
}
