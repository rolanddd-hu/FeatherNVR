package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var validIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Motion mode constants.
const (
	MotionModeONVIF          = "onvif"
	MotionModePixel          = "pixel"
	MotionModeONVIFOrPixel   = "onvif_or_pixel"
	MotionModeHikvisionISAPI = "hikvision_isapi"
	MotionModeISAPIOrPixel   = "isapi_or_pixel"
	MotionModeNone           = "none"
)

// Config holds the full application configuration loaded from YAML.
type Config struct {
	Server        ServerConfig         `yaml:"server"`
	Go2RTC        Go2RTCConfig         `yaml:"go2rtc"`
	FFmpegBin     string               `yaml:"ffmpeg_bin"`
	Storage       StorageConfig        `yaml:"storage"`
	Recording     RecordingConfig      `yaml:"recording"`
	HikvisionDVRs []HikvisionDVRConfig `yaml:"hikvision_dvrs"`
	Cameras       []CameraConfig       `yaml:"cameras"`
}

// ServerConfig configures the built-in HTTP server and web UI.
type ServerConfig struct {
	Listen   string `yaml:"listen"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Go2RTCConfig configures the managed local go2rtc relay binary.
type Go2RTCConfig struct {
	Bin        string `yaml:"bin"`
	APIPort    int    `yaml:"api_port"`
	RTSPPort   int    `yaml:"rtsp_port"`
	WebRTCPort int    `yaml:"webrtc_port"`
	ConfigPath string `yaml:"config_path"`
}

// StorageConfig configures clip storage, SQLite metadata, and FIFO retention.
type StorageConfig struct {
	Dir              string  `yaml:"dir"`
	DBPath           string  `yaml:"db_path"`
	MaxGB            float64 `yaml:"max_gb"`
	HighWatermarkPct float64 `yaml:"high_watermark_pct"`
	LowWatermarkPct  float64 `yaml:"low_watermark_pct"`
	SweepIntervalSec int     `yaml:"sweep_interval_sec"`
}

// RecordingConfig configures default pre-roll, cooldown, chunking, and buffer limits.
type RecordingConfig struct {
	PreRecordSec  int `yaml:"pre_record_sec"`
	PostRecordSec int `yaml:"post_record_sec"`
	MaxClipSec    int `yaml:"max_clip_sec"`
	MaxBufferMB   int `yaml:"max_buffer_mb"`
}

// HikvisionDVRConfig configures a Hikvision DVR/NVR ISAPI alertStream host.
type HikvisionDVRConfig struct {
	ID       string `yaml:"id"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	UseHTTPS bool   `yaml:"use_https"`
}

// PTZConfig configures optional Pan-Tilt-Zoom control for a camera.
type PTZConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Type         string `yaml:"type"`          // "onvif" or "hikvision_isapi"
	URL          string `yaml:"url"`           // ONVIF PTZ/Device URL or Hikvision base http(s) URL
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	ProfileToken string `yaml:"profile_token,omitempty"`
	Channel      int    `yaml:"channel,omitempty"`
}

// CameraConfig configures a single camera stream and its motion source.
type CameraConfig struct {
	ID            string       `yaml:"id"`
	Name          string       `yaml:"name"`
	Enabled       *bool        `yaml:"enabled"`
	MainRTSP      string       `yaml:"main_rtsp"`
	SubRTSP       string       `yaml:"sub_rtsp"`
	LiveStream    string       `yaml:"live_stream,omitempty"`
	Resolution    string       `yaml:"resolution,omitempty"`
	AspectRatio   string       `yaml:"aspect_ratio,omitempty"`
	PreRecordSec  *int         `yaml:"pre_record_sec"`
	PostRecordSec *int         `yaml:"post_record_sec"`
	MaxClipSec    *int         `yaml:"max_clip_sec"`
	Motion        MotionConfig `yaml:"motion"`
	PTZ           PTZConfig    `yaml:"ptz,omitempty"`
}

