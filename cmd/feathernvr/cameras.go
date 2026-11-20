package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"feathernvr/internal/config"
	"feathernvr/internal/go2rtc"
	"feathernvr/internal/motion"
	"feathernvr/internal/procjob"
	"feathernvr/internal/ptz"
	"feathernvr/internal/recorder"
	"feathernvr/internal/storage"
)

type runningCam struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// CameraRuntime manages live per-camera goroutines, go2rtc streams, PTZ controllers, and YAML persistence.
type CameraRuntime struct {
	opMu       sync.Mutex
	mu         sync.RWMutex
	rootCtx    context.Context
	cfg        *config.Config
	configPath string
	db         *storage.DB
	retention  *storage.RetentionManager
	g2         *go2rtc.Manager
	job        *procjob.Manager
	recMgr     *recorder.Manager
	ptzMgr     *ptz.Manager
	logger     *log.Logger

	isapiHubs map[string]*motion.ISAPIHub
	hubDone   []chan struct{}
	running   map[string]*runningCam
	rev       uint64
}

// NewCameraRuntime creates a live camera runtime manager.
func NewCameraRuntime(
	rootCtx context.Context,
	cfg *config.Config,
	configPath string,
	db *storage.DB,
	retention *storage.RetentionManager,
	g2 *go2rtc.Manager,
	job *procjob.Manager,
	recMgr *recorder.Manager,
	ptzMgr *ptz.Manager,
	logger *log.Logger,
) *CameraRuntime {
	return &CameraRuntime{
		rootCtx:    rootCtx,
		cfg:        cfg,
		configPath: configPath,
		db:         db,
		retention:  retention,
		g2:         g2,
		job:        job,
		recMgr:     recMgr,
		ptzMgr:     ptzMgr,
		logger:     logger,
		isapiHubs:  make(map[string]*motion.ISAPIHub),
		running:    make(map[string]*runningCam),
	}
}

// StartInitial launches all initially enabled cameras and required ISAPI hubs.
func (rt *CameraRuntime) StartInitial() {
	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	rt.mu.Lock()
	defer rt.mu.Unlock()

	for _, cam := range rt.cfg.EnabledCameras() {
		rt.startCameraLocked(cam)
	}
}

// Wait blocks until all camera and DVR hub goroutines exit after rootCtx cancellation.
func (rt *CameraRuntime) Wait() {
	rt.mu.RLock()
	dones := make([]chan struct{}, 0, len(rt.running)+len(rt.hubDone))
	for _, rc := range rt.running {
		dones = append(dones, rc.done)
	}
	dones = append(dones, rt.hubDone...)
	rt.mu.RUnlock()

	for _, ch := range dones {
		<-ch
	}
}

// ListCameras returns a shallow snapshot slice of all configured cameras.
func (rt *CameraRuntime) ListCameras() []config.CameraConfig {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	out := make([]config.CameraConfig, len(rt.cfg.Cameras))
	copy(out, rt.cfg.Cameras)
	return out
}

// GetCamera returns the configured camera for id if found.
func (rt *CameraRuntime) GetCamera(id string) (config.CameraConfig, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for _, cam := range rt.cfg.Cameras {
		if cam.ID == id {
			return cam, true
		}
	}
	return config.CameraConfig{}, false
}

// ListDVRs returns configured Hikvision DVR hosts.
func (rt *CameraRuntime) ListDVRs() []config.HikvisionDVRConfig {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	out := make([]config.HikvisionDVRConfig, len(rt.cfg.HikvisionDVRs))
	copy(out, rt.cfg.HikvisionDVRs)
	return out
}

// AddCamera validates, persists, and starts a new camera without restarting FeatherNVR.
func (rt *CameraRuntime) AddCamera(ctx context.Context, cam config.CameraConfig) (config.CameraConfig, error) {
	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	rt.mu.RLock()
	tmp := *rt.cfg
	tmp.Cameras = make([]config.CameraConfig, len(rt.cfg.Cameras)+1)
	copy(tmp.Cameras, rt.cfg.Cameras)
	tmp.Cameras[len(tmp.Cameras)-1] = cam
	configPath := rt.configPath
	rt.mu.RUnlock()

	tmp.ApplyDefaults()
	if err := tmp.Validate(); err != nil {
		return config.CameraConfig{}, err
	}
	savedCam := tmp.Cameras[len(tmp.Cameras)-1]

	if configPath != "" {
		if err := config.SaveCameras(configPath, tmp.Cameras); err != nil {
			return config.CameraConfig{}, err
		}
	}

	rt.mu.Lock()
	rt.cfg.Cameras = tmp.Cameras
	rt.mu.Unlock()

	if err := rt.g2.WriteConfig(); err != nil {
		rt.logger.Printf("[cameras] warning updating go2rtc runtime yaml: %v", err)
	}

	if savedCam.IsEnabled() {
		rt.syncGo2RTCStreams(ctx, savedCam)
		rt.mu.Lock()
		rt.startCameraLocked(savedCam)
		rt.mu.Unlock()
	}

	rt.logger.Printf("[cameras] added %q (%s)", savedCam.ID, savedCam.Name)
	return savedCam, nil
}

