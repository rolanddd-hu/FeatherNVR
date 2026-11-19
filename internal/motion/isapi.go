package motion

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"feathernvr/internal/config"
	"feathernvr/internal/procjob"
)

// ISAPIAlert represents one parsed Hikvision EventNotificationAlert XML payload.
type ISAPIAlert struct {
	ChannelID    int    `xml:"channelID"`
	DynChannelID int    `xml:"dynChannelID"`
	EventType    string `xml:"eventType"`
	EventState   string `xml:"eventState"`
}

// EffectiveChannel returns ChannelID or DynChannelID when ChannelID is zero.
func (a ISAPIAlert) EffectiveChannel() int {
	if a.ChannelID > 0 {
		return a.ChannelID
	}
	return a.DynChannelID
}

// IsMotionEvent reports whether the Hikvision eventType represents video motion / analytics.
func (a ISAPIAlert) IsMotionEvent() bool {
	switch strings.ToLower(strings.TrimSpace(a.EventType)) {
	case "vmd", "linedetection", "fielddetection", "shelteralarm", "scenechangedetection":
		return true
	default:
		return false
	}
}

type isapiBinding struct {
	camID   string
	emit    func(Event)
	tracker *StatsTracker
}

// ISAPIHub shares one HTTP alertStream connection per Hikvision DVR across all
// channels mapped to that DVR.
type ISAPIHub struct {
	dvr      config.HikvisionDVRConfig
	logger   *log.Logger
	client   *http.Client
	digestNC uint32

	mu            sync.RWMutex
	channels      map[int]isapiBinding
	strictCount   int
	failedOnce    sync.Once
	failedCh      chan struct{}
}

