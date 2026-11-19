package ptz

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"feathernvr/internal/config"
)

// ONVIFController controls ONVIF Profile S / Profile T PTZ devices.
type ONVIFController struct {
	rawURL       string
	username     string
	password     string
	client       *http.Client
	digestNC     uint32

	mu           sync.Mutex
	resolved     bool
	ptzURL       string
	mediaURL     string
	deviceURL    string
	profileToken string
	clockSynced  bool
	clockOffset  time.Duration
}

// NewONVIFController creates an ONVIF PTZ controller.
func NewONVIFController(cfg config.PTZConfig) (*ONVIFController, error) {
	u := strings.TrimSpace(cfg.URL)
	if u == "" {
		return nil, errors.New("onvif ptz url cannot be empty")
	}
	return &ONVIFController{
		rawURL:       u,
		username:     cfg.Username,
		password:     cfg.Password,
		profileToken: strings.TrimSpace(cfg.ProfileToken),
		client: &http.Client{
			Timeout: 6 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}, nil
}

// Move executes an ONVIF ContinuousMove with velocities clamped to [-1.0, 1.0].
func (c *ONVIFController) Move(ctx context.Context, pan, tilt, zoom float64) error {
	ptzURL, token, offset, err := c.ensureReady(ctx)
	if err != nil {
		return err
	}
	pan = clampUnit(pan)
	tilt = clampUnit(tilt)
	zoom = clampUnit(zoom)

	var vel strings.Builder
	if pan != 0 || tilt != 0 || zoom == 0 {
		fmt.Fprintf(&vel, `<tt:PanTilt x="%.3f" y="%.3f"/>`, pan, tilt)
	}
	if zoom != 0 {
		fmt.Fprintf(&vel, `<tt:Zoom x="%.3f"/>`, zoom)
	}

	action := "http://www.onvif.org/ver20/ptz/wsdl/ContinuousMove"
	body := fmt.Sprintf(`<tptz:ContinuousMove>
      <tptz:ProfileToken>%s</tptz:ProfileToken>
      <tptz:Velocity>%s</tptz:Velocity>
    </tptz:ContinuousMove>`, xmlEscape(token), vel.String())

	resp, err := c.sendPTZSOAP(ctx, ptzURL, action, body, offset)
	if err != nil {
		return err
	}
	if fault := extractSOAPFault(resp); fault != "" {
		return fmt.Errorf("onvif ContinuousMove fault: %s", fault)
	}
	return nil
}

// Stop sends an ONVIF PTZ Stop command for PanTilt and Zoom.
func (c *ONVIFController) Stop(ctx context.Context) error {
	ptzURL, token, offset, err := c.ensureReady(ctx)
	if err != nil {
		return err
	}
	action := "http://www.onvif.org/ver20/ptz/wsdl/Stop"
	body := fmt.Sprintf(`<tptz:Stop>
      <tptz:ProfileToken>%s</tptz:ProfileToken>
      <tptz:PanTilt>true</tptz:PanTilt>
      <tptz:Zoom>true</tptz:Zoom>
    </tptz:Stop>`, xmlEscape(token))

	resp, err := c.sendPTZSOAP(ctx, ptzURL, action, body, offset)
	if err != nil {
		return err
	}
	if fault := extractSOAPFault(resp); fault != "" {
		return fmt.Errorf("onvif Stop fault: %s", fault)
	}
	return nil
}

// ListPresets retrieves saved ONVIF PTZ presets via GetPresets.
func (c *ONVIFController) ListPresets(ctx context.Context) ([]Preset, error) {
	ptzURL, token, offset, err := c.ensureReady(ctx)
	if err != nil {
		return nil, err
	}
	action := "http://www.onvif.org/ver20/ptz/wsdl/GetPresets"
	body := fmt.Sprintf(`<tptz:GetPresets>
      <tptz:ProfileToken>%s</tptz:ProfileToken>
    </tptz:GetPresets>`, xmlEscape(token))

	resp, err := c.sendPTZSOAP(ctx, ptzURL, action, body, offset)
	if err != nil {
		return nil, err
	}
	if fault := extractSOAPFault(resp); fault != "" {
		return nil, fmt.Errorf("onvif GetPresets fault: %s", fault)
	}
	return parseONVIFPresets(resp), nil
}

// GotoPreset moves the ONVIF camera to presetID.
func (c *ONVIFController) GotoPreset(ctx context.Context, presetID string) error {
	id := strings.TrimSpace(presetID)
	if id == "" {
		return errors.New("preset id cannot be empty")
	}
	ptzURL, token, offset, err := c.ensureReady(ctx)
	if err != nil {
		return err
	}
	action := "http://www.onvif.org/ver20/ptz/wsdl/GotoPreset"
	body := fmt.Sprintf(`<tptz:GotoPreset>
      <tptz:ProfileToken>%s</tptz:ProfileToken>
      <tptz:PresetToken>%s</tptz:PresetToken>
    </tptz:GotoPreset>`, xmlEscape(token), xmlEscape(id))

	resp, err := c.sendPTZSOAP(ctx, ptzURL, action, body, offset)
	if err != nil {
		return err
	}
	if fault := extractSOAPFault(resp); fault != "" {
		return fmt.Errorf("onvif GotoPreset fault: %s", fault)
	}
	return nil
}

func (c *ONVIFController) ensureReady(ctx context.Context) (ptzURL, token string, offset time.Duration, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.resolved && c.ptzURL != "" && c.profileToken != "" {
		return c.ptzURL, c.profileToken, c.clockOffset, nil
	}

	base, parseErr := url.Parse(c.rawURL)
	if parseErr != nil {
		return "", "", 0, fmt.Errorf("invalid onvif ptz url %q: %w", c.rawURL, parseErr)
	}

	if c.deviceURL == "" {
		dev := *base
		dev.Path = "/onvif/device_service"
		dev.RawQuery = ""
		c.deviceURL = dev.String()
	}

	if !c.clockSynced {
		c.clockOffset = c.fetchClockOffset(ctx, c.deviceURL)
		c.clockSynced = true
	}

	lowerPath := strings.ToLower(base.Path)
	if c.ptzURL == "" && (strings.HasSuffix(lowerPath, "/ptz") || strings.HasSuffix(lowerPath, "/ptz_service")) {
		c.ptzURL = c.rawURL
	}

	if c.ptzURL == "" || c.profileToken == "" {
		sec := buildWSSecurityHeader(c.username, c.password, c.clockOffset)
		hdr := ""
		if sec != "" {
			hdr = "<s:Header>" + sec + "</s:Header>"
		}
		capsBody := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
  %s
  <s:Body><tds:GetCapabilities><tds:Category>All</tds:Category></tds:GetCapabilities></s:Body>
</s:Envelope>`, hdr)

		capsResp, capsErr := c.doSOAP(ctx, c.deviceURL, "http://www.onvif.org/ver10/device/wsdl/GetCapabilities", capsBody, true)
		if capsErr == nil {
			ptzXAddr, mediaXAddr := parsePTZAndMediaXAddrs(capsResp)
			if c.ptzURL == "" && ptzXAddr != "" {
				c.ptzURL = rewriteURLHost(ptzXAddr, base)
			}
			if c.mediaURL == "" && mediaXAddr != "" {
				c.mediaURL = rewriteURLHost(mediaXAddr, base)
			}
		}
	}

	if c.ptzURL == "" {
		c.ptzURL = c.rawURL
	}

	if c.profileToken == "" {
		mediaEndpoint := c.mediaURL
		if mediaEndpoint == "" {
			mediaEndpoint = c.deviceURL
		}
		sec := buildWSSecurityHeader(c.username, c.password, c.clockOffset)
		hdr := ""
		if sec != "" {
			hdr = "<s:Header>" + sec + "</s:Header>"
		}
		profBody := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:trt="http://www.onvif.org/ver10/media/wsdl">
  %s
  <s:Body><trt:GetProfiles/></s:Body>
</s:Envelope>`, hdr)
		profResp, profErr := c.doSOAP(ctx, mediaEndpoint, "http://www.onvif.org/ver10/media/wsdl/GetProfiles", profBody, true)
		if profErr != nil {
			return "", "", 0, fmt.Errorf("resolve ONVIF profile token: %w", profErr)
		}
		c.profileToken = parseFirstProfileToken(profResp)
		if c.profileToken == "" {
			return "", "", 0, errors.New("ONVIF GetProfiles returned no profile token")
		}
	}

	c.resolved = true
	return c.ptzURL, c.profileToken, c.clockOffset, nil
}

