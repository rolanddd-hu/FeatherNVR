package go2rtc

import (
	"bufio"
	"context"
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
	"time"

	"gopkg.in/yaml.v3"

	"feathernvr/internal/config"
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

// Manager supervises the local go2rtc relay process and generates its config.
type Manager struct {
	cfg    *config.Config
	job    *procjob.Manager
	logger *log.Logger
	client *http.Client

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
	data, err := m.RenderConfigYAML()
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
	if _, err := os.Stat(m.cfg.Go2RTC.Bin); err != nil {
		return fmt.Errorf("go2rtc binary not found at %q: %w", m.cfg.Go2RTC.Bin, err)
	}
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
	cmd := exec.Command(m.cfg.Go2RTC.Bin, "-c", m.cfg.Go2RTC.ConfigPath)

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
	return fmt.Sprintf("http://127.0.0.1:%d", m.cfg.Go2RTC.APIPort)
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
	return fmt.Sprintf("rtsp://127.0.0.1:%d/%s_main", m.cfg.Go2RTC.RTSPPort, camID)
}

// SubRTSPURL returns the local relay RTSP URL for a camera's sub-stream (or main if sub is unset).
func (m *Manager) SubRTSPURL(camID string) string {
	stream := camID + "_main"
	for _, cam := range m.cfg.Cameras {
		if cam.ID == camID {
			stream = StreamNameSub(cam)
			break
		}
	}
	return fmt.Sprintf("rtsp://127.0.0.1:%d/%s", m.cfg.Go2RTC.RTSPPort, stream)
}

// FrameJPEGURL returns the go2rtc single-frame JPEG snapshot URL for a stream name.
func (m *Manager) FrameJPEGURL(streamName string) string {
	return fmt.Sprintf("%s/api/frame.jpeg?src=%s", m.APIBaseURL(), url.QueryEscape(streamName))
}

// StreamNameSub returns the sub-stream name if configured, otherwise the main stream name.
func StreamNameSub(cam config.CameraConfig) string {
	if strings.TrimSpace(cam.SubRTSP) != "" {
		return cam.ID + "_sub"
	}
	return cam.ID + "_main"
}
