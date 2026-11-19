package motion

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"feathernvr/internal/config"
)

func makeGrayFrame(w, h int, bg uint8) []byte {
	f := make([]byte, w*h)
	for i := range f {
		f[i] = bg
	}
	return f
}

func drawRect(f []byte, w, h, x1, y1, x2, y2 int, val uint8) {
	for y := y1; y < y2 && y < h; y++ {
		for x := x1; x < x2 && x < w; x++ {
			f[y*w+x] = val
		}
	}
}

func TestPixelDiffEngine(t *testing.T) {
	cfg := config.MotionConfig{
		Width:          64,
		Height:         48,
		Threshold:      25,
		MinAreaPercent: 1.0,
		MaxAreaPercent: 80.0,
		MinFrames:      2,
		IgnoreZones:    [][4]float64{{0.0, 0.0, 1.0, 0.25}}, // top 25% ignored
	}

	eng := NewPixelDiffEngine(cfg)
	bg := makeGrayFrame(64, 48, 30)

	// Frame 1 initializes background.
	trig, _ := eng.ProcessFrame(bg)
	if trig {
		t.Fatalf("first frame must not trigger")
	}

	// Static frame must not trigger.
	trig, pct := eng.ProcessFrame(bg)
	if trig || pct != 0 {
		t.Fatalf("static frame triggered=%v pct=%v", trig, pct)
	}

	// Change only inside ignored zone (rows 0..10): must not trigger.
	ignoredMove := makeGrayFrame(64, 48, 30)
	drawRect(ignoredMove, 64, 48, 5, 1, 55, 10, 220)
	for i := 0; i < 3; i++ {
		trig, pct = eng.ProcessFrame(ignoredMove)
		if trig || pct != 0 {
			t.Fatalf("ignored zone change triggered=%v pct=%v", trig, pct)
		}
	}

	// Real moving block outside ignore zone across minFrames=2 frames.
	move1 := makeGrayFrame(64, 48, 30)
	drawRect(move1, 64, 48, 20, 20, 35, 35, 220)
	trig1, pct1 := eng.ProcessFrame(move1)
	if trig1 || pct1 < 5.0 {
		t.Fatalf("frame 1 of motion should not trigger yet (min_frames=2), trig=%v pct=%v", trig1, pct1)
	}
	trig2, pct2 := eng.ProcessFrame(move1)
	if !trig2 || pct2 < 5.0 {
		t.Fatalf("frame 2 of motion should trigger, trig=%v pct=%v", trig2, pct2)
	}

	// Full-frame IR flash (>80% changed) must be rejected.
	flash := makeGrayFrame(64, 48, 240)
	trigFlash, pctFlash := eng.ProcessFrame(flash)
	if trigFlash || pctFlash <= 80.0 {
		t.Fatalf("IR flash should be rejected (>80%%), trig=%v pct=%v", trigFlash, pctFlash)
	}
}

func TestONVIFHelpersAndParsers(t *testing.T) {
	// Test WS-Security PasswordDigest math against explicit SHA1 computation.
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	offset := 30 * time.Second
	hdr := buildWSSecurityHeader("admin", "pass123", offset, nonce, now)

	expectedCreated := "2026-11-19T12:00:30Z"
	sum := sha1.Sum(append(append(append([]byte(nil), nonce...), []byte(expectedCreated)...), []byte("pass123")...))
	expectedDigest := base64.StdEncoding.EncodeToString(sum[:])

	if !strings.Contains(hdr, expectedDigest) || !strings.Contains(hdr, expectedCreated) {
		t.Fatalf("unexpected WS-Security header: %s", hdr)
	}

	// Test GetSystemDateAndTime parser.
	dtXML := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
  <s:Body>
    <tds:GetSystemDateAndTimeResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">
      <tds:SystemDateAndTime>
        <tt:UTCDateTime>
          <tt:Time><tt:Hour>14</tt:Hour><tt:Minute>25</tt:Minute><tt:Second>36</tt:Second></tt:Time>
          <tt:Date><tt:Year>2026</tt:Year><tt:Month>11</tt:Month><tt:Day>19</tt:Day></tt:Date>
        </tt:UTCDateTime>
      </tds:SystemDateAndTime>
    </tds:GetSystemDateAndTimeResponse>
  </s:Body>
</s:Envelope>`
	parsedTime, err := parseSystemDateAndTime([]byte(dtXML))
	if err != nil {
		t.Fatalf("parseSystemDateAndTime failed: %v", err)
	}
	wantTime := time.Date(2026, 11, 19, 14, 25, 36, 0, time.UTC)
	if !parsedTime.Equal(wantTime) {
		t.Fatalf("got %v, want %v", parsedTime, wantTime)
	}

	// Test PullMessagesResponse parser (both Hikvision CellMotionDetector and Keview MotionAlarm).
	pullXML := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2" xmlns:tt="http://www.onvif.org/ver10/schema" xmlns:tev="http://www.onvif.org/ver10/events/wsdl">
  <s:Body>
    <tev:PullMessagesResponse>
      <wsnt:NotificationMessage>
        <wsnt:Topic Dialect="http://www.onvif.org/ver10/tev/topicExpression/ConcreteSet">tns1:RuleEngine/CellMotionDetector/Motion</wsnt:Topic>
        <wsnt:Message><tt:Message><tt:Data><tt:SimpleItem Name="IsMotion" Value="true"/></tt:Data></tt:Message></wsnt:Message>
      </wsnt:NotificationMessage>
      <wsnt:NotificationMessage>
        <wsnt:Topic>tns1:VideoSource/MotionAlarm</wsnt:Topic>
        <wsnt:Message><tt:Message><tt:Data><tt:SimpleItem Name="State" Value="false"/></tt:Data></tt:Message></wsnt:Message>
      </wsnt:NotificationMessage>
    </tev:PullMessagesResponse>
  </s:Body>
</s:Envelope>`
	sigs, err := parsePullMessagesEvents([]byte(pullXML))
	if err != nil {
		t.Fatalf("parsePullMessagesEvents failed: %v", err)
	}
	if len(sigs) != 2 || !sigs[0].Active || sigs[1].Active {
		t.Fatalf("unexpected parsed ONVIF signals: %+v", sigs)
	}

	// Test loopback SubscriptionReference host rewriting.
	base, _ := url.Parse("http://192.168.1.77:8899/onvif/device_service")
	rewritten := rewriteURLHost("http://127.0.0.1:8899/onvif/Subscription?id=42", base)
	if rewritten != "http://192.168.1.77:8899/onvif/Subscription?id=42" {
		t.Fatalf("unexpected rewritten URL: %q", rewritten)
	}
	rewrittenZero := rewriteURLHost("http://0.0.0.0/onvif/event_service", base)
	if rewrittenZero != "http://192.168.1.77:8899/onvif/event_service" {
		t.Fatalf("unexpected rewritten 0.0.0.0 URL: %q", rewrittenZero)
	}
}

