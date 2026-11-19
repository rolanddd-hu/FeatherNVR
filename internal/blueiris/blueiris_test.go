package blueiris

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"feathernvr/internal/config"
)

func toUTF16LEWithBOM(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	out := make([]byte, 2+len(u16)*2)
	out[0] = 0xFF
	out[1] = 0xFE
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(out[2+i*2:4+i*2], v)
	}
	return out
}

func TestParseRegBytesUTF16LEAndUTF8(t *testing.T) {
	regText := `Windows Registry Editor Version 5.00

[HKEY_LOCAL_MACHINE\SOFTWARE\Perspective Software\Blue Iris\Cameras\Café Patio]
"shortname"="Cam10"
"enabled"=dword:00000001
"ip"="192.0.2.10"
"ip_port"=dword:00000050
"ip_port2"=dword:0000022a
"ip_onvif"=dword:00000050
"ip_path"="/stream1"
"ptz_page"="/onvif/Ptz"
"profile_token"="MainStreamToken"
"ipid"="admin"
"ippw"="cGFzc0A6MS8j"
"ippwencode"=dword:00000001

[HKEY_LOCAL_MACHINE\SOFTWARE\Perspective Software\Blue Iris\Cameras\Café Patio\PTZ]
"enabled"=dword:00000001
"protocol"=dword:00000102

[HKEY_LOCAL_MACHINE\SOFTWARE\Perspective Software\Blue Iris\Cameras\Café Patio\Motion]
"ignored"=dword:00000001
`

	cams16, err := ParseRegBytes(toUTF16LEWithBOM(regText))
	if err != nil {
		t.Fatalf("ParseRegBytes UTF-16LE failed: %v", err)
	}
	cams8, err := ParseRegBytes([]byte(regText))
	if err != nil {
		t.Fatalf("ParseRegBytes UTF-8 failed: %v", err)
	}

	for _, cams := range [][]RawCamera{cams16, cams8} {
		if len(cams) != 1 {
			t.Fatalf("expected 1 camera (nested subkey ignored), got %d", len(cams))
		}
		c := cams[0]
		if c.KeyName != "Café Patio" || c.ShortName != "Cam10" || c.RTSPPort != 554 || c.Port != 80 {
			t.Fatalf("unexpected parsed camera: %+v", c)
		}
		if c.PTZEnabled != 1 || c.PTZProtocol != 258 || c.PTZPage != "/onvif/Ptz" || c.ProfileToken != "MainStreamToken" {
			t.Fatalf("unexpected parsed PTZ fields: %+v", c)
		}
	}
}

