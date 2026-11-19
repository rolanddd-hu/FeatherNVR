package ptz

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"feathernvr/internal/config"
)

func TestHikvisionPTZController(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var lastMoveBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Digest ") {
			w.Header().Set("WWW-Authenticate", `Digest realm="IP Camera", nonce="n123", qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/continuous") {
			lastMoveBody = string(body)
		}
		mu.Unlock()

		switch r.URL.Path {
		case "/ISAPI/PTZCtrl/channels/1/continuous":
			w.WriteHeader(http.StatusOK)
		case "/ISAPI/PTZCtrl/channels/1/presets":
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<PTZPresetList>
  <PTZPreset><enabled>true</enabled><id>1</id><presetName>Gate</presetName></PTZPreset>
  <PTZPreset><enabled>false</enabled><id>2</id><presetName>Hidden</presetName></PTZPreset>
  <PTZPreset><enabled>true</enabled><id>3</id><presetName>Driveway</presetName></PTZPreset>
</PTZPresetList>`)
		case "/ISAPI/PTZCtrl/channels/1/presets/3/goto":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctrl, err := NewController(config.PTZConfig{
		Enabled:  true,
		Type:     "hikvision_isapi",
		URL:      srv.URL,
		Username: "admin",
		Password: "secret",
		Channel:  1,
	})
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}

	ctx := context.Background()
	if err := ctrl.Move(ctx, 1.5, -0.5, 0.25); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if !strings.Contains(lastMoveBody, "<pan>100</pan><tilt>-50</tilt><zoom>25</zoom>") {
		t.Fatalf("unexpected move XML: %s", lastMoveBody)
	}

	if err := ctrl.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !strings.Contains(lastMoveBody, "<pan>0</pan><tilt>0</tilt><zoom>0</zoom>") {
		t.Fatalf("unexpected stop XML: %s", lastMoveBody)
	}

	presets, err := ctrl.ListPresets(ctx)
	if err != nil {
		t.Fatalf("ListPresets: %v", err)
	}
	if len(presets) != 2 || presets[0].ID != "1" || presets[0].Name != "Gate" || presets[1].ID != "3" {
		t.Fatalf("unexpected presets: %+v", presets)
	}

	if err := ctrl.GotoPreset(ctx, "3"); err != nil {
		t.Fatalf("GotoPreset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatalf("expected 4 authenticated calls, got %v", calls)
	}
}

func TestONVIFPTZController(t *testing.T) {
	var mu sync.Mutex
	var actions []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		body := string(bodyBytes)

		mu.Lock()
		switch {
		case strings.Contains(body, "GetSystemDateAndTime"):
			actions = append(actions, "GetSystemDateAndTime")
		case strings.Contains(body, "ContinuousMove"):
			actions = append(actions, "ContinuousMove:"+body)
		case strings.Contains(body, "<tptz:Stop"):
			actions = append(actions, "Stop")
		case strings.Contains(body, "GetPresets"):
			actions = append(actions, "GetPresets")
		case strings.Contains(body, "GotoPreset"):
			actions = append(actions, "GotoPreset:"+body)
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
		switch {
		case strings.Contains(body, "GetSystemDateAndTime"):
			_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><GetSystemDateAndTimeResponse><SystemDateAndTime><UTCDateTime><Time><Hour>12</Hour><Minute>0</Minute><Second>0</Second></Time><Date><Year>2026</Year><Month>11</Month><Day>19</Day></Date></UTCDateTime></SystemDateAndTime></GetSystemDateAndTimeResponse></s:Body></s:Envelope>`)
		case strings.Contains(body, "GetPresets"):
			_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body><tptz:GetPresetsResponse><tptz:Preset token="001"><tt:Name>Front Gate</tt:Name></tptz:Preset><tptz:Preset token="002"><tt:Name>Yard</tt:Name></tptz:Preset></tptz:GetPresetsResponse></s:Body></s:Envelope>`)
		default:
			_, _ = io.WriteString(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><OK/></s:Body></s:Envelope>`)
		}
	}))
	defer srv.Close()

	ctrl, err := NewController(config.PTZConfig{
		Enabled:      true,
		Type:         "onvif",
		URL:          srv.URL + "/onvif/Ptz",
		Username:     "admin",
		Password:     "pw",
		ProfileToken: "MainStreamToken",
	})
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}

	ctx := context.Background()
	if err := ctrl.Move(ctx, 0.5, 0.0, 0.0); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if err := ctrl.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	presets, err := ctrl.ListPresets(ctx)
	if err != nil {
		t.Fatalf("ListPresets: %v", err)
	}
	if len(presets) != 2 || presets[0].ID != "001" || presets[0].Name != "Front Gate" {
		t.Fatalf("unexpected presets: %+v", presets)
	}
	if err := ctrl.GotoPreset(ctx, "002"); err != nil {
		t.Fatalf("GotoPreset: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(actions) < 5 || !strings.Contains(actions[1], `x="0.500" y="0.000"`) || strings.Contains(actions[1], "<tt:Zoom") {
		t.Fatalf("unexpected ONVIF PTZ actions: %+v", actions)
	}
	if !strings.Contains(actions[4], "<tptz:PresetToken>002</tptz:PresetToken>") {
		t.Fatalf("unexpected GotoPreset body: %s", actions[4])
	}
}
