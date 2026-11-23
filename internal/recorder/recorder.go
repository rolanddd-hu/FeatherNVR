package recorder

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"feathernvr/internal/config"
	"feathernvr/internal/fmp4"
	"feathernvr/internal/motion"
	"feathernvr/internal/procjob"
	"feathernvr/internal/storage"
)

// CameraStatus is the sanitized live status of a single camera exposed via /api/status.
type CameraStatus struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Online       bool      `json:"online"`
	Recording    bool      `json:"recording"`
	StreamSub    string    `json:"stream_sub"`
	StreamMain   string    `json:"stream_main"`
	BitrateKbps  float64   `json:"bitrate_kbps"`
	FPS          float64   `json:"fps"`
	Width        int       `json:"width,omitempty"`
	Height       int       `json:"height,omitempty"`
	Codec        string    `json:"codec,omitempty"`
	LiveStream   string    `json:"live_stream"`
	AspectRatio  string    `json:"aspect_ratio,omitempty"`
	Resolution   string    `json:"resolution,omitempty"`
	ConfigRev    uint64    `json:"config_rev"`
	MotionMode   string    `json:"motion_mode"`
	MotionActive bool      `json:"motion_active"`
	MotionScore  float64   `json:"motion_score"`
	LastMotion   time.Time `json:"last_motion,omitempty"`
	PTZEnabled   bool      `json:"ptz_enabled"`
	Reconnects   int64     `json:"reconnects"`
	LastError    string    `json:"last_error,omitempty"`
}

// CameraRecorder ingests one camera's fMP4 stream from go2rtc into an in-memory
// GOP ring buffer and writes zero-transcode motion clips directly to disk.
type CameraRecorder struct {
	cam       config.CameraConfig
	defaults  config.RecordingConfig
	storage   config.StorageConfig
	mp4URL    string
	frameURL  string
	subStream string
	db        *storage.DB
	retention *storage.RetentionManager
	detector  motion.Detector
	ffmpegBin string
	job       *procjob.Manager
	logger    *log.Logger
	streamCli *http.Client
	thumbCli  *http.Client

	ring *fmp4.GOPRingBuffer

	mu                  sync.Mutex
	online              bool
	lastError           string
	reconnects          int64
	bitrateKbps         float64
	fps                 float64
	initInfo            *fmp4.InitInfo
	motionActive        bool
	lastMotionEvent     time.Time
	recording           bool
	awaitingKeyframe    bool
	wroteFragments      int
	wroteAudioFragments int
	clipHasAudioTrack   bool
	clipAudioSampleType string
	clipAudioTimescale  uint32
	clipStart           time.Time
	firstFragAt         time.Time
	lastFragAt          time.Time
	clipFile            *os.File
	clipBuf             *bufio.Writer
	clipRelPath         string
	clipAbsPath         string
	clipThumbRel        string
	clipSource          string
	rebaser             *fmp4.ClipRebaser
	configRev           uint64
	cooldownGen         uint64
	cooldownTimer       *time.Timer
	finalizeWG          sync.WaitGroup
}

// audioRemuxSem caps concurrent post-clip FFmpeg audio remux jobs to avoid CPU/disk spikes.
var audioRemuxSem = make(chan struct{}, 2)

type clipFinalizeJob struct {
	absPath         string
	relPath         string
	thumbRel        string
	start           time.Time
	end             time.Time
	source          string
	hasAudioTrack   bool
	audioSampleType string
	audioTimescale  uint32
	wroteAudioFrags int
	ffmpegBin       string
	job             *procjob.Manager
}

type limitedWriter struct {
	buf bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.max > 0 && w.buf.Len() < w.max {
		rem := w.max - w.buf.Len()
		if rem > len(p) {
			rem = len(p)
		}
		_, _ = w.buf.Write(p[:rem])
	}
	return len(p), nil
}

func needsAudioRemux(hasAudioTrack bool, sampleType string) bool {
	if !hasAudioTrack {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(sampleType), "mp4a")
}