func (c *ONVIFController) sendPTZSOAP(ctx context.Context, endpoint, action, innerBody string, offset time.Duration) ([]byte, error) {
	sec := buildWSSecurityHeader(c.username, c.password, offset)
	hdr := ""
	if sec != "" {
		hdr = "<s:Header>" + sec + "</s:Header>"
	}
	env := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
            xmlns:tptz="http://www.onvif.org/ver20/ptz/wsdl"
            xmlns:tt="http://www.onvif.org/ver10/schema">
  %s
  <s:Body>%s</s:Body>
</s:Envelope>`, hdr, innerBody)
	return c.doSOAP(ctx, endpoint, action, env, true)
}

func (c *ONVIFController) fetchClockOffset(ctx context.Context, deviceURL string) time.Duration {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
  <s:Body><tds:GetSystemDateAndTime/></s:Body>
</s:Envelope>`
	resp, err := c.doSOAP(reqCtx, deviceURL, "http://www.onvif.org/ver10/device/wsdl/GetSystemDateAndTime", body, false)
	if err != nil {
		return 0
	}
	camTime, err := parseSystemUTC(resp)
	if err != nil {
		return 0
	}
	return camTime.Sub(time.Now().UTC())
}

func (c *ONVIFController) doSOAP(ctx context.Context, endpoint, action, xmlBody string, allowHTTPAuth bool) ([]byte, error) {
	send := func(authHeader string) (int, http.Header, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(xmlBody))
		if err != nil {
			return 0, nil, nil, err
		}
		ct := "application/soap+xml; charset=utf-8"
		if action != "" {
			ct += fmt.Sprintf(`; action="%s"`, action)
		}
		req.Header.Set("Content-Type", ct)
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		return resp.StatusCode, resp.Header, b, readErr
	}

	status, hdr, body, err := send("")
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && allowHTTPAuth && c.username != "" {
		if authHdr := buildHTTPAuthHeader(hdr.Get("WWW-Authenticate"), http.MethodPost, endpoint, c.username, c.password, &c.digestNC); authHdr != "" {
			status, _, body, err = send(authHdr)
			if err != nil {
				return nil, err
			}
		}
	}
	if status >= 400 {
		if fault := extractSOAPFault(body); fault != "" {
			return nil, fmt.Errorf("HTTP %d (%s)", status, fault)
		}
		return nil, fmt.Errorf("HTTP %d from %s", status, endpoint)
	}
	return body, nil
}