// UpdateCamera validates, persists, and hot-restarts a camera with updated settings.
func (rt *CameraRuntime) UpdateCamera(ctx context.Context, cam config.CameraConfig) (config.CameraConfig, error) {
	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	rt.mu.RLock()
	tmp := *rt.cfg
	tmp.Cameras = make([]config.CameraConfig, len(rt.cfg.Cameras))
	copy(tmp.Cameras, rt.cfg.Cameras)
	configPath := rt.configPath
	rt.mu.RUnlock()

	idx := -1
	for i := range tmp.Cameras {
		if tmp.Cameras[i].ID == cam.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return config.CameraConfig{}, os.ErrNotExist
	}

	tmp.Cameras[idx] = cam
	tmp.ApplyDefaults()
	if err := tmp.Validate(); err != nil {
		return config.CameraConfig{}, err
	}
	savedCam := tmp.Cameras[idx]

	if configPath != "" {
		if err := config.SaveCameras(configPath, tmp.Cameras); err != nil {
			return config.CameraConfig{}, err
		}
	}

	rt.stopCamera(savedCam.ID)

	rt.mu.Lock()
	rt.cfg.Cameras = tmp.Cameras
	rt.mu.Unlock()

	if err := rt.g2.WriteConfig(); err != nil {
		rt.logger.Printf("[cameras] warning updating go2rtc runtime yaml: %v", err)
	}

	if savedCam.IsEnabled() {
		rt.syncGo2RTCStreams(ctx, savedCam)
		rt.mu.Lock()
		rt.startCameraLocked(savedCam)
		rt.mu.Unlock()
	} else {
		rt.deleteGo2RTCStreams(ctx, savedCam.ID)
	}

	rt.logger.Printf("[cameras] updated %q (%s)", savedCam.ID, savedCam.Name)
	return savedCam, nil
}

// DeleteCamera stops a camera, removes it from go2rtc and config.yaml (preserving recorded clips on disk).
func (rt *CameraRuntime) DeleteCamera(ctx context.Context, id string) error {
	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	rt.mu.RLock()
	nextCams := make([]config.CameraConfig, 0, len(rt.cfg.Cameras))
	found := false
	for _, c := range rt.cfg.Cameras {
		if c.ID == id {
			found = true
			continue
		}
		nextCams = append(nextCams, c)
	}
	configPath := rt.configPath
	rt.mu.RUnlock()

	if !found {
		return os.ErrNotExist
	}

	if configPath != "" {
		if err := config.SaveCameras(configPath, nextCams); err != nil {
			return err
		}
	}

	rt.stopCamera(id)

	rt.mu.Lock()
	rt.cfg.Cameras = nextCams
	rt.mu.Unlock()

	if err := rt.g2.WriteConfig(); err != nil {
		rt.logger.Printf("[cameras] warning updating go2rtc runtime yaml: %v", err)
	}
	rt.deleteGo2RTCStreams(ctx, id)

	rt.logger.Printf("[cameras] deleted %q", id)
	return nil
}

// TestStream delegates live stream probing to go2rtc.Manager.
func (rt *CameraRuntime) TestStream(ctx context.Context, rawURL string) (*go2rtc.StreamTestResult, error) {
	return rt.g2.TestStream(ctx, rawURL)
}

// ProbeCamera delegates ONVIF/Hikvision auto-detection to motion.ProbeCamera.
func (rt *CameraRuntime) ProbeCamera(ctx context.Context, req motion.ProbeRequest) (*motion.ProbeResult, error) {
	return motion.ProbeCamera(ctx, req)
}

func (rt *CameraRuntime) syncGo2RTCStreams(ctx context.Context, cam config.CameraConfig) {
	if err := rt.g2.UpsertStream(ctx, cam.ID+"_main", cam.MainRTSP); err != nil {
		rt.logger.Printf("[cameras][%s] go2rtc upsert main warning: %v", cam.ID, err)
	}
	if strings.TrimSpace(cam.SubRTSP) != "" {
		if err := rt.g2.UpsertStream(ctx, cam.ID+"_sub", cam.SubRTSP); err != nil {
			rt.logger.Printf("[cameras][%s] go2rtc upsert sub warning: %v", cam.ID, err)
		}
	} else {
		_ = rt.g2.DeleteStream(ctx, cam.ID+"_sub")
	}
}

func (rt *CameraRuntime) deleteGo2RTCStreams(ctx context.Context, id string) {
	_ = rt.g2.DeleteStream(ctx, id+"_main")
	_ = rt.g2.DeleteStream(ctx, id+"_sub")
}