// NewCameraRecorder creates a recorder for one enabled camera.
func NewCameraRecorder(
	cam config.CameraConfig,
	defaults config.RecordingConfig,
	stor config.StorageConfig,
	mp4URL string,
	frameURL string,
	subStream string,
	db *storage.DB,
	retention *storage.RetentionManager,
	detector motion.Detector,
	logger *log.Logger,
) *CameraRecorder {
	if logger == nil {
		logger = log.Default()
	}
	preSec := cam.EffectivePreRecordSec(defaults)
	maxBufBytes := int64(defaults.MaxBufferMB) * 1024 * 1024
	if maxBufBytes <= 0 {
		maxBufBytes = 24 * 1024 * 1024
	}
	return &CameraRecorder{
		cam:       cam,
		defaults:  defaults,
		storage:   stor,
		mp4URL:    mp4URL,
		frameURL:  frameURL,
		subStream: subStream,
		db:        db,
		retention: retention,
		detector:  detector,
		logger:    logger,
		streamCli: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 25 * time.Second,
			},
		},
		thumbCli: &http.Client{
			Timeout: 5 * time.Second,
		},
		ring: fmp4.NewGOPRingBuffer(time.Duration(preSec)*time.Second, maxBufBytes),
	}
}

// SetDetector attaches a motion detector after construction.
func (r *CameraRecorder) SetDetector(d motion.Detector) {
	r.mu.Lock()
	r.detector = d
	r.mu.Unlock()
}

// SetFFmpeg configures a local ffmpeg binary for thumbnail fallback when go2rtc frame.jpeg is unavailable.
func (r *CameraRecorder) SetFFmpeg(bin string, job *procjob.Manager) {
	r.mu.Lock()
	r.ffmpegBin = bin
	r.job = job
	r.mu.Unlock()
}

// Run streams fMP4 packets continuously from go2rtc until ctx is canceled.
func (r *CameraRecorder) Run(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			r.finalizeOnShutdown()
			return
		}

		start := time.Now()
		err := r.streamOnce(ctx)
		if ctx.Err() != nil {
			r.finalizeOnShutdown()
			return
		}

		r.mu.Lock()
		r.online = false
		r.bitrateKbps = 0
		r.fps = 0
		r.reconnects++
		r.lastError = err.Error()
		r.stopRecordingLocked(false)
		r.ring.Reset()
		r.mu.Unlock()

		if time.Since(start) > 30*time.Second {
			backoff = 2 * time.Second
		}
		r.logger.Printf("[recorder][%s] stream disconnected (%v); reconnecting in %s", r.cam.ID, err, backoff)

		select {
		case <-ctx.Done():
			r.finalizeOnShutdown()
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (r *CameraRecorder) finalizeOnShutdown() {
	r.mu.Lock()
	r.online = false
	r.stopRecordingLocked(false)
	r.mu.Unlock()
	r.finalizeWG.Wait()
}

func (r *CameraRecorder) streamOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.mp4URL, nil)
	if err != nil {
		return err
	}
	resp, err := r.streamCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, r.mp4URL)
	}

	// Stall watchdog closes resp.Body if no box arrives for 25s.
	var wdMu sync.Mutex
	wdStopped := false
	stallTimeout := 25 * time.Second
	watchdog := time.AfterFunc(stallTimeout, func() {
		wdMu.Lock()
		defer wdMu.Unlock()
		if !wdStopped {
			_ = resp.Body.Close()
		}
	})
	defer func() {
		wdMu.Lock()
		wdStopped = true
		watchdog.Stop()
		wdMu.Unlock()
	}()

	stopCancel := context.AfterFunc(ctx, func() {
		_ = resp.Body.Close()
	})
	defer stopCancel()

	br := bufio.NewReaderSize(resp.Body, 64*1024)
	var ftypRaw []byte
	var curInit *fmp4.InitInfo
	var pendingMoof []byte

	winStart := time.Now()
	var winBytes int64
	var winVideoFrames int

	for {
		box, err := fmp4.ReadBox(br)
		if err != nil {
			return err
		}

		wdMu.Lock()
		if !wdStopped {
			watchdog.Reset(stallTimeout)
		}
		wdMu.Unlock()

		switch box.Type {
		case "ftyp":
			ftypRaw = box.Raw
		case "moov":
			if len(ftypRaw) == 0 {
				return errors.New("received moov before ftyp")
			}
			initInfo, err := fmp4.ParseInit(ftypRaw, box.Raw)
			if err != nil {
				return err
			}
			curInit = initInfo
			r.mu.Lock()
			if r.recording {
				r.stopRecordingLocked(false)
			}
			r.initInfo = curInit
			r.online = true
			r.lastError = ""
			r.ring.Reset()
			r.mu.Unlock()
			winStart = time.Now()
			winBytes = 0
			winVideoFrames = 0
		case "moof":
			pendingMoof = box.Raw
		case "mdat":
			if curInit == nil || len(pendingMoof) == 0 {
				pendingMoof = nil
				continue
			}
			frag, err := fmp4.ParseMoof(pendingMoof, curInit)
			pendingMoof = nil
			if err != nil {
				return err
			}
			now := time.Now()
			frag.Mdat = box.Raw
			frag.ReceivedAt = now

			winBytes += frag.ByteSize()
			if frag.IsVideo {
				winVideoFrames++
			}
			elapsed := now.Sub(winStart)
			var kbps, fps float64
			updateRates := elapsed >= 2*time.Second
			if updateRates {
				secs := elapsed.Seconds()
				kbps = (float64(winBytes) * 8.0) / secs / 1000.0
				fps = float64(winVideoFrames) / secs
				winStart = now
				winBytes = 0
				winVideoFrames = 0
			}

			r.handleFragment(frag, updateRates, kbps, fps)
		}
	}
}