// MotionConfig configures motion triggering for a camera.
type MotionConfig struct {
	Mode           string       `yaml:"mode"`
	ONVIFURL       string       `yaml:"onvif_url"`
	Username       string       `yaml:"username"`
	Password       string       `yaml:"password"`
	DVR            string       `yaml:"dvr"`
	Channel        int          `yaml:"channel"`
	FPS            int          `yaml:"fps"`
	Width          int          `yaml:"width"`
	Height         int          `yaml:"height"`
	Threshold      uint8        `yaml:"threshold"`
	MinAreaPercent float64      `yaml:"min_area_percent"`
	MaxAreaPercent float64      `yaml:"max_area_percent"`
	MinFrames      int          `yaml:"min_frames"`
	IgnoreZones    [][4]float64 `yaml:"ignore_zones"`
}

// Load reads, applies defaults to, and validates a YAML configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return Parse(data)
}

// Parse unmarshals YAML bytes, applies defaults, and validates the configuration.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults populates missing configuration values with sensible defaults.
func (c *Config) ApplyDefaults() {
	if strings.TrimSpace(c.Server.Listen) == "" {
		c.Server.Listen = ":8080"
	}

	if strings.TrimSpace(c.Go2RTC.Bin) == "" {
		c.Go2RTC.Bin = DefaultGo2RTCBin()
	}
	if c.Go2RTC.APIPort == 0 {
		c.Go2RTC.APIPort = 1984
	}
	if c.Go2RTC.RTSPPort == 0 {
		c.Go2RTC.RTSPPort = 8554
	}
	if c.Go2RTC.WebRTCPort == 0 {
		c.Go2RTC.WebRTCPort = 8555
	}
	if strings.TrimSpace(c.Go2RTC.ConfigPath) == "" {
		c.Go2RTC.ConfigPath = filepath.FromSlash("data/go2rtc.runtime.yaml")
	}

	if strings.TrimSpace(c.FFmpegBin) == "" {
		c.FFmpegBin = DetectFFmpeg()
	}

	if strings.TrimSpace(c.Storage.Dir) == "" {
		c.Storage.Dir = "recordings"
	}
	if strings.TrimSpace(c.Storage.DBPath) == "" {
		c.Storage.DBPath = filepath.FromSlash("data/feathernvr.db")
	}
	if c.Storage.MaxGB == 0 {
		c.Storage.MaxGB = 100.0
	}
	if c.Storage.HighWatermarkPct == 0 {
		c.Storage.HighWatermarkPct = 98.0
	}
	if c.Storage.LowWatermarkPct == 0 {
		c.Storage.LowWatermarkPct = 92.0
	}
	if c.Storage.SweepIntervalSec == 0 {
		c.Storage.SweepIntervalSec = 300
	}

	if c.Recording.PreRecordSec == 0 {
		c.Recording.PreRecordSec = 5
	}
	if c.Recording.PostRecordSec == 0 {
		c.Recording.PostRecordSec = 10
	}
	if c.Recording.MaxClipSec == 0 {
		c.Recording.MaxClipSec = 300
	}
	if c.Recording.MaxBufferMB == 0 {
		c.Recording.MaxBufferMB = 24
	}

	for i := range c.HikvisionDVRs {
		if c.HikvisionDVRs[i].Port == 0 {
			if c.HikvisionDVRs[i].UseHTTPS {
				c.HikvisionDVRs[i].Port = 443
			} else {
				c.HikvisionDVRs[i].Port = 80
			}
		}
	}

	for i := range c.Cameras {
		cam := &c.Cameras[i]
		if cam.Enabled == nil {
			enabled := true
			cam.Enabled = &enabled
		}
		if strings.TrimSpace(cam.Name) == "" {
			cam.Name = cam.ID
		}
		cam.LiveStream = strings.ToLower(strings.TrimSpace(cam.LiveStream))
		cam.Resolution = strings.TrimSpace(cam.Resolution)
		cam.AspectRatio = strings.ToLower(strings.TrimSpace(cam.AspectRatio))

		m := &cam.Motion
		m.Mode = strings.ToLower(strings.TrimSpace(m.Mode))
		if m.Mode == "" {
			switch {
			case strings.TrimSpace(m.ONVIFURL) != "":
				m.Mode = MotionModeONVIFOrPixel
			case strings.TrimSpace(m.DVR) != "":
				m.Mode = MotionModeHikvisionISAPI
			default:
				m.Mode = MotionModePixel
			}
		}
		if m.FPS == 0 {
			m.FPS = 2
		}
		if m.Width == 0 {
			m.Width = 160
		}
		if m.Height == 0 {
			m.Height = 90
		}
		if m.Threshold == 0 {
			m.Threshold = 18
		}
		if m.MinAreaPercent == 0 {
			m.MinAreaPercent = 0.35
		}
		if m.MaxAreaPercent == 0 {
			m.MaxAreaPercent = 80.0
		}
		if m.MinFrames == 0 {
			m.MinFrames = 2
		}

		if cam.PTZ.Enabled {
			cam.PTZ.Type = strings.ToLower(strings.TrimSpace(cam.PTZ.Type))
			if cam.PTZ.Type == "hikvision_isapi" && cam.PTZ.Channel <= 0 {
				cam.PTZ.Channel = 1
			}
		}
	}
}

