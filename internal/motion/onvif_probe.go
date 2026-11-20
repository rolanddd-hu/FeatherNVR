package motion

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"feathernvr/internal/config"
)

// ProbeRequest specifies target host/URL and optional credentials for camera auto-detection.
type ProbeRequest struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// ProbeProfile describes one detected ONVIF media profile and its credential-free RTSP URI.
type ProbeProfile struct {
	Token   string `json:"token"`
	Name    string `json:"name"`
	Codec   string `json:"codec"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FPS     int    `json:"fps"`
	RTSPURL string `json:"rtsp_url"`
}

// ProbeResult contains auto-detected camera metadata, stream URLs, motion suggestion, and PTZ capabilities.
type ProbeResult struct {
	Name                string         `json:"name,omitempty"`
	Manufacturer        string         `json:"manufacturer,omitempty"`
	Model               string         `json:"model,omitempty"`
	ONVIFURL            string         `json:"onvif_url,omitempty"`
	PullPointSupported  *bool          `json:"pull_point_supported,omitempty"`
	SuggestedMotionMode string         `json:"suggested_motion_mode"`
	Profiles            []ProbeProfile `json:"profiles"`
	MainRTSP            string         `json:"main_rtsp,omitempty"`
	SubRTSP             string         `json:"sub_rtsp,omitempty"`
	PTZEnabled          bool           `json:"ptz_enabled"`
	PTZType             string         `json:"ptz_type,omitempty"`
	PTZURL              string         `json:"ptz_url,omitempty"`
	PTZProfileToken     string         `json:"ptz_profile_token,omitempty"`
	PTZChannel          int            `json:"ptz_channel,omitempty"`
}

// ProbeCamera connects to candidate ONVIF (and Hikvision ISAPI) endpoints on req.Host to discover
// streams, resolutions, codecs, motion PullPoint support, and PTZ capabilities.
func ProbeCamera(ctx context.Context, req ProbeRequest) (*ProbeResult, error) {
	candidates, err := buildCandidateONVIFURLs(req.Host, req.Port)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, endpoint := range candidates {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		res, probeErr := probeONVIFEndpoint(ctx, endpoint, req.Username, req.Password)
		if probeErr == nil {
			return res, nil
		}
		lastErr = probeErr
		// Stop probing further ports immediately if the endpoint rejected credentials.
		if strings.Contains(probeErr.Error(), "HTTP 401") || strings.Contains(strings.ToLower(probeErr.Error()), "not authorized") {
			return nil, probeErr
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no ONVIF service responded on host")
}

func buildCandidateONVIFURLs(rawHost string, port int) ([]string, error) {
	rawHost = strings.TrimSpace(rawHost)
	if rawHost == "" {
		return nil, errors.New("host or ONVIF URL is required")
	}

	lower := strings.ToLower(rawHost)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		u, err := url.Parse(rawHost)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("invalid URL %q", rawHost)
		}
		var urls []string
		if u.Path != "" && u.Path != "/" {
			urls = append(urls, u.String())
		}
		uSvc := *u
		uSvc.Path = "/onvif/device_service"
		uSvc.RawQuery = ""
		if uSvc.Port() == "" && port > 0 {
			uSvc.Host = net.JoinHostPort(uSvc.Hostname(), strconv.Itoa(port))
		}
		sSvc := uSvc.String()
		if len(urls) == 0 || urls[0] != sSvc {
			urls = append(urls, sSvc)
		}
		return urls, nil
	}

	cleanHost := rawHost
	explicitPort := port
	if h, pStr, err := net.SplitHostPort(rawHost); err == nil {
		cleanHost = h
		if explicitPort <= 0 {
			if p, pErr := strconv.Atoi(pStr); pErr == nil && p > 0 {
				explicitPort = p
			}
		}
	}
	cleanHost = strings.Trim(cleanHost, "[]")
	if cleanHost == "" {
		return nil, fmt.Errorf("invalid host %q", rawHost)
	}

	var ports []int
	seenPort := make(map[int]struct{})
	addPort := func(p int) {
		if p <= 0 || p > 65535 {
			return
		}
		if _, ok := seenPort[p]; !ok {
			seenPort[p] = struct{}{}
			ports = append(ports, p)
		}
	}
	if explicitPort > 0 {
		addPort(explicitPort)
	}
	for _, p := range []int{80, 8899, 8000, 8080} {
		addPort(p)
	}

	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, fmt.Sprintf("http://%s/onvif/device_service", net.JoinHostPort(cleanHost, strconv.Itoa(p))))
	}
	return out, nil
}

func probeONVIFEndpoint(ctx context.Context, endpoint, username, password string) (*ProbeResult, error) {
	baseURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}

	d := NewONVIFDetector("probe", endpoint, username, password, nil)
	d.client = &http.Client{
		Timeout: 6 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	clockOffset := d.fetchClockOffset(ctx)

	capsBody := soapWithSecurity(username, password, clockOffset, `<tds:GetCapabilities><tds:Category>All</tds:Category></tds:GetCapabilities>`)
	capCtx, capCancel := context.WithTimeout(ctx, 5*time.Second)
	capsResp, err := d.doSOAP(capCtx, endpoint, "http://www.onvif.org/ver10/device/wsdl/GetCapabilities", capsBody, true)
	capCancel()
	if err != nil {
		return nil, err
	}
	if fault := extractSOAPFault(capsResp); fault != "" {
		return nil, errors.New(fault)
	}

	mediaURL, eventsURL, ptzXAddr, pullSupport := parseCapabilitiesAll(capsResp)
	if mediaURL != "" {
		mediaURL = rewriteURLHost(mediaURL, baseURL)
	} else {
		mediaURL = endpoint
	}
	if eventsURL != "" {
		_ = rewriteURLHost(eventsURL, baseURL)
	}
	if ptzXAddr != "" {
		ptzXAddr = rewriteURLHost(ptzXAddr, baseURL)
	}

	res := &ProbeResult{
		ONVIFURL:            endpoint,
		PullPointSupported:  pullSupport,
		SuggestedMotionMode: config.MotionModeONVIFOrPixel,
		Profiles:            []ProbeProfile{},
	}
	if pullSupport != nil && !*pullSupport {
		res.SuggestedMotionMode = config.MotionModePixel
	}

	// Best-effort GetDeviceInformation
	infoBody := soapWithSecurity(username, password, clockOffset, `<tds:GetDeviceInformation/>`)
	infoCtx, infoCancel := context.WithTimeout(ctx, 4*time.Second)
	if infoResp, infoErr := d.doSOAP(infoCtx, endpoint, "http://www.onvif.org/ver10/device/wsdl/GetDeviceInformation", infoBody, true); infoErr == nil {
		if extractSOAPFault(infoResp) == "" {
			res.Manufacturer, res.Model = parseDeviceInformation(infoResp)
			name := strings.TrimSpace(res.Manufacturer + " " + res.Model)
			if name != "" {
				res.Name = name
			}
		}
	}
	infoCancel()

	// Fetch ONVIF Media profiles
	profBody := soapWithSecurity(username, password, clockOffset, `<trt:GetProfiles/>`)
	profCtx, profCancel := context.WithTimeout(ctx, 5*time.Second)
	profResp, profErr := d.doSOAP(profCtx, mediaURL, "http://www.onvif.org/ver10/media/wsdl/GetProfiles", profBody, true)
	profCancel()
	if profErr == nil && extractSOAPFault(profResp) == "" {
		profiles := parseMediaProfiles(profResp)
		limit := len(profiles)
		if limit > 4 {
			limit = 4
		}
		for i := 0; i < limit; i++ {
			streamReq := fmt.Sprintf(`<trt:GetStreamUri>
      <trt:StreamSetup>
        <tt:Stream>RTP-Unicast</tt:Stream>
        <tt:Transport><tt:Protocol>RTSP</tt:Protocol></tt:Transport>
      </trt:StreamSetup>
      <trt:ProfileToken>%s</trt:ProfileToken>
    </trt:GetStreamUri>`, xmlEscape(profiles[i].Token))
			uriBody := soapWithSecurity(username, password, clockOffset, streamReq)
			uCtx, uCancel := context.WithTimeout(ctx, 4*time.Second)
			uriResp, uErr := d.doSOAP(uCtx, mediaURL, "http://www.onvif.org/ver10/media/wsdl/GetStreamUri", uriBody, true)
			uCancel()
			if uErr == nil && extractSOAPFault(uriResp) == "" {
				if rawURI := parseStreamURI(uriResp); rawURI != "" {
					profiles[i].RTSPURL = stripURLCredentials(rewriteURLHost(rawURI, baseURL))
				}
			}
		}

		// Sort largest resolution first, preferring H.264 on ties.
		sort.SliceStable(profiles, func(i, j int) bool {
			ai := profiles[i].Width * profiles[i].Height
			aj := profiles[j].Width * profiles[j].Height
			if ai != aj {
				return ai > aj
			}
			hi := strings.EqualFold(profiles[i].Codec, "H264")
			hj := strings.EqualFold(profiles[j].Codec, "H264")
			if hi != hj {
				return hi
			}
			return profiles[i].FPS > profiles[j].FPS
		})

		res.Profiles = profiles
		for _, p := range profiles {
			if p.RTSPURL != "" {
				res.MainRTSP = p.RTSPURL
				break
			}
		}
		if res.MainRTSP != "" {
			for _, p := range profiles {
				if p.RTSPURL != "" && p.RTSPURL != res.MainRTSP && strings.EqualFold(p.Codec, "H264") {
					res.SubRTSP = p.RTSPURL
					break
				}
			}
			if res.SubRTSP == "" {
				for _, p := range profiles {
					if p.RTSPURL != "" && p.RTSPURL != res.MainRTSP {
						res.SubRTSP = p.RTSPURL
						break
					}
				}
			}
		}
	}

	// Detect Hikvision ISAPI PTZ or fall back to ONVIF PTZ.
	isHik := strings.Contains(strings.ToLower(res.Manufacturer+" "+res.Model), "hikvision")
	httpBase := fmt.Sprintf("%s://%s", baseURL.Scheme, baseURL.Host)
	if (isHik || ptzXAddr == "") && probeHikvisionISAPIPTZ(ctx, d.client, httpBase, username, password) {
		res.PTZEnabled = true
		res.PTZType = "hikvision_isapi"
		res.PTZURL = httpBase
		res.PTZChannel = 1
	} else if ptzXAddr != "" {
		res.PTZEnabled = true
		res.PTZType = "onvif"
		res.PTZURL = ptzXAddr
		if len(res.Profiles) > 0 {
			res.PTZProfileToken = res.Profiles[0].Token
		}
	}

	return res, nil
}

func soapWithSecurity(username, password string, clockOffset time.Duration, bodyInner string) string {
	sec := buildWSSecurityHeader(username, password, clockOffset, nil, time.Now())
	hdr := ""
	if sec != "" {
		hdr = "<s:Header>" + sec + "</s:Header>"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
            xmlns:tds="http://www.onvif.org/ver10/device/wsdl"
            xmlns:trt="http://www.onvif.org/ver10/media/wsdl"
            xmlns:tt="http://www.onvif.org/ver10/schema">
  %s
  <s:Body>%s</s:Body>
</s:Envelope>`, hdr, bodyInner)
}