func (r *CameraRecorder) handleFragment(frag fmp4.Fragment, updateRates bool, kbps, fps float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if updateRates {
		r.bitrateKbps = kbps
		r.fps = fps
	}

	r.ring.Push(frag)
	if !r.recording {
		return
	}

	maxClip := time.Duration(r.cam.EffectiveMaxClipSec(r.defaults)) * time.Second
	if frag.IsKeyframe && !r.awaitingKeyframe && !r.firstFragAt.IsZero() && frag.ReceivedAt.Sub(r.firstFragAt) >= maxClip {
		source := r.clipSource
		r.stopRecordingLocked(false)
		r.startRolloverLocked(frag, source)
		return
	}

	if r.awaitingKeyframe {
		if !frag.IsKeyframe {
			return
		}
		r.awaitingKeyframe = false
		r.firstFragAt = frag.ReceivedAt
	}

	if err := r.writeFragmentLocked(frag); err != nil {
		r.logger.Printf("[recorder][%s] write fragment error: %v", r.cam.ID, err)
		r.stopRecordingLocked(false)
		return
	}
}

// Trigger starts recording (if idle) or resets the post-record cooldown (if active).
func (r *CameraRecorder) Trigger(source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.triggerLocked(source, time.Now())
}

// HandleMotionEvent processes an active/inactive motion event from a detector.
func (r *CameraRecorder) HandleMotionEvent(ev motion.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := ev.Time
	if now.IsZero() {
		now = time.Now()
	}

	r.motionActive = ev.Active
	if ev.Active {
		r.lastMotionEvent = now
		r.triggerLocked(ev.Source, now)
	} else if r.recording {
		r.resetCooldownLocked()
	}
}

func (r *CameraRecorder) triggerLocked(source string, now time.Time) {
	if source == "" {
		source = "manual"
	}
	if r.initInfo == nil {
		return
	}
	if r.recording {
		r.resetCooldownLocked()
		return
	}
	if err := r.startRecordingLocked(source, now); err != nil {
		r.logger.Printf("[recorder][%s] failed to start clip: %v", r.cam.ID, err)
	}
}