// NewISAPIHub creates a Hikvision ISAPI alertStream hub for a DVR.
func NewISAPIHub(dvr config.HikvisionDVRConfig, logger *log.Logger) *ISAPIHub {
	if logger == nil {
		logger = log.Default()
	}
	return &ISAPIHub{
		dvr:      dvr,
		logger:   logger,
		channels: make(map[int]isapiBinding),
		failedCh: make(chan struct{}),
		client: &http.Client{
			// No overall request Timeout because alertStream is an infinite streaming response.
			Transport: &http.Transport{
				ResponseHeaderTimeout: 10 * time.Second,
				TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// Failed returns a channel closed once the DVR ISAPI endpoint fails fatally or repeatedly pre-connect.
func (h *ISAPIHub) Failed() <-chan struct{} {
	return h.failedCh
}

func (h *ISAPIHub) markFailed() {
	h.failedOnce.Do(func() {
		close(h.failedCh)
	})
}

func isFatalISAPIError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "http 404") ||
		strings.Contains(msg, "http 405") ||
		strings.Contains(msg, "http 501") ||
		strings.Contains(msg, "unsupported auth challenge")
}

// ChannelDetector returns a per-camera strict ISAPI Detector view backed by this hub.
func (h *ISAPIHub) ChannelDetector(camID string, channel int) Detector {
	return h.channelDetector(camID, channel, true)
}

func (h *ISAPIHub) channelDetector(camID string, channel int, strict bool) Detector {
	tracker := NewStatsTracker(config.MotionModeHikvisionISAPI)
	h.mu.Lock()
	h.channels[channel] = isapiBinding{
		camID:   camID,
		tracker: tracker,
	}
	if strict {
		h.strictCount++
	}
	h.mu.Unlock()
	return &isapiChannelDetector{
		hub:     h,
		camID:   camID,
		channel: channel,
		tracker: tracker,
	}
}

func (h *ISAPIHub) hasStrictChannels() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.strictCount > 0
}

type isapiChannelDetector struct {
	hub     *ISAPIHub
	camID   string
	channel int
	tracker *StatsTracker
}

func (d *isapiChannelDetector) Run(ctx context.Context, emit func(Event)) {
	d.hub.mu.Lock()
	b := d.hub.channels[d.channel]
	b.emit = emit
	d.hub.channels[d.channel] = b
	d.hub.mu.Unlock()
	<-ctx.Done()
}

func (d *isapiChannelDetector) Stats() Stats {
	return d.tracker.Snapshot()
}

// Run maintains the alertStream HTTP connection to the DVR until ctx is canceled.
func (h *ISAPIHub) Run(ctx context.Context) {
	backoff := 2 * time.Second
	everConnected := false
	consecFailures := 0

	for {
		if ctx.Err() != nil {
			h.setAllConnected(false, "")
			return
		}

		start := time.Now()
		connected, err := h.streamOnce(ctx)
		if ctx.Err() != nil {
			h.setAllConnected(false, "")
			return
		}

		if connected {
			everConnected = true
			consecFailures = 0
		} else {
			consecFailures++
			if !everConnected && (isFatalISAPIError(err) || consecFailures >= 3) {
				h.markFailed()
				if !h.hasStrictChannels() {
					h.setAllConnected(false, err.Error())
					h.logger.Printf("[motion][dvr:%s] ISAPI unavailable (%v); stopping ISAPI poller (channels switched to pixel fallback)", h.dvr.ID, err)
					return
				}
			}
		}

		h.setAllConnected(false, err.Error())
		if time.Since(start) > 30*time.Second {
			backoff = 2 * time.Second
		}
		h.logger.Printf("[motion][dvr:%s] ISAPI alertStream disconnected (%v); retrying in %s", h.dvr.ID, err, backoff)

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

func (h *ISAPIHub) alertStreamURL() string {
	scheme := "http"
	if h.dvr.UseHTTPS {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s:%d/ISAPI/Event/notification/alertStream", scheme, h.dvr.Host, h.dvr.Port)
}

func (h *ISAPIHub) streamOnce(ctx context.Context) (bool, error) {
	endpoint := h.alertStreamURL()

	openStream := func(authHeader string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		return h.client.Do(req)
	}

	resp, err := openStream("")
	if err != nil {
		return false, err
	}
	if resp.StatusCode == http.StatusUnauthorized && h.dvr.Username != "" {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		authHdr := buildHTTPAuthHeader(wwwAuth, http.MethodGet, endpoint, h.dvr.Username, h.dvr.Password, &h.digestNC)
		if authHdr == "" {
			return false, fmt.Errorf("DVR %s returned 401 with unsupported auth challenge %q", h.dvr.ID, wwwAuth)
		}
		resp, err = openStream(authHdr)
		if err != nil {
			return false, err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("HTTP %d from %s", resp.StatusCode, endpoint)
	}

	h.setAllConnected(true, "")
	h.logger.Printf("[motion][dvr:%s] ISAPI alertStream connected (%s)", h.dvr.ID, endpoint)

	streamErr := parseISAPIAlertStream(resp.Body, func(alert ISAPIAlert) {
		if !alert.IsMotionEvent() {
			return
		}
		ch := alert.EffectiveChannel()
		active := strings.EqualFold(strings.TrimSpace(alert.EventState), "active")

		h.mu.RLock()
		binding, ok := h.channels[ch]
		h.mu.RUnlock()
		if !ok {
			return
		}

		score := 0.0
		if active {
			score = 100.0
		}
		ev := Event{
			CameraID: binding.camID,
			Active:   active,
			Source:   "isapi",
			Score:    score,
			Time:     time.Now(),
		}
		binding.tracker.RecordEvent(ev)
		if binding.emit != nil {
			binding.emit(ev)
		}
	})
	return true, streamErr
}

// ISAPIOrPixelDetector tries Hikvision DVR ISAPI alertStream events first and
// automatically falls back to sub-stream pixel detection if the DVR ISAPI endpoint fails.
type ISAPIOrPixelDetector struct {
	camID  string
	hub    *ISAPIHub
	isapi  Detector
	pixel  *PixelDetector
	logger *log.Logger

	mu     sync.RWMutex
	active Detector
}

// NewISAPIOrPixelDetector builds a hybrid DVR ISAPI + sub-stream Pixel detector.
func NewISAPIOrPixelDetector(
	camID string,
	hub *ISAPIHub,
	channel int,
	subRTSP, ffmpegBin string,
	cfg config.MotionConfig,
	job *procjob.Manager,
	logger *log.Logger,
) *ISAPIOrPixelDetector {
	if logger == nil {
		logger = log.Default()
	}
	isapiDet := hub.channelDetector(camID, channel, false)
	var pix *PixelDetector
	if ffmpegBin != "" && subRTSP != "" {
		pix = NewPixelDetector(camID, subRTSP, ffmpegBin, cfg, job, logger)
	}
	return &ISAPIOrPixelDetector{
		camID:  camID,
		hub:    hub,
		isapi:  isapiDet,
		pixel:  pix,
		logger: logger,
		active: isapiDet,
	}
}

// Stats returns the stats of whichever detector (ISAPI or Pixel) is currently active.
func (d *ISAPIOrPixelDetector) Stats() Stats {
	d.mu.RLock()
	cur := d.active
	d.mu.RUnlock()
	return cur.Stats()
}

// Run monitors the ISAPI hub; if the hub reports failure and pixel fallback is configured,
// it switches to PixelDetector for the remainder of ctx.
func (d *ISAPIOrPixelDetector) Run(ctx context.Context, emit func(Event)) {
	isapiCtx, cancelISAPI := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.isapi.Run(isapiCtx, emit)
	}()

	select {
	case <-ctx.Done():
		cancelISAPI()
		<-done
		return
	case <-d.hub.Failed():
		cancelISAPI()
		<-done
		if ctx.Err() != nil || d.pixel == nil {
			return
		}
		d.logger.Printf("[motion][%s] Hikvision ISAPI unavailable; falling back to sub-stream pixel motion", d.camID)
		d.mu.Lock()
		d.active = d.pixel
		d.mu.Unlock()
		d.pixel.Run(ctx, emit)
	}
}

func (h *ISAPIHub) setAllConnected(connected bool, errMsg string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, b := range h.channels {
		b.tracker.SetConnected(connected, errMsg)
	}
}

// parseISAPIAlertStream extracts <EventNotificationAlert>...</EventNotificationAlert> XML blocks
// across Hikvision multipart/mixed chunks regardless of boundary or CRLF quirks.
func parseISAPIAlertStream(r io.Reader, onAlert func(ISAPIAlert)) error {
	const (
		startTag = "<EventNotificationAlert"
		endTag   = "</EventNotificationAlert>"
		maxBuf   = 256 * 1024
	)

	br := bufio.NewReaderSize(r, 16*1024)
	var acc bytes.Buffer

	chunk := make([]byte, 8192)
	for {
		n, err := br.Read(chunk)
		if n > 0 {
			acc.Write(chunk[:n])
			for {
				data := acc.Bytes()
				sIdx := bytes.Index(data, []byte(startTag))
				if sIdx < 0 {
					// Keep a small tail in case startTag is split across reads.
					if len(data) > len(startTag) {
						acc.Next(len(data) - len(startTag))
					}
					break
				}
				if sIdx > 0 {
					acc.Next(sIdx)
					data = acc.Bytes()
				}
				eIdx := bytes.Index(data, []byte(endTag))
				if eIdx < 0 {
					if len(data) > maxBuf {
						acc.Reset()
					}
					break
				}
				blockEnd := eIdx + len(endTag)
				xmlBlock := append([]byte(nil), data[:blockEnd]...)
				acc.Next(blockEnd)

				var alert ISAPIAlert
				if xmlErr := xml.Unmarshal(xmlBlock, &alert); xmlErr == nil {
					onAlert(alert)
				}
			}
		}
		if err != nil {
			return err
		}
	}
}