func parseCapabilitiesAll(xmlBytes []byte) (mediaXAddr, eventsXAddr, ptzXAddr string, pullPointSupported *bool) {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	section := ""
	depth := 0
	sectionDepth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			local := t.Name.Local
			if section == "" && (local == "Media" || local == "Events" || local == "PTZ") {
				section = local
				sectionDepth = depth
				continue
			}
			if section != "" && depth == sectionDepth+1 {
				switch local {
				case "XAddr":
					var txt string
					if dec.DecodeElement(&txt, &t) == nil {
						depth--
						val := strings.TrimSpace(txt)
						switch section {
						case "Media":
							if mediaXAddr == "" {
								mediaXAddr = val
							}
						case "Events":
							if eventsXAddr == "" {
								eventsXAddr = val
							}
						case "PTZ":
							if ptzXAddr == "" {
								ptzXAddr = val
							}
						}
					}
				case "WSPullPointSupport":
					if section == "Events" {
						var txt string
						if dec.DecodeElement(&txt, &t) == nil {
							depth--
							if b, ok := parseBoolValue(strings.ToLower(strings.TrimSpace(txt))); ok {
								v := b
								pullPointSupported = &v
							}
						}
					}
				}
			}
		case xml.EndElement:
			if section != "" && depth == sectionDepth && t.Name.Local == section {
				section = ""
				sectionDepth = 0
			}
			depth--
		}
	}
	return mediaXAddr, eventsXAddr, ptzXAddr, pullPointSupported
}