func (r *CameraRecorder) startRecordingLocked(source string, now time.Time) error {
	snap := r.ring.Snapshot()
	clipStart := now
	if len(snap) > 0 && !snap[0].ReceivedAt.IsZero() {
		clipStart = snap[0].ReceivedAt
	}

	if err := r.openClipFileLocked(clipStart, source); err != nil {
		return err
	}

	if len(snap) > 0 {
		r.awaitingKeyframe = false
		r.firstFragAt = snap[0].ReceivedAt
		for _, frag := range snap {
			if err := r.writeFragmentLocked(frag); err != nil {
				r.abortClipFileLocked()
				return err
			}
		}
		if err := r.clipBuf.Flush(); err != nil {
			r.abortClipFileLocked()
			return err
		}
	} else {
		r.awaitingKeyframe = true
		r.firstFragAt = time.Time{}
	}

	r.resetCooldownLocked()
	r.spawnThumbnailCapture(r.clipAbsPath, r.clipRelPath)
	return nil
}

func (r *CameraRecorder) startRolloverLocked(keyFrag fmp4.Fragment, source string) {
	now := keyFrag.ReceivedAt
	if now.IsZero() {
		now = time.Now()
	}
	if err := r.openClipFileLocked(now, source); err != nil {
		r.logger.Printf("[recorder][%s] rollover open error: %v", r.cam.ID, err)
		return
	}
	r.awaitingKeyframe = false
	r.firstFragAt = now
	if err := r.writeFragmentLocked(keyFrag); err != nil {
		r.logger.Printf("[recorder][%s] rollover write error: %v", r.cam.ID, err)
		r.abortClipFileLocked()
		return
	}
	r.resetCooldownLocked()
	r.spawnThumbnailCapture(r.clipAbsPath, r.clipRelPath)
}