func TestISAPIAlertStreamParser(t *testing.T) {
	rawStream := "--boundary\r\nContent-Type: application/xml\r\n\r\n" +
		`<EventNotificationAlert version="2.0" xmlns="http://www.hikvision.com/ver20/XMLSchema">
<channelID>2</channelID>
<eventType>VMD</eventType>
<eventState>active</eventState>
</EventNotificationAlert>` +
		"\r\n--boundary\r\nContent-Type: text/xml\r\n\r\n" +
		`<EventNotificationAlert version="2.0">
<dynChannelID>5</dynChannelID>
<eventType>linedetection</eventType>
<eventState>inactive</eventState>
</EventNotificationAlert>` +
		"\r\n--boundary--\r\n"

	var alerts []ISAPIAlert
	err := parseISAPIAlertStream(strings.NewReader(rawStream), func(a ISAPIAlert) {
		alerts = append(alerts, a)
	})
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected parser error: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(alerts))
	}
	if alerts[0].EffectiveChannel() != 2 || alerts[0].EventType != "VMD" || alerts[0].EventState != "active" || !alerts[0].IsMotionEvent() {
		t.Fatalf("unexpected alert 0: %+v", alerts[0])
	}
	if alerts[1].EffectiveChannel() != 5 || alerts[1].EventType != "linedetection" || alerts[1].EventState != "inactive" {
		t.Fatalf("unexpected alert 1: %+v", alerts[1])
	}
}

func TestISAPIOrPixelFallbackOnHubFailure(t *testing.T) {
	hub := NewISAPIHub(config.HikvisionDVRConfig{ID: "dvr1", Host: "127.0.0.1", Port: 80}, nil)
	det := NewISAPIOrPixelDetector("cam1", hub, 1, "rtsp://127.0.0.1/cam1_sub", "nonexistent-ffmpeg-binary-for-test", config.MotionConfig{}, nil, nil)

	if st := det.Stats(); st.Mode != config.MotionModeHikvisionISAPI {
		t.Fatalf("expected initial mode hikvision_isapi, got %q", st.Mode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		det.Run(ctx, func(Event) {})
	}()

	hub.markFailed()

	deadline := time.Now().Add(2 * time.Second)
	switched := false
	for time.Now().Before(deadline) {
		if det.Stats().Mode == config.MotionModePixel {
			switched = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if !switched {
		t.Fatalf("expected ISAPIOrPixelDetector to switch to pixel mode on hub failure, mode=%q", det.Stats().Mode)
	}
}

func TestParseCapabilitiesEventsWSPullPointSupportFalse(t *testing.T) {
	capsXML := `<?xml version="1.0" encoding="UTF-8"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope" xmlns:tt="http://www.onvif.org/ver10/schema">
  <SOAP-ENV:Body>
    <tds:GetCapabilitiesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
      <tds:Capabilities>
        <tt:Events>
          <tt:XAddr>http://192.0.2.40:80/onvif/Events</tt:XAddr>
          <tt:WSSubscriptionPolicySupport>true</tt:WSSubscriptionPolicySupport>
          <tt:WSPullPointSupport>false</tt:WSPullPointSupport>
        </tt:Events>
      </tds:Capabilities>
    </tds:GetCapabilitiesResponse>
  </SOAP-ENV:Body>
</SOAP-ENV:Envelope>`

	xaddr, pullSupport := parseCapabilitiesEvents([]byte(capsXML))
	if xaddr != "http://192.0.2.40:80/onvif/Events" {
		t.Fatalf("unexpected events xaddr: %q", xaddr)
	}
	if pullSupport == nil || *pullSupport {
		t.Fatalf("expected pullSupport=false, got %v", pullSupport)
	}
}

func TestISAPIHubStopsOn401WhenOnlyFallbackChannels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Digest realm="test", nonce="abc", qop="auth"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	hub := NewISAPIHub(config.HikvisionDVRConfig{
		ID:       "dvr1",
		Host:     u.Hostname(),
		Port:     port,
		Username: "admin",
		Password: "bad",
	}, nil)

	_ = NewISAPIOrPixelDetector("cam1", hub, 1, "rtsp://127.0.0.1/sub", "", config.MotionConfig{}, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		hub.Run(ctx)
	}()

	select {
	case <-hubDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected ISAPIHub.Run to exit cleanly after HTTP 401 when only fallback channels exist")
	}
}