func TestConvertRealWorldBlueIrisShapes(t *testing.T) {
	pw := base64.StdEncoding.EncodeToString([]byte("p@ss:1/#"))
	raw := []RawCamera{
		{
			KeyName:         "Café Patio",
			ShortName:       "Cam10",
			Enabled:         1,
			IP:              "192.0.2.10",
			Port:            80,
			RTSPPort:        554,
			ONVIFPort:       80,
			Path:            "/stream1",
			Username:        "admin",
			PasswordRaw:     pw,
			PasswordEncoded: 1,
			ProfileToken:    "MainStreamToken",
			PTZEnabled:      1,
			PTZProtocol:     258,
			PTZPage:         "/onvif/Ptz",
		},
		{
			KeyName:         "IP DOME - PTZ",
			ShortName:       "PTZ",
			Enabled:         1,
			IP:              "192.0.2.20",
			Port:            80,
			RTSPPort:        554,
			ONVIFPort:       8999,
			Path:            "/",
			Username:        "admin",
			PasswordRaw:     pw,
			PasswordEncoded: 1,
			ProfileToken:    "Profile_1",
			ProfileToken2:   "Profile_2",
			PTZEnabled:      1,
			PTZProtocol:     324,
			PTZPage:         "/onvif/ptz_service",
		},
		{
			KeyName:         "My Camera 1",
			ShortName:       "Cam1",
			Enabled:         1,
			IP:              "198.51.100.10",
			HTTPS:           2, // Blue Iris uses 2 for RTSP transport; DVR HTTP ISAPI is still plain HTTP port 80
			Port:            80,
			RTSPPort:        554,
			ONVIFPort:       8999,
			Path:            "/Streaming/Channels/101",
			SubPath:         "198.51.100.10/Streaming/Channels/102",
			Username:        "admin",
			PasswordRaw:     "MTIzNDU=",
			PasswordEncoded: 1,
		},
		{
			KeyName:         "My Camera 7",
			ShortName:       "Cam7",
			Enabled:         1,
			IP:              "198.51.100.10",
			Port:            80,
			RTSPPort:        554,
			ONVIFPort:       8999,
			Path:            "/Streaming/Channels/401",
			Username:        "admin",
			PasswordRaw:     "MTIzNDU=",
			PasswordEncoded: 1,
		},
		{
			KeyName:         "My Camera 8",
			ShortName:       "Cam8",
			Enabled:         1,
			IP:              "192.0.2.30",
			Port:            80,
			RTSPPort:        554,
			ONVIFPort:       554, // should guard against RTSP port and fall back to Port=80
			Path:            "/",
			Username:        "admin",
			PasswordRaw:     "MTIzNDU=",
			PasswordEncoded: 1,
			ProfileToken:    "Profile_1",
		},
		{
			KeyName:   "My Camera 11",
			ShortName: "Cam11",
			Enabled:   0,
			IP:        "192.0.2.99",
		},
	}

	res, err := Convert(raw, ImportOptions{MaxGB: 50, IncludeDisabled: true})
	if err != nil {
		t.Fatalf("Convert failed: %v", err)
	}
	if res.EnabledCount != 5 || res.DisabledCount != 1 || res.DVRCount != 1 {
		t.Fatalf("unexpected counts: %+v", res)
	}
	cfg := res.Config
	if cfg.Storage.MaxGB != 50 {
		t.Fatalf("expected max_gb=50, got %v", cfg.Storage.MaxGB)
	}

	byName := make(map[string]config.CameraConfig)
	for _, c := range cfg.Cameras {
		byName[c.Name] = c
	}

	// Accented slugification + special password URL-escaping.
	patio := byName["Café Patio"]
	if patio.ID != "cafe_patio" {
		t.Fatalf("unexpected slug for Café Patio: %q", patio.ID)
	}
	if !strings.Contains(patio.MainRTSP, "p%40ss%3A1%2F%23@192.0.2.10:554/stream1") {
		t.Fatalf("unexpected RTSP URL escaping: %q", patio.MainRTSP)
	}
	if patio.Motion.Mode != config.MotionModeONVIFOrPixel || patio.Motion.ONVIFURL != "http://192.0.2.10:80/onvif/device_service" {
		t.Fatalf("unexpected patio motion: %+v", patio.Motion)
	}
	if !patio.PTZ.Enabled || patio.PTZ.Type != "onvif" || patio.PTZ.URL != "http://192.0.2.10:80/onvif/Ptz" || patio.PTZ.ProfileToken != "MainStreamToken" {
		t.Fatalf("unexpected patio PTZ: %+v", patio.PTZ)
	}

	// Direct RTSP "/" for PTZ + ONVIF placeholder port 8999 falling back to Port=80 + Hikvision ISAPI PTZ.
	ptz := byName["IP DOME - PTZ"]
	if !strings.HasPrefix(ptz.MainRTSP, "rtsp://") || !strings.HasSuffix(ptz.MainRTSP, "@192.0.2.20:554/") {
		t.Fatalf("unexpected PTZ main stream: %q", ptz.MainRTSP)
	}
	if ptz.Motion.ONVIFURL != "http://192.0.2.20:80/onvif/device_service" {
		t.Fatalf("expected PTZ ONVIF port fallback to 80, got %q", ptz.Motion.ONVIFURL)
	}
	if !ptz.PTZ.Enabled || ptz.PTZ.Type != "hikvision_isapi" || ptz.PTZ.URL != "http://192.0.2.20:80" || ptz.PTZ.Channel != 1 {
		t.Fatalf("unexpected Hikvision PTZ config: %+v", ptz.PTZ)
	}

	// Hikvision DVR channel grouping + https=2 stays HTTP + IP-prefixed subpath + auto-derived 402.
	if len(cfg.HikvisionDVRs) != 1 || cfg.HikvisionDVRs[0].UseHTTPS || cfg.HikvisionDVRs[0].Port != 80 {
		t.Fatalf("expected DVR UseHTTPS=false Port=80 when https=2, got %+v", cfg.HikvisionDVRs)
	}
	cam1 := byName["My Camera 1"]
	if cam1.SubRTSP != "rtsp://admin:12345@198.51.100.10:554/Streaming/Channels/102" {
		t.Fatalf("unexpected cam1 sub_rtsp: %q", cam1.SubRTSP)
	}
	if cam1.Motion.Mode != config.MotionModeISAPIOrPixel || cam1.Motion.Channel != 1 || cam1.Motion.DVR != "hikvision_dvr_1" {
		t.Fatalf("unexpected cam1 DVR motion: %+v", cam1.Motion)
	}
	cam7 := byName["My Camera 7"]
	if cam7.SubRTSP != "rtsp://admin:12345@198.51.100.10:554/Streaming/Channels/402" {
		t.Fatalf("expected derived 402 subpath for cam7, got %q", cam7.SubRTSP)
	}
	if cam7.Motion.Channel != 4 || cam7.Motion.DVR != "hikvision_dvr_1" {
		t.Fatalf("unexpected cam7 DVR motion: %+v", cam7.Motion)
	}

	// Direct RTSP "/" + ONVIF port 554 guard falls back to Port=80.
	cam8 := byName["My Camera 8"]
	if cam8.MainRTSP != "rtsp://admin:12345@192.0.2.30:554/" {
		t.Fatalf("unexpected cam8 main_rtsp: %q", cam8.MainRTSP)
	}
	if cam8.Motion.ONVIFURL != "http://192.0.2.30:80/onvif/device_service" {
		t.Fatalf("expected ONVIF port fallback to 80, got %q", cam8.Motion.ONVIFURL)
	}

	// Disabled camera preserved as enabled=false.
	cam11 := byName["My Camera 11"]
	if cam11.IsEnabled() {
		t.Fatalf("expected Cam11 to be disabled")
	}
}