func (r *CameraRecorder) openClipFileLocked(clipStart time.Time, source string) error {
	dateDir := clipStart.Format("2006-01-02")
	dirAbs := filepath.Join(r.storage.Dir, r.cam.ID, dateDir)
	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		return fmt.Errorf("create clip dir: %w", err)
	}

	baseName := clipStart.Format("20060102_150405")
	var relPath, absPath string
	for idx := 0; idx < 100; idx++ {
		name := baseName + ".mp4"
		if idx > 0 {
			name = fmt.Sprintf("%s_%d.mp4", baseName, idx)
		}
		candidateRel := filepath.ToSlash(filepath.Join(r.cam.ID, dateDir, name))
		candidateAbs := filepath.Join(dirAbs, name)
		if _, err := os.Stat(candidateAbs); os.IsNotExist(err) {
			relPath = candidateRel
			absPath = candidateAbs
			break
		}
	}
	if absPath == "" {
		return fmt.Errorf("could not allocate unique clip filename for %s", baseName)
	}

	f, err := os.OpenFile(absPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil && os.IsNotExist(err) {
		if mkErr := os.MkdirAll(dirAbs, 0o755); mkErr == nil {
			f, err = os.OpenFile(absPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		}
	}
	if err != nil {
		return fmt.Errorf("open clip file: %w", err)
	}
	bw := bufio.NewWriterSize(f, 64*1024)
	if _, err := bw.Write(r.initInfo.Raw); err != nil {
		_ = f.Close()
		_ = os.Remove(absPath)
		return fmt.Errorf("write init segment: %w", err)
	}

	r.recording = true
	r.wroteFragments = 0
	r.wroteAudioFragments = 0
	if r.initInfo != nil && r.initInfo.AudioTrackID != 0 {
		r.clipHasAudioTrack = true
		r.clipAudioSampleType = r.initInfo.AudioSampleType
		r.clipAudioTimescale = r.initInfo.AudioTimescale()
	} else {
		r.clipHasAudioTrack = false
		r.clipAudioSampleType = ""
		r.clipAudioTimescale = 0
	}
	r.clipStart = clipStart
	r.lastFragAt = clipStart
	r.clipFile = f
	r.clipBuf = bw
	r.clipRelPath = relPath
	r.clipAbsPath = absPath
	r.clipThumbRel = ""
	r.clipSource = source
	r.rebaser = fmp4.NewClipRebaser()
	return nil
}

func (r *CameraRecorder) writeFragmentLocked(frag fmp4.Fragment) error {
	rebasedMoof, err := r.rebaser.RebaseMoof(frag.Moof)
	if err != nil {
		return err
	}
	if _, err := r.clipBuf.Write(rebasedMoof); err != nil {
		return err
	}
	if _, err := r.clipBuf.Write(frag.Mdat); err != nil {
		return err
	}
	r.wroteFragments++
	if frag.IsAudio {
		r.wroteAudioFragments++
	}
	if !frag.ReceivedAt.IsZero() {
		r.lastFragAt = frag.ReceivedAt
	}
	if frag.IsKeyframe {
		if err := r.clipBuf.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func (r *CameraRecorder) abortClipFileLocked() {
	if r.clipFile != nil {
		_ = r.clipFile.Close()
	}
	if r.clipAbsPath != "" {
		_ = os.Remove(r.clipAbsPath)
	}
	r.recording = false
	r.wroteFragments = 0
	r.wroteAudioFragments = 0
	r.clipHasAudioTrack = false
	r.clipAudioSampleType = ""
	r.clipAudioTimescale = 0
	r.clipFile = nil
	r.clipBuf = nil
	r.clipAbsPath = ""
	r.clipRelPath = ""
	r.rebaser = nil
}

func (r *CameraRecorder) resetCooldownLocked() {
	r.cooldownGen++
	gen := r.cooldownGen
	postDur := time.Duration(r.cam.EffectivePostRecordSec(r.defaults)) * time.Second
	if postDur <= 0 {
		postDur = 10 * time.Second
	}

	if r.cooldownTimer != nil {
		r.cooldownTimer.Stop()
	}
	r.cooldownTimer = time.AfterFunc(postDur, func() {
		r.onCooldownExpired(gen)
	})
}

func (r *CameraRecorder) onCooldownExpired(gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recording || r.cooldownGen != gen {
		return
	}
	// If ONVIF/ISAPI reported motion still active, keep extending unless stale for >90s.
	if r.motionActive && time.Since(r.lastMotionEvent) < 90*time.Second {
		r.resetCooldownLocked()
		return
	}
	r.motionActive = false
	r.stopRecordingLocked(false)
}

func (r *CameraRecorder) stopRecordingLocked(keepFile bool) {
	if !r.recording {
		return
	}
	if r.cooldownTimer != nil {
		r.cooldownTimer.Stop()
		r.cooldownTimer = nil
	}
	r.cooldownGen++

	if r.clipBuf != nil {
		_ = r.clipBuf.Flush()
	}
	if r.clipFile != nil {
		_ = r.clipFile.Close()
	}

	j := clipFinalizeJob{
		absPath:         r.clipAbsPath,
		relPath:         r.clipRelPath,
		thumbRel:        r.clipThumbRel,
		start:           r.clipStart,
		end:             r.lastFragAt,
		source:          r.clipSource,
		hasAudioTrack:   r.clipHasAudioTrack,
		audioSampleType: r.clipAudioSampleType,
		audioTimescale:  r.clipAudioTimescale,
		wroteAudioFrags: r.wroteAudioFragments,
		ffmpegBin:       r.ffmpegBin,
		job:             r.job,
	}
	wrote := r.wroteFragments

	r.recording = false
	r.wroteFragments = 0
	r.wroteAudioFragments = 0
	r.clipHasAudioTrack = false
	r.clipAudioSampleType = ""
	r.clipAudioTimescale = 0
	r.clipFile = nil
	r.clipBuf = nil
	r.clipAbsPath = ""
	r.clipRelPath = ""
	r.rebaser = nil

	if wrote == 0 && !keepFile {
		if j.absPath != "" {
			_ = os.Remove(j.absPath)
		}
		return
	}

	st, err := os.Stat(j.absPath)
	if err != nil || st.Size() == 0 {
		if j.absPath != "" {
			_ = os.Remove(j.absPath)
		}
		return
	}

	if needsAudioRemux(j.hasAudioTrack, j.audioSampleType) && strings.TrimSpace(j.ffmpegBin) != "" {
		r.finalizeWG.Add(1)
		go func(job clipFinalizeJob) {
			defer r.finalizeWG.Done()
			r.remuxClipAudio(job)
			r.commitFinalizedClip(job)
		}(j)
		return
	}

	r.commitFinalizedClip(j)
}

func (r *CameraRecorder) remuxClipAudio(j clipFinalizeJob) {
	audioRemuxSem <- struct{}{}
	defer func() { <-audioRemuxSem }()

	tmpAbs := j.absPath + ".tmp"
	_ = os.Remove(tmpAbs)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-threads", "1",
		"-i", j.absPath,
	}
	if j.wroteAudioFrags > 0 {
		args = append(args, "-map", "0:v:0", "-map", "0:a:0", "-c:v", "copy", "-c:a", "aac")
		if j.audioTimescale > 0 && j.audioTimescale < 16000 {
			args = append(args, "-ar", "16000", "-b:a", "48k")
		} else {
			args = append(args, "-b:a", "64k")
		}
	} else {
		args = append(args, "-map", "0:v:0", "-c:v", "copy")
	}
	args = append(args, "-movflags", "+faststart", "-f", "mp4", "-y", tmpAbs)

	var errOut limitedWriter
	errOut.max = 2048

	cmd := exec.CommandContext(ctx, j.ffmpegBin, args...)
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		r.logger.Printf("[recorder][%s] audio remux start failed for %s: %v", r.cam.ID, j.relPath, err)
		_ = os.Remove(tmpAbs)
		return
	}
	if j.job != nil {
		_ = j.job.Assign(cmd.Process.Pid)
	}
	if err := cmd.Wait(); err != nil {
		detail := strings.TrimSpace(errOut.buf.String())
		if detail != "" {
			r.logger.Printf("[recorder][%s] audio remux failed for %s (%v): %s", r.cam.ID, j.relPath, err, detail)
		} else {
			r.logger.Printf("[recorder][%s] audio remux failed for %s: %v", r.cam.ID, j.relPath, err)
		}
		_ = os.Remove(tmpAbs)
		return
	}

	st, err := os.Stat(tmpAbs)
	if err != nil || st.Size() == 0 {
		r.logger.Printf("[recorder][%s] audio remux produced empty output for %s", r.cam.ID, j.relPath)
		_ = os.Remove(tmpAbs)
		return
	}

	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		renameErr = os.Rename(tmpAbs, j.absPath)
		if renameErr == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.logger.Printf("[recorder][%s] audio remux rename failed for %s: %v", r.cam.ID, j.relPath, renameErr)
	_ = os.Remove(tmpAbs)
}

func (r *CameraRecorder) commitFinalizedClip(j clipFinalizeJob) {
	st, err := os.Stat(j.absPath)
	if err != nil || st.Size() == 0 {
		if j.absPath != "" {
			_ = os.Remove(j.absPath)
		}
		return
	}

	thumbRel := j.thumbRel
	if thumbRel == "" {
		thumbAbs := strings.TrimSuffix(j.absPath, filepath.Ext(j.absPath)) + ".jpg"
		if thumbSt, thumbErr := os.Stat(thumbAbs); thumbErr == nil && !thumbSt.IsDir() && thumbSt.Size() > 0 {
			thumbRel = strings.TrimSuffix(j.relPath, filepath.Ext(j.relPath)) + ".jpg"
		}
	}

	end := j.end
	if end.Before(j.start) {
		end = time.Now()
	}
	dur := end.Sub(j.start).Seconds()
	if dur < 0.1 {
		dur = 0.1
	}

	rec := &storage.Recording{
		CameraID:      r.cam.ID,
		CameraName:    r.cam.Name,
		StartTime:     j.start.Unix(),
		EndTime:       end.Unix(),
		DurationSec:   dur,
		FilePath:      j.relPath,
		ThumbPath:     thumbRel,
		SizeBytes:     st.Size(),
		TriggerSource: j.source,
	}

	if r.db != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, insErr := r.db.InsertRecording(ctx, rec)
		cancel()
		if insErr != nil {
			r.logger.Printf("[recorder][%s] sqlite insert error: %v", r.cam.ID, insErr)
		}
	}
	if thumbRel == "" && j.ffmpegBin != "" {
		r.spawnFFmpegThumbnailFallback(j.absPath, j.relPath, j.ffmpegBin, j.job)
	}
	if r.retention != nil {
		r.retention.Notify()
	}
	r.logger.Printf("[recorder][%s] saved clip %s (%.1fs, %.1f KB, trigger=%s)",
		r.cam.ID, j.relPath, dur, float64(st.Size())/1024.0, j.source)
}

