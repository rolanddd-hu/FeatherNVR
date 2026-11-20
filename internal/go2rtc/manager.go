package go2rtc

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"feathernvr/internal/config"
	"feathernvr/internal/fmp4"
	"feathernvr/internal/procjob"
)

type runtimeConfig struct {
	API struct {
		Listen string `yaml:"listen"`
		Origin string `yaml:"origin,omitempty"`
	} `yaml:"api"`
	RTSP struct {
		Listen string `yaml:"listen"`
	} `yaml:"rtsp"`
	WebRTC struct {
		Listen string `yaml:"listen"`
	} `yaml:"webrtc"`
	FFmpeg  *ffmpegRuntimeConfig `yaml:"ffmpeg,omitempty"`
	Streams map[string]string    `yaml:"streams"`
}

type ffmpegRuntimeConfig struct {
	Bin string `yaml:"bin"`
}

// StreamTestResult reports the outcome and detected stream parameters from probing a URL through go2rtc.
type StreamTestResult struct {
	OK          bool   `json:"ok"`
	LatencyMS   int64  `json:"latency_ms"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Codec       string `json:"codec,omitempty"`
	SnapshotB64 string `json:"snapshot_b64,omitempty"`
	Error       string `json:"error,omitempty"`
}

var probeSeq atomic.Uint64

// Manager supervises the local go2rtc relay process and generates its config.
type Manager struct {
	mu      sync.RWMutex
	cfg     *config.Config
	job     *procjob.Manager
	logger  *log.Logger
	client  *http.Client
	binPath string

	wg sync.WaitGroup
}

// New creates a go2rtc process manager.
func New(cfg *config.Config, job *procjob.Manager, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{
		cfg:    cfg,
		job:    job,
		logger: logger,
		client: &http.Client{Timeout: 2 * time.Second},
	}
}

// RenderConfigYAML builds the runtime go2rtc YAML configuration.
func (m *Manager) RenderConfigYAML() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.renderConfigYAMLLocked()
}

func (m *Manager) renderConfigYAMLLocked() ([]byte, error) {
	var rc runtimeConfig
	rc.API.Listen = fmt.Sprintf("127.0.0.1:%d", m.cfg.Go2RTC.APIPort)
	rc.API.Origin = "*"
	rc.RTSP.Listen = fmt.Sprintf("127.0.0.1:%d", m.cfg.Go2RTC.RTSPPort)
	rc.WebRTC.Listen = fmt.Sprintf(":%d", m.cfg.Go2RTC.WebRTCPort)
	if strings.TrimSpace(m.cfg.FFmpegBin) != "" {
		rc.FFmpeg = &ffmpegRuntimeConfig{Bin: m.cfg.FFmpegBin}
	}

	rc.Streams = make(map[string]string)
	for _, cam := range m.cfg.EnabledCameras() {
		rc.Streams[cam.ID+"_main"] = prepareGo2RTCStream(cam.MainRTSP)
		if strings.TrimSpace(cam.SubRTSP) != "" {
			rc.Streams[cam.ID+"_sub"] = prepareGo2RTCStream(cam.SubRTSP)
		}
	}

	return yaml.Marshal(&rc)
}

// prepareGo2RTCStream appends #backchannel=0 to bare rtsp:// URLs so cheap IP
// cameras don't stall waiting on an unsupported ONVIF audio backchannel DESCRIBE.
func prepareGo2RTCStream(raw string) string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(s), "rtsp://") && !strings.Contains(s, "#") {
		return s + "#backchannel=0"
	}
	return s
}

// WriteConfig writes the generated runtime YAML to disk.
func (m *Manager) WriteConfig() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := m.renderConfigYAMLLocked()
	if err != nil {
		return fmt.Errorf("marshal go2rtc config: %w", err)
	}
	dir := filepath.Dir(m.cfg.Go2RTC.ConfigPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create go2rtc config dir: %w", err)
		}
	}
	if err := os.WriteFile(m.cfg.Go2RTC.ConfigPath, data, 0o600); err != nil {
		return fmt.Errorf("write go2rtc config: %w", err)
	}
	return nil
}

// Start writes the go2rtc config, launches go2rtc, and waits until its API responds.
func (m *Manager) Start(ctx context.Context) error {
	resolvedBin, err := resolveBinPath(m.cfg.Go2RTC.Bin)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.binPath = resolvedBin
	m.mu.Unlock()

	if err := m.WriteConfig(); err != nil {
		return err
	}

	firstExit := make(chan error, 1)
	m.wg.Add(1)
	go m.supervise(ctx, firstExit)

	if err := m.waitReady(ctx, 10*time.Second, firstExit); err != nil {
		return err
	}
	return nil
}

func resolveBinPath(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		configured = config.DefaultGo2RTCBin()
	}
	if st, err := os.Stat(configured); err == nil && !st.IsDir() {
		return configured, nil
	}
	if !filepath.IsAbs(configured) {
		if exe, err := os.Executable(); err == nil {
			cand := filepath.Join(filepath.Dir(exe), configured)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand, nil
			}
		}
	}
	base := strings.ToLower(filepath.Base(configured))
	if base == "go2rtc" || base == "go2rtc.exe" {
		if p, err := exec.LookPath("go2rtc"); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("go2rtc binary not found at %q (and not on PATH)", configured)
}

// Wait blocks until the supervisor goroutine has exited after ctx cancellation.
func (m *Manager) Wait() {
	m.wg.Wait()
}

func (m *Manager) supervise(ctx context.Context, firstExit chan<- error) {
	defer m.wg.Done()

	backoff := time.Second
	firstReported := false
	reportInitial := func(err error) {
		if !firstReported {
			firstReported = true
			firstExit <- err
		}
	}

	for {
		if ctx.Err() != nil {
			return
		}

		startAt := time.Now()
		err := m.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}

		reportInitial(err)
		if time.Since(startAt) > 30*time.Second {
			backoff = time.Second
		}
		m.logger.Printf("[go2rtc] process exited (%v); restarting in %s", err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}

func (m *Manager) runOnce(ctx context.Context) error {
	m.mu.RLock()
	bin := m.binPath
	if bin == "" {
		bin = m.cfg.Go2RTC.Bin
	}
	configPath := m.cfg.Go2RTC.ConfigPath
	m.mu.RUnlock()
	cmd := exec.Command(bin, "-c", configPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("go2rtc stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("go2rtc stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start go2rtc: %w", err)
	}
	if m.job != nil {
		if err := m.job.Assign(cmd.Process.Pid); err != nil {
			m.logger.Printf("[go2rtc] warning: job assign pid %d: %v", cmd.Process.Pid, err)
		}
	}

	var logWG sync.WaitGroup
	logPipe := func(r io.Reader) {
		defer logWG.Done()
		sc := bufio.NewScanner(r)
		buf := make([]byte, 0, 16*1024)
		sc.Buffer(buf, 256*1024)
		for sc.Scan() {
			m.logger.Printf("[go2rtc] %s", sc.Text())
		}
	}
	logWG.Add(2)
	go logPipe(stdout)
	go logPipe(stderr)

	waitCh := make(chan error, 1)
	go func() {
		logWG.Wait()
		waitCh <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-waitCh
		return ctx.Err()
	case err := <-waitCh:
		if err == nil {
			return errors.New("exited with status 0")
		}
		return err
	}
}

func (m *Manager) waitReady(ctx context.Context, timeout time.Duration, firstExit <-chan error) error {
	deadline := time.Now().Add(timeout)
	probeURL := m.APIBaseURL() + "/api"
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case err := <-firstExit:
			return fmt.Errorf("go2rtc exited before becoming ready: %w", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
		if err == nil {
			resp, err := m.client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("go2rtc API %s did not become ready within %s", probeURL, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// APIBaseURL returns the loopback HTTP base URL of go2rtc.
func (m *Manager) APIBaseURL() string {
	m.mu.RLock()
	port := m.cfg.Go2RTC.APIPort
	m.mu.RUnlock()
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// MainMP4URL returns the fragmented MP4 stream URL for a camera's main stream.
func (m *Manager) MainMP4URL(camID string) string {
	return fmt.Sprintf("%s/api/stream.mp4?src=%s", m.APIBaseURL(), url.QueryEscape(camID+"_main"))
}

// SubMP4URL returns the fragmented MP4 stream URL for a camera's sub-stream (or main if sub is unset).
func (m *Manager) SubMP4URL(cam config.CameraConfig) string {
	return fmt.Sprintf("%s/api/stream.mp4?src=%s", m.APIBaseURL(), url.QueryEscape(StreamNameSub(cam)))
}

// MainRTSPURL returns the local relay RTSP URL for a camera's main stream.
func (m *Manager) MainRTSPURL(camID string) string {
	m.mu.RLock()
	port := m.cfg.Go2RTC.RTSPPort
	m.mu.RUnlock()
	return fmt.Sprintf("rtsp://127.0.0.1:%d/%s_main", port, camID)
}

// SubRTSPURL returns the local relay RTSP URL for a camera's sub-stream (or main if sub is unset).
func (m *Manager) SubRTSPURL(camID string) string {
	m.mu.RLock()
	port := m.cfg.Go2RTC.RTSPPort
	stream := camID + "_main"
	for _, cam := range m.cfg.Cameras {
		if cam.ID == camID {
			stream = StreamNameSub(cam)
			break
		}
	}
	m.mu.RUnlock()
	return fmt.Sprintf("rtsp://127.0.0.1:%d/%s", port, stream)
}

// FrameJPEGURL returns the go2rtc single-frame JPEG snapshot URL for a stream name.
func (m *Manager) FrameJPEGURL(streamName string) string {
	return fmt.Sprintf("%s/api/frame.jpeg?src=%s", m.APIBaseURL(), url.QueryEscape(streamName))
}

// UpsertStream registers or updates a live stream in go2rtc via HTTP PUT.
func (m *Manager) UpsertStream(ctx context.Context, name, rawURL string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("stream name cannot be empty")
	}
	src := prepareGo2RTCStream(rawURL)
	if src == "" {
		return errors.New("stream url cannot be empty")
	}

	u := fmt.Sprintf("%s/api/streams?name=%s&src=%s", m.APIBaseURL(), url.QueryEscape(name), url.QueryEscape(src))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("go2rtc upsert %q HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// DeleteStream removes a stream from go2rtc via HTTP DELETE.
func (m *Manager) DeleteStream(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	u := fmt.Sprintf("%s/api/streams?src=%s", m.APIBaseURL(), url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("go2rtc delete %q HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// TestStream temporarily registers rawURL in go2rtc, probes its fMP4 header for codec and resolution,
// grabs a best-effort JPEG snapshot, and cleans up the temporary stream.
func (m *Manager) TestStream(ctx context.Context, rawURL string) (*StreamTestResult, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, errors.New("stream url is required")
	}

	probeName := fmt.Sprintf("_probe_%d_%d", time.Now().UnixNano(), probeSeq.Add(1))
	start := time.Now()

	upCtx, upCancel := context.WithTimeout(ctx, 5*time.Second)
	err := m.UpsertStream(upCtx, probeName, rawURL)
	upCancel()
	if err != nil {
		return &StreamTestResult{
			OK:        false,
			LatencyMS: time.Since(start).Milliseconds(),
			Error:     err.Error(),
		}, nil
	}
	defer func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = m.DeleteStream(delCtx, probeName)
		delCancel()
	}()

	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	defer probeCancel()

	mp4URL := fmt.Sprintf("%s/api/stream.mp4?src=%s", m.APIBaseURL(), url.QueryEscape(probeName))
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, mp4URL, nil)
	if err != nil {
		return &StreamTestResult{OK: false, LatencyMS: time.Since(start).Milliseconds(), Error: err.Error()}, nil
	}

	httpCli := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpCli.Do(req)
	if err != nil {
		return &StreamTestResult{OK: false, LatencyMS: time.Since(start).Milliseconds(), Error: err.Error()}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d from go2rtc stream.mp4", resp.StatusCode)
		}
		return &StreamTestResult{
			OK:        false,
			LatencyMS: time.Since(start).Milliseconds(),
			Error:     msg,
		}, nil
	}

	var ftypRaw, moovRaw []byte
	for i := 0; i < 16 && (len(ftypRaw) == 0 || len(moovRaw) == 0); i++ {
		box, boxErr := fmp4.ReadBox(resp.Body)
		if boxErr != nil {
			return &StreamTestResult{
				OK:        false,
				LatencyMS: time.Since(start).Milliseconds(),
				Error:     boxErr.Error(),
			}, nil
		}
		switch box.Type {
		case "ftyp":
			ftypRaw = box.Raw
		case "moov":
			moovRaw = box.Raw
		}
	}
	if len(ftypRaw) == 0 || len(moovRaw) == 0 {
		return &StreamTestResult{
			OK:        false,
			LatencyMS: time.Since(start).Milliseconds(),
			Error:     "stream initialization header (ftyp/moov) not received",
		}, nil
	}

	info, err := fmp4.ParseInit(ftypRaw, moovRaw)
	if err != nil {
		return &StreamTestResult{
			OK:        false,
			LatencyMS: time.Since(start).Milliseconds(),
			Error:     fmt.Sprintf("parse init header: %v", err),
		}, nil
	}

	res := &StreamTestResult{
		OK:        true,
		LatencyMS: time.Since(start).Milliseconds(),
		Width:     int(info.Width),
		Height:    int(info.Height),
		Codec:     info.Codec,
	}

	// Best-effort snapshot while stream.mp4 producer is still open.
	snapCtx, snapCancel := context.WithTimeout(ctx, 5*time.Second)
	snapReq, snapErr := http.NewRequestWithContext(snapCtx, http.MethodGet, m.FrameJPEGURL(probeName), nil)
	if snapErr == nil {
		if snapResp, doErr := httpCli.Do(snapReq); doErr == nil {
			if snapResp.StatusCode == http.StatusOK {
				if jpgBytes, readErr := io.ReadAll(io.LimitReader(snapResp.Body, 2*1024*1024)); readErr == nil && len(jpgBytes) > 0 {
					res.SnapshotB64 = base64.StdEncoding.EncodeToString(jpgBytes)
				}
			}
			_ = snapResp.Body.Close()
		}
	}
	snapCancel()

	return res, nil
}

// StreamNameSub returns the sub-stream name if configured, otherwise the main stream name.
func StreamNameSub(cam config.CameraConfig) string {
	if strings.TrimSpace(cam.SubRTSP) != "" {
		return cam.ID + "_sub"
	}
	return cam.ID + "_main"
}