func parseDeviceInformation(xmlBytes []byte) (manufacturer, model string) {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "Manufacturer":
			var s string
			if dec.DecodeElement(&s, &se) == nil && manufacturer == "" {
				manufacturer = strings.TrimSpace(s)
			}
		case "Model":
			var s string
			if dec.DecodeElement(&s, &se) == nil && model == "" {
				model = strings.TrimSpace(s)
			}
		}
	}
	return manufacturer, model
}

func parseMediaProfiles(xmlBytes []byte) []ProbeProfile {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	var out []ProbeProfile
	var cur *ProbeProfile
	inVideoEnc := false
	inRes := false
	inRate := false

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Profiles":
				token := ""
				for _, a := range t.Attr {
					if a.Name.Local == "token" {
						token = strings.TrimSpace(a.Value)
						break
					}
				}
				cur = &ProbeProfile{Token: token}
				inVideoEnc, inRes, inRate = false, false, false
			case "Name":
				if cur != nil && !inVideoEnc && cur.Name == "" {
					var s string
					if dec.DecodeElement(&s, &t) == nil {
						cur.Name = strings.TrimSpace(s)
					}
				}
			case "VideoEncoderConfiguration":
				if cur != nil {
					inVideoEnc = true
				}
			case "Encoding":
				if cur != nil && inVideoEnc && cur.Codec == "" {
					var s string
					if dec.DecodeElement(&s, &t) == nil {
						cur.Codec = strings.TrimSpace(s)
					}
				}
			case "Resolution":
				if cur != nil && inVideoEnc {
					inRes = true
				}
			case "Width":
				if cur != nil && inRes {
					var s string
					if dec.DecodeElement(&s, &t) == nil {
						cur.Width, _ = strconv.Atoi(strings.TrimSpace(s))
					}
				}
			case "Height":
				if cur != nil && inRes {
					var s string
					if dec.DecodeElement(&s, &t) == nil {
						cur.Height, _ = strconv.Atoi(strings.TrimSpace(s))
					}
				}
			case "RateControl":
				if cur != nil && inVideoEnc {
					inRate = true
				}
			case "FrameRateLimit":
				if cur != nil && inRate {
					var s string
					if dec.DecodeElement(&s, &t) == nil {
						if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
							cur.FPS = int(f + 0.5)
						}
					}
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "Profiles":
				if cur != nil && cur.Token != "" {
					out = append(out, *cur)
				}
				cur = nil
				inVideoEnc, inRes, inRate = false, false, false
			case "VideoEncoderConfiguration":
				inVideoEnc = false
				inRes, inRate = false, false
			case "Resolution":
				inRes = false
			case "RateControl":
				inRate = false
			}
		}
	}
	return out
}

func parseStreamURI(xmlBytes []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local == "Uri" {
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func stripURLCredentials(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func probeHikvisionISAPIPTZ(ctx context.Context, client *http.Client, httpBase, username, password string) bool {
	if client == nil || httpBase == "" {
		return false
	}
	endpoint := strings.TrimRight(httpBase, "/") + "/ISAPI/PTZCtrl/channels/1/capabilities"
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var nc uint32
	doReq := func(authHeader string) (int, http.Header, error) {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			return 0, nil, err
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header, nil
	}

	status, hdr, err := doReq("")
	if err != nil {
		return false
	}
	if status == http.StatusUnauthorized && username != "" {
		if authHdr := buildHTTPAuthHeader(hdr.Get("WWW-Authenticate"), http.MethodGet, endpoint, username, password, &nc); authHdr != "" {
			status, _, err = doReq(authHdr)
			if err != nil {
				return false
			}
		}
	}
	return status == http.StatusOK
}