func (r *CameraRecorder) spawnFFmpegThumbnailFallback(clipAbs, clipRel, ffmpegBin string, job *procjob.Manager) {
	thumbAbs := strings.TrimSuffix(clipAbs, filepath.Ext(clipAbs)) + ".jpg"
	thumbRel := strings.TrimSuffix(clipRel, filepath.Ext(clipRel)) + ".jpg"
	thumbTmp := strings.TrimSuffix(clipAbs, filepath.Ext(clipAbs)) + ".thumb.tmp.jpg"

	go func() {
		if st, err := os.Stat(thumbAbs); err == nil && st.Size() > 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, ffmpegBin,
			"-hide_banner", "-loglevel", "error",
			"-i", clipAbs,
			"-frames:v", "1",
			"-q:v", "6",
			"-vf", "scale=480:-1",
			"-y", thumbTmp,
		)
		if err := cmd.Start(); err != nil {
			return
		}
		if job != nil {
			_ = job.Assign(cmd.Process.Pid)
		}
		if err := cmd.Wait(); err != nil {
			_ = os.Remove(thumbTmp)
			return
		}
		st, err := os.Stat(thumbTmp)
		if err != nil || st.Size() == 0 {
			_ = os.Remove(thumbTmp)
			return
		}
		if existing, err := os.Stat(thumbAbs); err == nil && existing.Size() > 0 {
			_ = os.Remove(thumbTmp)
			return
		}
		if err := os.Rename(thumbTmp, thumbAbs); err != nil {
			_ = os.Remove(thumbTmp)
			return
		}
		if r.db != nil {
			updCtx, updCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = r.db.UpdateThumbPathByFilePath(updCtx, clipRel, thumbRel)
			updCancel()
		}
	}()
}