// Validate checks configuration invariants and returns descriptive errors.
func (c *Config) Validate() error {
	if c.Go2RTC.APIPort <= 0 || c.Go2RTC.APIPort > 65535 {
		return fmt.Errorf("go2rtc.api_port must be 1..65535, got %d", c.Go2RTC.APIPort)
	}
	if c.Go2RTC.RTSPPort <= 0 || c.Go2RTC.RTSPPort > 65535 {
		return fmt.Errorf("go2rtc.rtsp_port must be 1..65535, got %d", c.Go2RTC.RTSPPort)
	}
	if c.Go2RTC.WebRTCPort <= 0 || c.Go2RTC.WebRTCPort > 65535 {
		return fmt.Errorf("go2rtc.webrtc_port must be 1..65535, got %d", c.Go2RTC.WebRTCPort)
	}

	if c.Storage.MaxGB <= 0 {
		return fmt.Errorf("storage.max_gb must be > 0, got %v", c.Storage.MaxGB)
	}
	if c.Storage.LowWatermarkPct <= 0 || c.Storage.LowWatermarkPct > c.Storage.HighWatermarkPct || c.Storage.HighWatermarkPct > 100 {
		return fmt.Errorf("storage watermarks invalid: low=%v high=%v", c.Storage.LowWatermarkPct, c.Storage.HighWatermarkPct)
	}
	if c.Storage.SweepIntervalSec < 1 {
		return fmt.Errorf("storage.sweep_interval_sec must be >= 1")
	}

	if c.Recording.PreRecordSec < 0 {
		return errors.New("recording.pre_record_sec must be >= 0")
	}
	if c.Recording.PostRecordSec < 1 {
		return errors.New("recording.post_record_sec must be >= 1")
	}
	if c.Recording.MaxClipSec < 1 {
		return errors.New("recording.max_clip_sec must be >= 1")
	}
	if c.Recording.MaxBufferMB < 1 {
		return errors.New("recording.max_buffer_mb must be >= 1")
	}

	dvrs := make(map[string]HikvisionDVRConfig, len(c.HikvisionDVRs))
	for _, dvr := range c.HikvisionDVRs {
		if !validIDRe.MatchString(dvr.ID) {
			return fmt.Errorf("invalid hikvision_dvr id %q (allowed: [a-zA-Z0-9_-])", dvr.ID)
		}
		if _, dup := dvrs[dvr.ID]; dup {
			return fmt.Errorf("duplicate hikvision_dvr id %q", dvr.ID)
		}
		if strings.TrimSpace(dvr.Host) == "" {
			return fmt.Errorf("hikvision_dvr %q host cannot be empty", dvr.ID)
		}
		dvrs[dvr.ID] = dvr
	}

	seenCams := make(map[string]struct{}, len(c.Cameras))
	for _, cam := range c.Cameras {
		if !validIDRe.MatchString(cam.ID) {
			return fmt.Errorf("invalid camera id %q (allowed: [a-zA-Z0-9_-])", cam.ID)
		}
		if _, dup := seenCams[cam.ID]; dup {
			return fmt.Errorf("duplicate camera id %q", cam.ID)
		}
		seenCams[cam.ID] = struct{}{}

		if !cam.IsEnabled() {
			continue
		}

		if strings.TrimSpace(cam.MainRTSP) == "" {
			return fmt.Errorf("camera %q main_rtsp cannot be empty", cam.ID)
		}

		if cam.PreRecordSec != nil && *cam.PreRecordSec < 0 {
			return fmt.Errorf("camera %q pre_record_sec must be >= 0", cam.ID)
		}
		if cam.PostRecordSec != nil && *cam.PostRecordSec < 1 {
			return fmt.Errorf("camera %q post_record_sec must be >= 1", cam.ID)
		}
		if cam.MaxClipSec != nil && *cam.MaxClipSec < 1 {
			return fmt.Errorf("camera %q max_clip_sec must be >= 1", cam.ID)
		}

		switch cam.Motion.Mode {
		case MotionModeONVIF, MotionModeONVIFOrPixel:
			if strings.TrimSpace(cam.Motion.ONVIFURL) == "" {
				return fmt.Errorf("camera %q motion.mode=%s requires motion.onvif_url", cam.ID, cam.Motion.Mode)
			}
		case MotionModeHikvisionISAPI, MotionModeISAPIOrPixel:
			if _, ok := dvrs[cam.Motion.DVR]; !ok {
				return fmt.Errorf("camera %q motion.dvr %q not found in hikvision_dvrs", cam.ID, cam.Motion.DVR)
			}
			if cam.Motion.Channel <= 0 {
				return fmt.Errorf("camera %q motion.channel must be > 0 for %s", cam.ID, cam.Motion.Mode)
			}
		case MotionModePixel, MotionModeNone:
		default:
			return fmt.Errorf("camera %q invalid motion.mode %q", cam.ID, cam.Motion.Mode)
		}

		if cam.Motion.FPS < 1 || cam.Motion.Width < 16 || cam.Motion.Height < 16 {
			return fmt.Errorf("camera %q invalid pixel motion dimensions/fps", cam.ID)
		}
		if cam.Motion.MinAreaPercent <= 0 || cam.Motion.MinAreaPercent >= cam.Motion.MaxAreaPercent || cam.Motion.MaxAreaPercent > 100 {
			return fmt.Errorf("camera %q invalid motion area percentages: min=%v max=%v", cam.ID, cam.Motion.MinAreaPercent, cam.Motion.MaxAreaPercent)
		}
		for idx, z := range cam.Motion.IgnoreZones {
			if z[0] < 0 || z[1] < 0 || z[2] > 1 || z[3] > 1 || z[0] >= z[2] || z[1] >= z[3] {
				return fmt.Errorf("camera %q ignore_zones[%d] invalid normalized rect %v", cam.ID, idx, z)
			}
		}

		if cam.PTZ.Enabled {
			switch cam.PTZ.Type {
			case "onvif", "hikvision_isapi":
			default:
				return fmt.Errorf("camera %q invalid ptz.type %q (expected onvif or hikvision_isapi)", cam.ID, cam.PTZ.Type)
			}
			if strings.TrimSpace(cam.PTZ.URL) == "" {
				return fmt.Errorf("camera %q ptz.url cannot be empty when ptz.enabled is true", cam.ID)
			}
		}

		switch cam.LiveStream {
		case "", "auto", "sub", "main":
		default:
			return fmt.Errorf("camera %q invalid live_stream %q (expected auto, sub, or main)", cam.ID, cam.LiveStream)
		}
		switch cam.AspectRatio {
		case "", "auto", "16:9", "4:3", "fill":
		default:
			return fmt.Errorf("camera %q invalid aspect_ratio %q (expected auto, 16:9, 4:3, or fill)", cam.ID, cam.AspectRatio)
		}
	}

	return nil
}