func buildWSSecurityHeader(username, password string, offset time.Duration) string {
	if username == "" && password == "" {
		return ""
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	created := time.Now().UTC().Add(offset).Format("2006-01-02T15:04:05Z")

	h := sha1.New()
	_, _ = h.Write(nonce)
	_, _ = h.Write([]byte(created))
	_, _ = h.Write([]byte(password))
	digestB64 := base64.StdEncoding.EncodeToString(h.Sum(nil))
	nonceB64 := base64.StdEncoding.EncodeToString(nonce)

	return fmt.Sprintf(`<Security s:mustUnderstand="1" xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">
  <UsernameToken>
    <Username>%s</Username>
    <Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</Password>
    <Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">%s</Nonce>
    <Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">%s</Created>
  </UsernameToken>
</Security>`, xmlEscape(username), digestB64, nonceB64, created)
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func parsePTZAndMediaXAddrs(xmlBytes []byte) (ptzXAddr, mediaXAddr string) {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	section := ""
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "PTZ":
				section = "ptz"
			case "Media":
				section = "media"
			case "XAddr":
				if section != "" {
					var txt string
					if dec.DecodeElement(&txt, &t) == nil {
						if section == "ptz" && ptzXAddr == "" {
							ptzXAddr = strings.TrimSpace(txt)
						} else if section == "media" && mediaXAddr == "" {
							mediaXAddr = strings.TrimSpace(txt)
						}
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "PTZ" || t.Name.Local == "Media" {
				section = ""
			}
		}
	}
	return ptzXAddr, mediaXAddr
}

func parseFirstProfileToken(xmlBytes []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "Profiles" {
			for _, a := range se.Attr {
				if a.Name.Local == "token" && strings.TrimSpace(a.Value) != "" {
					return strings.TrimSpace(a.Value)
				}
			}
		}
	}
	return ""
}

func parseONVIFPresets(xmlBytes []byte) []Preset {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	var out []Preset
	inPreset := false
	curToken := ""
	curName := ""

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Preset":
				inPreset = true
				curToken = ""
				curName = ""
				for _, a := range t.Attr {
					if a.Name.Local == "token" {
						curToken = strings.TrimSpace(a.Value)
					}
				}
			case "Name":
				if inPreset {
					var txt string
					if dec.DecodeElement(&txt, &t) == nil {
						curName = strings.TrimSpace(txt)
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "Preset" && inPreset {
				inPreset = false
				if curToken != "" {
					name := curName
					if name == "" {
						name = "Preset " + curToken
					}
					out = append(out, Preset{ID: curToken, Name: name})
				}
			}
		}
	}
	return out
}

func parseSystemUTC(xmlBytes []byte) (time.Time, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	inUTC := false
	var year, month, day, hour, minute, second int
	gotHour := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "UTCDateTime":
				inUTC = true
			case "Year", "Month", "Day", "Hour", "Minute", "Second":
				if inUTC {
					var val string
					if dec.DecodeElement(&val, &t) == nil {
						n, _ := strconv.Atoi(strings.TrimSpace(val))
						switch t.Name.Local {
						case "Year":
							year = n
						case "Month":
							month = n
						case "Day":
							day = n
						case "Hour":
							hour = n
							gotHour = true
						case "Minute":
							minute = n
						case "Second":
							second = n
						}
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "UTCDateTime" {
				inUTC = false
			}
		}
	}
	if year < 1970 || month < 1 || month > 12 || day < 1 || day > 31 || !gotHour {
		return time.Time{}, errors.New("UTCDateTime not found")
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC), nil
}

func rewriteURLHost(rawAddr string, base *url.URL) string {
	rawAddr = strings.TrimSpace(rawAddr)
	if rawAddr == "" || base == nil {
		return rawAddr
	}
	u, err := url.Parse(rawAddr)
	if err != nil {
		return rawAddr
	}
	if u.Scheme == "" {
		u.Scheme = base.Scheme
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "127.0.0.1" || host == "localhost" || host == "0.0.0.0" || host == "::1" {
		port := u.Port()
		if port == "" {
			port = base.Port()
		}
		if port != "" {
			u.Host = net.JoinHostPort(base.Hostname(), port)
		} else {
			u.Host = base.Hostname()
		}
	}
	return u.String()
}

func extractSOAPFault(xmlBytes []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	inFault := false
	var reason, subcode string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Fault":
				inFault = true
			case "Text", "faultstring":
				if inFault {
					var txt string
					if dec.DecodeElement(&txt, &t) == nil && strings.TrimSpace(txt) != "" {
						reason = strings.TrimSpace(txt)
					}
				}
			case "Value", "faultcode":
				if inFault {
					var txt string
					if dec.DecodeElement(&txt, &t) == nil && strings.TrimSpace(txt) != "" {
						subcode = strings.TrimSpace(txt)
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "Fault" {
				inFault = false
			}
		}
	}
	if reason == "" && subcode == "" {
		return ""
	}
	if subcode != "" && reason != "" {
		return subcode + ": " + reason
	}
	if reason != "" {
		return reason
	}
	return subcode
}
