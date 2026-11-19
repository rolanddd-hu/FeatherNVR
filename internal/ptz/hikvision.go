package ptz

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"feathernvr/internal/config"
)

// HikvisionController controls Hikvision cameras over ISAPI /ISAPI/PTZCtrl.
type HikvisionController struct {
	baseURL  string
	channel  int
	username string
	password string
	client   *http.Client
	digestNC uint32
}

// NewHikvisionController creates an ISAPI PTZ controller.
func NewHikvisionController(cfg config.PTZConfig) (*HikvisionController, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if base == "" {
		return nil, errors.New("hikvision ptz url cannot be empty")
	}
	ch := cfg.Channel
	if ch <= 0 {
		ch = 1
	}
	return &HikvisionController{
		baseURL:  base,
		channel:  ch,
		username: cfg.Username,
		password: cfg.Password,
		client: &http.Client{
			Timeout: 6 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}, nil
}

// Move issues continuous pan/tilt/zoom velocities clamped to [-1.0, 1.0].
func (c *HikvisionController) Move(ctx context.Context, pan, tilt, zoom float64) error {
	p := int(math.Round(clampUnit(pan) * 100.0))
	t := int(math.Round(clampUnit(tilt) * 100.0))
	z := int(math.Round(clampUnit(zoom) * 100.0))
	payload := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><PTZData><pan>%d</pan><tilt>%d</tilt><zoom>%d</zoom></PTZData>`, p, t, z)
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/continuous", c.channel)
	_, err := c.doRequest(ctx, http.MethodPut, path, "application/xml", []byte(payload))
	return err
}

// Stop halts all continuous pan/tilt/zoom movement.
func (c *HikvisionController) Stop(ctx context.Context) error {
	return c.Move(ctx, 0, 0, 0)
}

// ListPresets retrieves configured PTZ presets from the Hikvision camera.
func (c *HikvisionController) ListPresets(ctx context.Context) ([]Preset, error) {
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/presets", c.channel)
	body, err := c.doRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}

	var list struct {
		Presets []struct {
			Enabled    string `xml:"enabled"`
			ID         string `xml:"id"`
			PresetName string `xml:"presetName"`
		} `xml:"PTZPreset"`
	}
	if err := xml.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse hikvision presets: %w", err)
	}

	out := make([]Preset, 0, len(list.Presets))
	for _, p := range list.Presets {
		if strings.EqualFold(strings.TrimSpace(p.Enabled), "false") {
			continue
		}
		id := strings.TrimSpace(p.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(p.PresetName)
		if name == "" {
			name = "Preset " + id
		}
		out = append(out, Preset{ID: id, Name: name})
	}
	return out, nil
}

// GotoPreset moves the Hikvision camera to presetID.
func (c *HikvisionController) GotoPreset(ctx context.Context, presetID string) error {
	id := strings.TrimSpace(presetID)
	if id == "" {
		return errors.New("preset id cannot be empty")
	}
	path := fmt.Sprintf("/ISAPI/PTZCtrl/channels/%d/presets/%s/goto", c.channel, url.PathEscape(id))
	_, err := c.doRequest(ctx, http.MethodPut, path, "", nil)
	return err
}

func (c *HikvisionController) doRequest(ctx context.Context, method, reqPath, contentType string, payload []byte) ([]byte, error) {
	endpoint := c.baseURL + reqPath

	send := func(authHeader string) (int, http.Header, []byte, error) {
		var bodyReader io.Reader
		if len(payload) > 0 {
			bodyReader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
		if err != nil {
			return 0, nil, nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
		return resp.StatusCode, resp.Header, b, err
	}

	status, hdr, body, err := send("")
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && c.username != "" {
		wwwAuth := hdr.Get("WWW-Authenticate")
		if authHdr := buildHTTPAuthHeader(wwwAuth, method, endpoint, c.username, c.password, &c.digestNC); authHdr != "" {
			status, _, body, err = send(authHdr)
			if err != nil {
				return nil, err
			}
		}
	}
	if status >= 400 {
		return nil, fmt.Errorf("hikvision PTZ HTTP %d from %s", status, endpoint)
	}
	return body, nil
}

func buildHTTPAuthHeader(wwwAuth, method, rawURL, username, password string, nc *uint32) string {
	lower := strings.ToLower(wwwAuth)
	if strings.HasPrefix(lower, "digest ") {
		u, err := url.Parse(rawURL)
		if err != nil {
			return ""
		}
		uri := u.RequestURI()
		params := parseDigestChallenge(wwwAuth[7:])
		realm := params["realm"]
		nonce := params["nonce"]
		if realm == "" || nonce == "" {
			return ""
		}
		algo := strings.ToUpper(params["algorithm"])
		hashFn := func(s string) string {
			sum := md5.Sum([]byte(s))
			return hex.EncodeToString(sum[:])
		}
		if strings.HasPrefix(algo, "SHA-256") {
			hashFn = func(s string) string {
				sum := sha256.Sum256([]byte(s))
				return hex.EncodeToString(sum[:])
			}
		}

		var cnonceBytes [8]byte
		_, _ = rand.Read(cnonceBytes[:])
		cnonce := hex.EncodeToString(cnonceBytes[:])
		n := atomic.AddUint32(nc, 1)
		ncStr := fmt.Sprintf("%08x", n)

		ha1 := hashFn(username + ":" + realm + ":" + password)
		if strings.HasSuffix(algo, "-SESS") {
			ha1 = hashFn(ha1 + ":" + nonce + ":" + cnonce)
		}
		ha2 := hashFn(method + ":" + uri)

		qop := ""
		for _, part := range strings.Split(params["qop"], ",") {
			if strings.EqualFold(strings.TrimSpace(part), "auth") {
				qop = "auth"
				break
			}
		}

		var response string
		if qop != "" {
			response = hashFn(ha1 + ":" + nonce + ":" + ncStr + ":" + cnonce + ":" + qop + ":" + ha2)
		} else {
			response = hashFn(ha1 + ":" + nonce + ":" + ha2)
		}

		var sb strings.Builder
		sb.WriteString("Digest ")
		fmt.Fprintf(&sb, `username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
			username, realm, nonce, uri, response)
		if opaque := params["opaque"]; opaque != "" {
			fmt.Fprintf(&sb, `, opaque="%s"`, opaque)
		}
		if algo != "" {
			fmt.Fprintf(&sb, `, algorithm=%s`, algo)
		}
		if qop != "" {
			fmt.Fprintf(&sb, `, qop=%s, nc=%s, cnonce="%s"`, qop, ncStr, cnonce)
		}
		return sb.String()
	}

	if strings.HasPrefix(lower, "basic") {
		tok := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		return "Basic " + tok
	}
	return ""
}

func parseDigestChallenge(s string) map[string]string {
	out := make(map[string]string)
	for len(s) > 0 {
		s = strings.TrimLeft(s, ", \t")
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			break
		}
		k := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimLeft(s[eq+1:], " \t")
		var v string
		if strings.HasPrefix(s, `"`) {
			s = s[1:]
			end := strings.IndexByte(s, '"')
			if end < 0 {
				v = s
				s = ""
			} else {
				v = s[:end]
				s = s[end+1:]
			}
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				v = strings.TrimSpace(s)
				s = ""
			} else {
				v = strings.TrimSpace(s[:end])
				s = s[end+1:]
			}
		}
		out[k] = v
	}
	return out
}