// FindDVR returns the configured Hikvision DVR by ID.
func (c *Config) FindDVR(id string) (HikvisionDVRConfig, bool) {
	for _, dvr := range c.HikvisionDVRs {
		if dvr.ID == id {
			return dvr, true
		}
	}
	return HikvisionDVRConfig{}, false
}

// EnabledCameras returns only enabled cameras in declaration order.
func (c *Config) EnabledCameras() []CameraConfig {
	out := make([]CameraConfig, 0, len(c.Cameras))
	for _, cam := range c.Cameras {
		if cam.IsEnabled() {
			out = append(out, cam)
		}
	}
	return out
}

// IsEnabled reports whether the camera is enabled.
func (c CameraConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// EffectiveSubRTSP returns SubRTSP when set, otherwise falls back to MainRTSP.
func (c CameraConfig) EffectiveSubRTSP() string {
	if strings.TrimSpace(c.SubRTSP) != "" {
		return c.SubRTSP
	}
	return c.MainRTSP
}

// EffectiveLiveStream returns "main" when configured for main stream or when no sub_rtsp is set, else "sub".
func (c CameraConfig) EffectiveLiveStream() string {
	if strings.EqualFold(strings.TrimSpace(c.LiveStream), "main") || strings.TrimSpace(c.SubRTSP) == "" {
		return "main"
	}
	return "sub"
}

// EffectivePreRecordSec returns the camera's pre-record seconds or the global default.
func (c CameraConfig) EffectivePreRecordSec(def RecordingConfig) int {
	if c.PreRecordSec != nil {
		return *c.PreRecordSec
	}
	return def.PreRecordSec
}

// EffectivePostRecordSec returns the camera's post-record seconds or the global default.
func (c CameraConfig) EffectivePostRecordSec(def RecordingConfig) int {
	if c.PostRecordSec != nil {
		return *c.PostRecordSec
	}
	return def.PostRecordSec
}

// EffectiveMaxClipSec returns the camera's max clip seconds or the global default.
func (c CameraConfig) EffectiveMaxClipSec(def RecordingConfig) int {
	if c.MaxClipSec != nil {
		return *c.MaxClipSec
	}
	return def.MaxClipSec
}

// DefaultGo2RTCBin returns the default bundled go2rtc executable path for the current OS.
func DefaultGo2RTCBin() string {
	if runtime.GOOS == "windows" {
		return filepath.FromSlash("bin/go2rtc.exe")
	}
	return filepath.FromSlash("bin/go2rtc")
}

// DetectFFmpeg locates ffmpeg in bundled bin/, on PATH, or in standard OS install directories.
func DetectFFmpeg() string {
	exeName := "ffmpeg"
	if runtime.GOOS == "windows" {
		exeName = "ffmpeg.exe"
	}

	localCandidates := []string{filepath.Join("bin", exeName)}
	if exe, err := os.Executable(); err == nil {
		localCandidates = append(localCandidates, filepath.Join(filepath.Dir(exe), "bin", exeName))
	}
	for _, c := range localCandidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}

	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}

	var sysCandidates []string
	if runtime.GOOS == "windows" {
		sysCandidates = []string{
			`C:\Program Files (x86)\ffmpeg\bin\ffmpeg.exe`,
			`C:\Program Files\ffmpeg\bin\ffmpeg.exe`,
			`C:\ffmpeg\bin\ffmpeg.exe`,
		}
	} else {
		sysCandidates = []string{
			"/opt/homebrew/bin/ffmpeg",
			"/usr/local/bin/ffmpeg",
			"/usr/bin/ffmpeg",
		}
	}
	for _, c := range sysCandidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// EnsureDefaultConfig writes a starter config.yaml at path if it does not already exist.
