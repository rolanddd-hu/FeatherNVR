package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"feathernvr/internal/blueiris"
	"feathernvr/internal/config"
	"feathernvr/internal/go2rtc"
	"feathernvr/internal/motion"
	"feathernvr/internal/procjob"
	"feathernvr/internal/ptz"
	"feathernvr/internal/recorder"
	"feathernvr/internal/server"
	"feathernvr/internal/storage"
)

const version = "0.1.0"

func main() {
	configPath := flag.String("config", "config.yaml", "Path to FeatherNVR YAML configuration")
	discoverFlag := flag.Bool("discover", false, "Scan LAN for ONVIF cameras and exit")
	checkFlag := flag.Bool("check", false, "Validate configuration and print sanitized camera summary")
	importBI := flag.String("import-blueiris", "", "Import Blue Iris cameras from 'registry' or a .reg file path")
	importOut := flag.String("import-out", "config.yaml", "Output path for -import-blueiris")
	maxGBFlag := flag.Float64("max-gb", 50, "Storage cap in GB when generating config via -import-blueiris")
	forceFlag := flag.Bool("force", false, "Overwrite existing -import-out file (creates a .bak backup)")
	versionFlag := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("feathernvr %s\n", version)
		return
	}

	if *discoverFlag {
		runDiscoverCLI()
		return
	}

	if strings.TrimSpace(*importBI) != "" {
		runImportBlueIris(*importBI, *importOut, *maxGBFlag, *forceFlag)
		return
	}

	if *checkFlag {
		runCheckCLI(*configPath)
		return
	}

	logger := log.New(os.Stdout, "", log.LstdFlags)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("configuration error: %v", err)
	}

	enabledCams := cfg.EnabledCameras()
	if len(enabledCams) == 0 {
		logger.Printf("warning: no enabled cameras configured in %s", *configPath)
	}

	job, err := procjob.New()
	if err != nil {
		logger.Fatalf("init process job object: %v", err)
	}
	defer func() {
		if err := job.Close(); err != nil {
			logger.Printf("warning: close job object: %v", err)
		}
	}()

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	db, err := storage.Open(cfg.Storage.DBPath)
	if err != nil {
		logger.Fatalf("open sqlite db: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Printf("warning: close sqlite db: %v", err)
		}
	}()

	retention := storage.NewRetentionManager(db, cfg.Storage, logger)
	if err := retention.Reconcile(rootCtx); err != nil {
		logger.Fatalf("storage reconcile error: %v", err)
	}
	if _, _, err := retention.Enforce(rootCtx); err != nil {
		logger.Printf("warning: initial retention enforce: %v", err)
	}

	g2 := go2rtc.New(cfg, job, logger)
	logger.Printf("starting go2rtc (%s)...", cfg.Go2RTC.Bin)
	if err := g2.Start(rootCtx); err != nil {
		logger.Fatalf("start go2rtc: %v", err)
	}
	defer g2.Wait()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		retention.Run(rootCtx)
	}()

	recMgr := recorder.NewManager()
	isapiHubs := make(map[string]*motion.ISAPIHub)
	ptzMgr := ptz.NewManager()

	for _, cam := range enabledCams {
		subStream := go2rtc.StreamNameSub(cam)
		mp4URL := g2.MainMP4URL(cam.ID)
		frameURL := g2.FrameJPEGURL(subStream)
		motionInputURL := g2.SubMP4URL(cam)

		rec := recorder.NewCameraRecorder(
			cam,
			cfg.Recording,
			cfg.Storage,
			mp4URL,
			frameURL,
			subStream,
			db,
			retention,
			nil,
			logger,
		)
		rec.SetFFmpeg(cfg.FFmpegBin, job)

		var det motion.Detector
		switch cam.Motion.Mode {
		case config.MotionModeONVIF:
			det = motion.NewONVIFDetector(cam.ID, cam.Motion.ONVIFURL, cam.Motion.Username, cam.Motion.Password, logger)
		case config.MotionModePixel:
			det = motion.NewPixelDetector(cam.ID, motionInputURL, cfg.FFmpegBin, cam.Motion, job, logger)
		case config.MotionModeONVIFOrPixel:
			det = motion.NewONVIFOrPixelDetector(
				cam.ID,
				cam.Motion.ONVIFURL,
				cam.Motion.Username,
				cam.Motion.Password,
				motionInputURL,
				cfg.FFmpegBin,
				cam.Motion,
				job,
				logger,
			)
		case config.MotionModeHikvisionISAPI, config.MotionModeISAPIOrPixel:
			dvr, ok := cfg.FindDVR(cam.Motion.DVR)
			if !ok {
				logger.Fatalf("camera %q references unknown dvr %q", cam.ID, cam.Motion.DVR)
			}
			hub, exists := isapiHubs[dvr.ID]
			if !exists {
				hub = motion.NewISAPIHub(dvr, logger)
				isapiHubs[dvr.ID] = hub
			}
			if cam.Motion.Mode == config.MotionModeISAPIOrPixel {
				det = motion.NewISAPIOrPixelDetector(
					cam.ID,
					hub,
					cam.Motion.Channel,
					motionInputURL,
					cfg.FFmpegBin,
					cam.Motion,
					job,
					logger,
				)
			} else {
				det = hub.ChannelDetector(cam.ID, cam.Motion.Channel)
			}
		case config.MotionModeNone:
			det = motion.NewNoneDetector()
		}

		rec.SetDetector(det)
		recMgr.Register(rec)

		if cam.PTZ.Enabled {
			ctrl, err := ptz.NewController(cam.PTZ)
			if err != nil {
				logger.Printf("[ptz][%s] init warning: %v", cam.ID, err)
			} else {
				ptzMgr.Register(cam.ID, ctrl)
				logger.Printf("[ptz][%s] enabled (%s)", cam.ID, cam.PTZ.Type)
			}
		}

		d := det
		r := rec
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Run(rootCtx, r.HandleMotionEvent)
		}()
	}

	for _, hub := range isapiHubs {
		h := hub
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Run(rootCtx)
		}()
	}

	recMgr.StartAll(rootCtx, &wg)

	go2rtcURL, err := url.Parse(g2.APIBaseURL())
	if err != nil {
		logger.Fatalf("invalid go2rtc api url: %v", err)
	}
	srv, err := server.New(cfg, db, retention, recMgr, go2rtcURL, logger)
	if err != nil {
		logger.Fatalf("init http server: %v", err)
	}
	srv.SetConfigPath(*configPath)
	srv.SetPTZManager(ptzMgr)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	printAccessURLs(logger, cfg.Server.Listen)
	logger.Printf("FeatherNVR %s ready (%d camera(s) active, cap=%.2f GB)", version, len(enabledCams), cfg.Storage.MaxGB)

	select {
	case <-rootCtx.Done():
		logger.Printf("shutting down...")
	case srvErr := <-errCh:
		logger.Printf("HTTP server error: %v", srvErr)
		stopSignals()
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(shutdownCtx)
	cancelShutdown()

	wg.Wait()
	logger.Printf("FeatherNVR stopped cleanly")
}