func (r *CameraRecorder) spawnThumbnailCapture(clipAbs, clipRel string) {
	if strings.TrimSpace(r.frameURL) == "" {
		return
	}
	thumbAbs := strings.TrimSuffix(clipAbs, filepath.Ext(clipAbs)) + ".jpg"
	thumbRel := strings.TrimSuffix(clipRel, filepath.Ext(clipRel)) + ".jpg"

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.frameURL, nil)
		if err != nil {
			return
		}
		resp, err := r.thumbCli.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		if err != nil || len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
			return
		}
		if err := os.WriteFile(thumbAbs, data, 0o644); err != nil {
			return
		}

		r.mu.Lock()
		stillOpen := r.recording && r.clipRelPath == clipRel
		if stillOpen {
			r.clipThumbRel = thumbRel
		}
		r.mu.Unlock()

		// If the clip already finalized and got inserted before the snapshot arrived, update SQLite by file_path.
		if !stillOpen && r.db != nil {
			updCtx, updCancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = r.db.UpdateThumbPathByFilePath(updCtx, clipRel, thumbRel)
			updCancel()
		}
	}()
}

// SetConfigRev updates the camera configuration revision included in /api/status snapshots.
func (r *CameraRecorder) SetConfigRev(rev uint64) {
	r.mu.Lock()
	r.configRev = rev
	r.mu.Unlock()
}