func EnsureDefaultConfig(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat config %q: %w", path, err)
	}

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("create config directory %q: %w", dir, err)
		}
	}

	starter := fmt.Sprintf(`# FeatherNVR Configuration (auto-generated on first run)
# Add cameras anytime from the Web UI (+ Add Camera) or by editing this file.

server:
  listen: ":8080"

go2rtc:
  bin: "%s"
  api_port: 1984
  rtsp_port: 8554
  webrtc_port: 8555
  config_path: "data/go2rtc.runtime.yaml"

storage:
  dir: "recordings"
  db_path: "data/feathernvr.db"
  max_gb: 100
  high_watermark_pct: 98
  low_watermark_pct: 92
  sweep_interval_sec: 300

recording:
  pre_record_sec: 5
  post_record_sec: 10
  max_clip_sec: 300
  max_buffer_mb: 24

cameras: []
`, filepath.ToSlash(DefaultGo2RTCBin()))

	if err := os.WriteFile(path, []byte(starter), 0o600); err != nil {
		return false, fmt.Errorf("write starter config %q: %w", path, err)
	}
	return true, nil
}

// SaveStorageCap updates storage.max_gb inside configPath while preserving
// existing comments, key ordering, and camera configuration.
func SaveStorageCap(configPath string, maxGB float64) error {
	if maxGB <= 0 {
		return fmt.Errorf("storage.max_gb must be > 0, got %v", maxGB)
	}
	return updateConfigYAML(configPath, func(root *yaml.Node) error {
		storageNode := findOrAddMappingKey(root, "storage", yaml.MappingNode)
		if storageNode.Kind != yaml.MappingNode {
			storageNode.Kind = yaml.MappingNode
			storageNode.Content = nil
		}

		valStr := strconv.FormatFloat(maxGB, 'f', -1, 64)
		tag := "!!int"
		if strings.ContainsRune(valStr, '.') {
			tag = "!!float"
		}
		maxNode := findOrAddMappingKey(storageNode, "max_gb", yaml.ScalarNode)
		maxNode.Kind = yaml.ScalarNode
		maxNode.Tag = tag
		maxNode.Style = 0
		maxNode.Value = valStr
		return nil
	})
}