func runDiscoverCLI() {
	fmt.Println("Scanning LAN for ONVIF devices (4 seconds)...")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	devs, err := motion.Discover(ctx, 4*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discovery failed: %v\n", err)
		os.Exit(1)
	}
	if len(devs) == 0 {
		fmt.Println("No ONVIF devices responded.")
		return
	}
	for _, dev := range devs {
		name := dev.Name
		if name == "" {
			name = "ONVIF Device"
		}
		fmt.Printf("- %s (%s)\n", name, dev.IP)
		for _, xa := range dev.XAddrs {
			fmt.Printf("    onvif_url: %s\n", xa)
		}
	}
}

func runImportBlueIris(source, outPath string, maxGB float64, force bool) {
	var raw []blueiris.RawCamera
	var err error
	if strings.EqualFold(strings.TrimSpace(source), "registry") {
		raw, err = blueiris.ReadWindowsRegistry()
	} else {
		raw, err = blueiris.ReadRegFile(source)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Blue Iris import failed: %v\n", err)
		os.Exit(1)
	}

	res, err := blueiris.Convert(raw, blueiris.ImportOptions{
		MaxGB:           maxGB,
		IncludeDisabled: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Blue Iris conversion failed: %v\n", err)
		os.Exit(1)
	}

	if err := blueiris.WriteConfigYAML(outPath, res.Config, force); err != nil {
		fmt.Fprintf(os.Stderr, "Write config failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Imported %d enabled camera(s) and %d disabled camera(s) (%d Hikvision DVR(s)) to %s\n",
		res.EnabledCount, res.DisabledCount, res.DVRCount, outPath)
	fmt.Printf("Storage cap: %.2f GB (editable in Web UI -> Status -> Storage Cap)\n", res.Config.Storage.MaxGB)
	printSanitizedCameraSummary(res.Config)
	for _, w := range res.Warnings {
		fmt.Printf("Warning: %s\n", w)
	}
}

func runCheckCLI(configPath string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration check failed: %v\n", err)
		os.Exit(1)
	}
	enabled := cfg.EnabledCameras()
	fmt.Printf("Config %s valid: %d enabled / %d total camera(s), %d Hikvision DVR(s), storage cap %.2f GB\n",
		configPath, len(enabled), len(cfg.Cameras), len(cfg.HikvisionDVRs), cfg.Storage.MaxGB)
	printSanitizedCameraSummary(cfg)
}

func printSanitizedCameraSummary(cfg *config.Config) {
	for _, cam := range cfg.Cameras {
		state := "enabled"
		if !cam.IsEnabled() {
			state = "disabled"
		}
		hasSub := strings.TrimSpace(cam.SubRTSP) != ""
		ptzState := "off"
		if cam.PTZ.Enabled {
			ptzState = cam.PTZ.Type
		}
		fmt.Printf("  - %-20s name=%q state=%s motion=%s has_sub=%v ptz=%s\n",
			cam.ID, cam.Name, state, cam.Motion.Mode, hasSub, ptzState)
	}
}

func printAccessURLs(logger *log.Logger, listen string) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		port = strings.TrimPrefix(listen, ":")
	}
	if port == "" {
		port = "8080"
	}

	logger.Printf("Web UI (local): http://127.0.0.1:%s", port)
	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			logger.Printf("Web UI (LAN):   http://%s:%s", ip4.String(), port)
		}
	}
}