// Status returns a sanitized live snapshot of this camera's recorder and motion state.
func (r *CameraRecorder) Status() CameraStatus {
	r.mu.Lock()
	st := CameraStatus{
		ID:          r.cam.ID,
		Name:        r.cam.Name,
		Online:      r.online,
		Recording:   r.recording,
		StreamSub:   r.subStream,
		StreamMain:  r.cam.ID + "_main",
		BitrateKbps: r.bitrateKbps,
		FPS:         r.fps,
		LiveStream:  r.cam.EffectiveLiveStream(),
		AspectRatio: r.cam.AspectRatio,
		Resolution:  r.cam.Resolution,
		ConfigRev:   r.configRev,
		MotionMode:  r.cam.Motion.Mode,
		PTZEnabled:  r.cam.PTZ.Enabled,
		Reconnects:  r.reconnects,
		LastError:   r.lastError,
	}
	if r.initInfo != nil {
		st.Width = int(r.initInfo.Width)
		st.Height = int(r.initInfo.Height)
		st.Codec = r.initInfo.Codec
	}
	det := r.detector
	r.mu.Unlock()

	if det != nil {
		ds := det.Stats()
		st.MotionMode = ds.Mode
		st.MotionActive = ds.Active
		st.MotionScore = ds.LastScore
		st.LastMotion = ds.LastEvent
		if st.LastError == "" && ds.LastError != "" {
			st.LastError = ds.LastError
		}
	}
	return st
}

// Manager coordinates recorders for all enabled cameras.
type Manager struct {
	mu        sync.RWMutex
	order     []string
	recorders map[string]*CameraRecorder
}

// NewManager creates an empty recorder Manager.
func NewManager() *Manager {
	return &Manager{
		recorders: make(map[string]*CameraRecorder),
	}
}

// Register adds a CameraRecorder to the manager.
func (m *Manager) Register(rec *CameraRecorder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.recorders[rec.cam.ID]; !exists {
		m.order = append(m.order, rec.cam.ID)
	}
	m.recorders[rec.cam.ID] = rec
}

// Remove unregisters camID from the recorder manager.
func (m *Manager) Remove(camID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.recorders, camID)
	filtered := m.order[:0]
	for _, id := range m.order {
		if id != camID {
			filtered = append(filtered, id)
		}
	}
	m.order = filtered
}

// Get returns the CameraRecorder for camID.
func (m *Manager) Get(camID string) (*CameraRecorder, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.recorders[camID]
	return rec, ok
}

// TriggerManual triggers recording for camID.
func (m *Manager) TriggerManual(camID string) bool {
	rec, ok := m.Get(camID)
	if !ok {
		return false
	}
	rec.Trigger("manual")
	return true
}

// Statuses returns status snapshots for all registered cameras in declaration order.
func (m *Manager) Statuses() []CameraStatus {
	m.mu.RLock()
	ids := append([]string(nil), m.order...)
	recs := make([]*CameraRecorder, 0, len(ids))
	for _, id := range ids {
		recs = append(recs, m.recorders[id])
	}
	m.mu.RUnlock()

	out := make([]CameraStatus, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.Status())
	}
	return out
}

// StartAll launches all CameraRecorder Run loops in background goroutines until ctx is canceled.
func (m *Manager) StartAll(ctx context.Context, wg *sync.WaitGroup) {
	m.mu.RLock()
	recs := make([]*CameraRecorder, 0, len(m.order))
	for _, id := range m.order {
		recs = append(recs, m.recorders[id])
	}
	m.mu.RUnlock()

	for _, rec := range recs {
		r := rec
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Run(ctx)
		}()
	}
}