func (rt *CameraRuntime) stopCamera(id string) {
	rt.mu.Lock()
	rc, ok := rt.running[id]
	if ok {
		delete(rt.running, id)
	}
	rt.recMgr.Remove(id)
	rt.ptzMgr.Remove(id)
	rt.mu.Unlock()

	if ok {
		rc.cancel()
		<-rc.done
	}
}

func (rt *CameraRuntime) ensureISAPIHubLocked(dvr config.HikvisionDVRConfig) *motion.ISAPIHub {
	if hub, exists := rt.isapiHubs[dvr.ID]; exists {
		return hub
	}
	hub := motion.NewISAPIHub(dvr, rt.logger)
	rt.isapiHubs[dvr.ID] = hub
	done := make(chan struct{})
	rt.hubDone = append(rt.hubDone, done)
	go func() {
		defer close(done)
		hub.Run(rt.rootCtx)
	}()
	return hub
}

func (rt *CameraRuntime) startCameraLocked(cam config.CameraConfig) {
	if prev, ok := rt.running[cam.ID]; ok {
		delete(rt.running, cam.ID)
		prev.cancel()
		<-prev.done
	}

	subStream := go2rtc.StreamNameSub(cam)
	mp4URL := rt.g2.MainMP4URL(cam.ID)
	frameURL := rt.g2.FrameJPEGURL(subStream)
	motionInputURL := rt.g2.SubMP4URL(cam)

	rec := recorder.NewCameraRecorder(
		cam,
		rt.cfg.Recording,
		rt.cfg.Storage,
		mp4URL,
		frameURL,
		subStream,
		rt.db,
		rt.retention,
		nil,
		rt.logger,
	)
	rec.SetFFmpeg(rt.cfg.FFmpegBin, rt.job)
	rt.rev++
	rec.SetConfigRev(rt.rev)

	var det motion.Detector
	switch cam.Motion.Mode {
	case config.MotionModeONVIF:
		det = motion.NewONVIFDetector(cam.ID, cam.Motion.ONVIFURL, cam.Motion.Username, cam.Motion.Password, rt.logger)
	case config.MotionModePixel:
		det = motion.NewPixelDetector(cam.ID, motionInputURL, rt.cfg.FFmpegBin, cam.Motion, rt.job, rt.logger)
	case config.MotionModeONVIFOrPixel:
		det = motion.NewONVIFOrPixelDetector(
			cam.ID,
			cam.Motion.ONVIFURL,
			cam.Motion.Username,
			cam.Motion.Password,
			motionInputURL,
			rt.cfg.FFmpegBin,
			cam.Motion,
			rt.job,
			rt.logger,
		)
	case config.MotionModeHikvisionISAPI, config.MotionModeISAPIOrPixel:
		dvr, ok := rt.cfg.FindDVR(cam.Motion.DVR)
		if !ok {
			rt.logger.Printf("[cameras][%s] unknown dvr %q; falling back to none motion", cam.ID, cam.Motion.DVR)
			det = motion.NewNoneDetector()
		} else {
			hub := rt.ensureISAPIHubLocked(dvr)
			if cam.Motion.Mode == config.MotionModeISAPIOrPixel {
				det = motion.NewISAPIOrPixelDetector(
					cam.ID,
					hub,
					cam.Motion.Channel,
					motionInputURL,
					rt.cfg.FFmpegBin,
					cam.Motion,
					rt.job,
					rt.logger,
				)
			} else {
				det = hub.ChannelDetector(cam.ID, cam.Motion.Channel)
			}
		}
	case config.MotionModeNone:
		det = motion.NewNoneDetector()
	default:
		det = motion.NewNoneDetector()
	}

	rec.SetDetector(det)
	rt.recMgr.Register(rec)

	if cam.PTZ.Enabled {
		ctrl, err := ptz.NewController(cam.PTZ)
		if err != nil {
			rt.logger.Printf("[ptz][%s] init warning: %v", cam.ID, err)
			rt.ptzMgr.Remove(cam.ID)
		} else {
			rt.ptzMgr.Register(cam.ID, ctrl)
			rt.logger.Printf("[ptz][%s] enabled (%s)", cam.ID, cam.PTZ.Type)
		}
	} else {
		rt.ptzMgr.Remove(cam.ID)
	}

	camCtx, cancel := context.WithCancel(rt.rootCtx)
	done := make(chan struct{})

	go func(cID string, d motion.Detector, r *recorder.CameraRecorder) {
		defer close(done)
		var inner sync.WaitGroup
		inner.Add(2)
		go func() {
			defer inner.Done()
			d.Run(camCtx, r.HandleMotionEvent)
		}()
		go func() {
			defer inner.Done()
			r.Run(camCtx)
		}()
		inner.Wait()
		_ = fmt.Sprintf("stopped %s", cID)
	}(cam.ID, det, rec)

	rt.running[cam.ID] = &runningCam{cancel: cancel, done: done}
}