// SaveCameras replaces the top-level cameras sequence inside configPath while preserving
// existing comments, key ordering, and non-camera sections.
func SaveCameras(configPath string, cameras []CameraConfig) error {
	if cameras == nil {
		cameras = []CameraConfig{}
	}
	return updateConfigYAML(configPath, func(root *yaml.Node) error {
		var camDoc yaml.Node
		if err := camDoc.Encode(cameras); err != nil {
			return fmt.Errorf("encode cameras yaml: %w", err)
		}
		camNode := &camDoc
		if camDoc.Kind == yaml.DocumentNode && len(camDoc.Content) > 0 {
			camNode = camDoc.Content[0]
		}
		target := findOrAddMappingKey(root, "cameras", yaml.SequenceNode)
		*target = *camNode
		return nil
	})
}

func updateConfigYAML(configPath string, fn func(root *yaml.Node) error) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", configPath, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config %q: %w", configPath, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc = yaml.Node{
			Kind:    yaml.DocumentNode,
			Content: []*yaml.Node{{Kind: yaml.MappingNode}},
		}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config root must be a YAML mapping")
	}

	if err := fn(root); err != nil {
		return err
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encode config yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close yaml encoder: %w", err)
	}

	if err := os.WriteFile(configPath, out.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write config %q: %w", configPath, err)
	}
	return nil
}

func findOrAddMappingKey(mapping *yaml.Node, key string, defaultValKind yaml.Kind) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	kNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	vNode := &yaml.Node{Kind: defaultValKind}
	mapping.Content = append(mapping.Content, kNode, vNode)
	return vNode
}
