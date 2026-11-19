package motion

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"feathernvr/internal/config"
	"feathernvr/internal/procjob"
)

// PixelDiffEngine performs lightweight grayscale 3x3 box-blurred EMA background
// subtraction with normalized ignore zones and frame hysteresis.
type PixelDiffEngine struct {
	width        int
	height       int
	threshold    uint8
	minAreaPct   float64
	maxAreaPct   float64
	minFrames    int
	mask         []bool
	activePixels int
	bg           []uint16 // Q4 fixed-point (pixel * 16)
	blurred      []uint8
	initialized  bool
	streak       int
}

// NewPixelDiffEngine creates a pixel motion analyzer for fixed-size grayscale frames.
func NewPixelDiffEngine(cfg config.MotionConfig) *PixelDiffEngine {
	w, h := cfg.Width, cfg.Height
	if w <= 0 {
		w = 160
	}
	if h <= 0 {
		h = 90
	}
	thresh := cfg.Threshold
	if thresh == 0 {
		thresh = 18
	}
	minArea := cfg.MinAreaPercent
	if minArea <= 0 {
		minArea = 0.35
	}
	maxArea := cfg.MaxAreaPercent
	if maxArea <= 0 {
		maxArea = 80.0
	}
	minFrames := cfg.MinFrames
	if minFrames <= 0 {
		minFrames = 2
	}

	n := w * h
	mask := make([]bool, n)
	for _, z := range cfg.IgnoreZones {
		x1 := int(z[0] * float64(w))
		y1 := int(z[1] * float64(h))
		x2 := int(z[2] * float64(w))
		y2 := int(z[3] * float64(h))
		if x1 < 0 {
			x1 = 0
		}
		if y1 < 0 {
			y1 = 0
		}
		if x2 > w {
			x2 = w
		}
		if y2 > h {
			y2 = h
		}
		for y := y1; y < y2; y++ {
			row := y * w
			for x := x1; x < x2; x++ {
				mask[row+x] = true
			}
		}
	}

	activePixels := 0
	for _, ignored := range mask {
		if !ignored {
			activePixels++
		}
	}

	return &PixelDiffEngine{
		width:        w,
		height:       h,
		threshold:    thresh,
		minAreaPct:   minArea,
		maxAreaPct:   maxArea,
		minFrames:    minFrames,
		mask:         mask,
		activePixels: activePixels,
		bg:           make([]uint16, n),
		blurred:      make([]uint8, n),
	}
}

// ProcessFrame analyzes one w*h grayscale frame and returns whether motion is
// triggered and the percentage of unmasked pixels that changed.
func (e *PixelDiffEngine) ProcessFrame(gray []byte) (bool, float64) {
	n := e.width * e.height
	if len(gray) != n || e.activePixels == 0 {
		return false, 0
	}

	e.boxBlur3x3(gray)

	if !e.initialized {
		for i := 0; i < n; i++ {
			e.bg[i] = uint16(e.blurred[i]) << 4
		}
		e.initialized = true
		return false, 0
	}

	thresh := int(e.threshold)
	changed := 0

	for i := 0; i < n; i++ {
		cur := int(e.blurred[i])
		bgVal := int(e.bg[i] >> 4)
		diff := cur - bgVal
		if diff < 0 {
			diff = -diff
		}
		if !e.mask[i] && diff >= thresh {
			changed++
		}
		// Alpha = 1/8 exponential moving average in Q4 fixed point.
		e.bg[i] = uint16((7*int(e.bg[i]) + (cur << 4) + 4) >> 3)
	}

	pct := (float64(changed) * 100.0) / float64(e.activePixels)
	if pct >= e.minAreaPct && pct <= e.maxAreaPct {
		e.streak++
	} else {
		e.streak = 0
	}

	return e.streak >= e.minFrames, pct
}

// Reset clears the learned background model and streak state (e.g., on stream reconnect).
func (e *PixelDiffEngine) Reset() {
	e.initialized = false
	e.streak = 0
}

func (e *PixelDiffEngine) boxBlur3x3(src []byte) {
	w, h := e.width, e.height
	dst := e.blurred

	// Copy outer 1px border directly, blur interior with 3x3 box filter.
	copy(dst[:w], src[:w])
	copy(dst[(h-1)*w:], src[(h-1)*w:])
	for y := 1; y < h-1; y++ {
		row := y * w
		prev := row - w
		next := row + w
		dst[row] = src[row]
		dst[row+w-1] = src[row+w-1]
		for x := 1; x < w-1; x++ {
			sum := int(src[prev+x-1]) + int(src[prev+x]) + int(src[prev+x+1]) +
				int(src[row+x-1]) + int(src[row+x]) + int(src[row+x+1]) +
				int(src[next+x-1]) + int(src[next+x]) + int(src[next+x+1])
			dst[row+x] = uint8((sum + 4) / 9)
		}
	}
}

// PixelDetector decodes a low-fps grayscale sub-stream via ffmpeg and detects motion.
type PixelDetector struct {
	camID     string
	rtspURL   string
	ffmpegBin string
	cfg       config.MotionConfig
	job       *procjob.Manager
	logger    *log.Logger
	tracker   *StatsTracker
}

// NewPixelDetector creates a sub-stream pixel-diff motion detector.
func NewPixelDetector(camID, rtspURL, ffmpegBin string, cfg config.MotionConfig, job *procjob.Manager, logger *log.Logger) *PixelDetector {
	if logger == nil {
		logger = log.Default()
	}
	if cfg.Width <= 0 {
		cfg.Width = 160
	}
	if cfg.Height <= 0 {
		cfg.Height = 90
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 2
	}
	return &PixelDetector{
		camID:     camID,
		rtspURL:   rtspURL,
		ffmpegBin: ffmpegBin,
		cfg:       cfg,
		job:       job,
		logger:    logger,
		tracker:   NewStatsTracker(config.MotionModePixel),
	}
}

// Stats returns the current detector status.
func (d *PixelDetector) Stats() Stats {
	return d.tracker.Snapshot()
}

// Run decodes frames from subRTSP via ffmpeg and emits motion events until ctx is canceled.
func (d *PixelDetector) Run(ctx context.Context, emit func(Event)) {
	if d.ffmpegBin == "" {
		d.tracker.SetConnected(false, "ffmpeg binary not found for pixel motion detection")
		<-ctx.Done()
		return
	}

	engine := NewPixelDiffEngine(d.cfg)
	backoff := 2 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		engine.Reset()
		start := time.Now()
		err := d.runSession(ctx, engine, emit)
		if ctx.Err() != nil {
			d.tracker.SetConnected(false, "")
			return
		}

		d.tracker.SetConnected(false, err.Error())
		if time.Since(start) > 30*time.Second {
			backoff = 2 * time.Second
		}
		d.logger.Printf("[motion][%s] pixel stream ended (%v); retrying in %s", d.camID, err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (d *PixelDetector) runSession(ctx context.Context, engine *PixelDiffEngine, emit func(Event)) error {
	vf := fmt.Sprintf("fps=%d,scale=%d:%d,format=gray", d.cfg.FPS, d.cfg.Width, d.cfg.Height)
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-threads", "1",
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.rtspURL)), "rtsp://") {
		args = append(args, "-rtsp_transport", "tcp")
	}
	args = append(args,
		"-i", d.rtspURL,
		"-vf", vf,
		"-f", "rawvideo",
		"-",
	)

	cmd := exec.Command(d.ffmpegBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	if d.job != nil {
		_ = d.job.Assign(cmd.Process.Pid)
	}

	var errTextMu sync.Mutex
	var lastErrLine string
	go func() {
		buf, _ := io.ReadAll(io.LimitReader(stderr, 4096))
		if len(buf) > 0 {
			errTextMu.Lock()
			lastErrLine = string(buf)
			errTextMu.Unlock()
		}
	}()

	// Watchdog kills a stalled ffmpeg process if no frame arrives within 15s or ctx is canceled.
	var watchdogMu sync.Mutex
	stopped := false
	stallTimeout := 15 * time.Second
	watchdog := time.AfterFunc(stallTimeout, func() {
		watchdogMu.Lock()
		defer watchdogMu.Unlock()
		if !stopped && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer func() {
		watchdogMu.Lock()
		stopped = true
		watchdog.Stop()
		watchdogMu.Unlock()
	}()

	stopCancel := context.AfterFunc(ctx, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer stopCancel()

	frameSize := d.cfg.Width * d.cfg.Height
	frame := make([]byte, frameSize)
	wasActive := false
	lastEmit := time.Time{}

	for {
		if _, err := io.ReadFull(stdout, frame); err != nil {
			_ = cmd.Wait()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errTextMu.Lock()
			detail := lastErrLine
			errTextMu.Unlock()
			if detail != "" {
				return fmt.Errorf("ffmpeg read: %w (%s)", err, detail)
			}
			return fmt.Errorf("ffmpeg read: %w", err)
		}

		watchdogMu.Lock()
		if !stopped {
			watchdog.Reset(stallTimeout)
		}
		watchdogMu.Unlock()

		d.tracker.SetConnected(true, "")
		active, pct := engine.ProcessFrame(frame)
		d.tracker.SetScore(pct)
		now := time.Now()

		if active != wasActive || (active && now.Sub(lastEmit) >= time.Second) {
			ev := Event{
				CameraID: d.camID,
				Active:   active,
				Source:   config.MotionModePixel,
				Score:    pct,
				Time:     now,
			}
			d.tracker.RecordEvent(ev)
			emit(ev)
			wasActive = active
			lastEmit = now
		}
	}
}
