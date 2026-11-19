package motion

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"feathernvr/internal/config"
	"feathernvr/internal/procjob"
)

// DiscoveredDevice represents a camera discovered via ONVIF WS-Discovery.
type DiscoveredDevice struct {
	IP     string   `json:"ip"`
	XAddrs []string `json:"xaddrs"`
	Name   string   `json:"name,omitempty"`
	Scopes []string `json:"scopes,omitempty"`
}

// Discover performs ONVIF WS-Discovery multicast probing on the local network.
func Discover(ctx context.Context, timeout time.Duration) ([]DiscoveredDevice, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen udp: %w", err)
	}
	defer conn.Close()

	mcastAddr := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}
	msgID := newUUID()
	probe := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
            xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing"
            xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"
            xmlns:dn="http://www.onvif.org/ver10/network/wsdl">
  <s:Header>
    <a:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action>
    <a:MessageID>urn:uuid:%s</a:MessageID>
    <a:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To>
  </s:Header>
  <s:Body>
    <d:Probe>
      <d:Types>dn:NetworkVideoTransmitter</d:Types>
    </d:Probe>
  </s:Body>
</s:Envelope>`, msgID)

	if _, err := conn.WriteToUDP([]byte(probe), mcastAddr); err != nil {
		return nil, fmt.Errorf("send ws-discovery probe: %w", err)
	}
	// Send a second generic probe shortly after to catch cameras that omit NetworkVideoTransmitter type.
	probeAll := strings.Replace(probe, "<d:Types>dn:NetworkVideoTransmitter</d:Types>", "", 1)
	probeAll = strings.Replace(probeAll, msgID, newUUID(), 1)
	_, _ = conn.WriteToUDP([]byte(probeAll), mcastAddr)

	deadline := time.Now().Add(timeout)
	_ = conn.SetReadDeadline(deadline)

	seen := make(map[string]DiscoveredDevice)
	buf := make([]byte, 32*1024)

	for {
		if ctx.Err() != nil {
			break
		}
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			break
		}

		xaddrs, scopes := parseProbeMatch(buf[:n])
		if len(xaddrs) == 0 {
			continue
		}
		key := strings.Join(xaddrs, ",")
		if _, exists := seen[key]; exists {
			continue
		}
		name := ""
		for _, sc := range scopes {
			if strings.Contains(sc, "onvif://www.onvif.org/name/") {
				val := strings.TrimPrefix(sc, "onvif://www.onvif.org/name/")
				if decoded, err := url.PathUnescape(val); err == nil {
					name = decoded
				} else {
					name = val
				}
				break
			}
		}
		seen[key] = DiscoveredDevice{
			IP:     src.IP.String(),
			XAddrs: xaddrs,
			Name:   name,
			Scopes: scopes,
		}
	}

	out := make([]DiscoveredDevice, 0, len(seen))
	for _, dev := range seen {
		out = append(out, dev)
	}
	return out, nil
}

func parseProbeMatch(xmlBytes []byte) (xaddrs []string, scopes []string) {
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
		case "XAddrs":
			var txt string
			if dec.DecodeElement(&txt, &se) == nil {
				for _, f := range strings.Fields(txt) {
					xaddrs = append(xaddrs, f)
				}
			}
		case "Scopes":
			var txt string
			if dec.DecodeElement(&txt, &se) == nil {
				for _, f := range strings.Fields(txt) {
					scopes = append(scopes, f)
				}
			}
		}
	}
	return xaddrs, scopes
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// buildWSSecurityHeader builds an OASIS WS-UsernameToken PasswordDigest header.
func buildWSSecurityHeader(username, password string, clockOffset time.Duration, nonce []byte, now time.Time) string {
	if username == "" && password == "" {
		return ""
	}
	if len(nonce) == 0 {
		nonce = make([]byte, 16)
		_, _ = rand.Read(nonce)
	}
	created := now.UTC().Add(clockOffset).Format("2006-01-02T15:04:05Z")

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

// parseSystemDateAndTime extracts the camera UTC time from a GetSystemDateAndTimeResponse.
func parseSystemDateAndTime(xmlBytes []byte) (time.Time, error) {
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
		return time.Time{}, errors.New("UTCDateTime not found in GetSystemDateAndTime response")
	}
	return time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC), nil
}

// rewriteURLHost rewrites loopback/unspecified addresses returned by camera firmware to match baseURL.
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

// ONVIFMotionSignal represents one parsed motion notification item.
type ONVIFMotionSignal struct {
	Topic  string
	Active bool
}

// parsePullMessagesEvents extracts motion active/inactive signals from a PullMessagesResponse.
func parsePullMessagesEvents(xmlBytes []byte) ([]ONVIFMotionSignal, error) {
	if fault := extractSOAPFault(xmlBytes); fault != "" {
		return nil, &soapFaultError{msg: fault}
	}

	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	var signals []ONVIFMotionSignal

	inNotification := false
	curTopic := ""
	var itemStates []bool

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "NotificationMessage":
				inNotification = true
				curTopic = ""
				itemStates = nil
			case "Topic":
				if inNotification {
					var txt string
					if dec.DecodeElement(&txt, &t) == nil {
						curTopic = strings.TrimSpace(txt)
					}
				}
			case "SimpleItem":
				if inNotification {
					var name, val string
					for _, a := range t.Attr {
						switch a.Name.Local {
						case "Name":
							name = strings.ToLower(strings.TrimSpace(a.Value))
						case "Value":
							val = strings.ToLower(strings.TrimSpace(a.Value))
						}
					}
					if isMotionItemName(name) {
						if b, ok := parseBoolValue(val); ok {
							itemStates = append(itemStates, b)
						}
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "NotificationMessage" && inNotification {
				inNotification = false
				if isMotionTopic(curTopic) || len(itemStates) > 0 {
					for _, active := range itemStates {
						signals = append(signals, ONVIFMotionSignal{
							Topic:  curTopic,
							Active: active,
						})
					}
				}
			}
		}
	}
	return signals, nil
}

func isMotionTopic(topic string) bool {
	if topic == "" {
		return true // some minimal cameras omit Topic and rely on SimpleItem Name="IsMotion"
	}
	lt := strings.ToLower(topic)
	return strings.Contains(lt, "motion") ||
		strings.Contains(lt, "cellmotion") ||
		strings.Contains(lt, "linedetector") ||
		strings.Contains(lt, "fielddetector")
}

func isMotionItemName(name string) bool {
	switch name {
	case "ismotion", "state", "logicalstate", "motionalarm", "active":
		return true
	default:
		return false
	}
}

func parseBoolValue(val string) (bool, bool) {
	switch val {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func extractSOAPFault(xmlBytes []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	inFault := false
	var reason string
	var subcode string

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

type soapFaultError struct {
	msg string
}

func (e *soapFaultError) Error() string {
	return "SOAP fault: " + e.msg
}

func isFatalONVIFError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "actionnotsupported") ||
		strings.Contains(msg, "notauthorized") ||
		strings.Contains(msg, "not implemented") ||
		strings.Contains(msg, "wspullpointsupport=false") ||
		strings.Contains(msg, "actively refused") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "http 404") ||
		strings.Contains(msg, "http 405") ||
		strings.Contains(msg, "http 501") ||
		strings.Contains(msg, "unknown action")
}

// ONVIFDetector subscribes to ONVIF PullPoint motion events for one camera.
type ONVIFDetector struct {
	camID     string
	deviceURL string
	username  string
	password  string
	logger    *log.Logger
	tracker   *StatsTracker
	client    *http.Client
	digestNC  uint32
}

// NewONVIFDetector creates an ONVIF PullPoint event detector.
func NewONVIFDetector(camID, onvifURL, username, password string, logger *log.Logger) *ONVIFDetector {
	if logger == nil {
		logger = log.Default()
	}
	return &ONVIFDetector{
		camID:     camID,
		deviceURL: onvifURL,
		username:  username,
		password:  password,
		logger:    logger,
		tracker:   NewStatsTracker(config.MotionModeONVIF),
		client: &http.Client{
			Timeout: 25 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// Stats returns the current ONVIF detector status.
func (d *ONVIFDetector) Stats() Stats {
	return d.tracker.Snapshot()
}

// Run maintains a PullPoint subscription loop until ctx is canceled.
func (d *ONVIFDetector) Run(ctx context.Context, emit func(Event)) {
	_ = d.runWithFailureCount(ctx, emit, 0)
}

// runWithFailureCount runs until ctx cancels or consecutive pre-connect fatal/setup failures reach maxConsecFailures (if > 0).
// Returns true if it aborted due to maxConsecFailures (triggering fallback).
func (d *ONVIFDetector) runWithFailureCount(ctx context.Context, emit func(Event), maxConsecFailures int) bool {
	backoff := 2 * time.Second
	consecSetupFailures := 0
	everSucceeded := false

	for {
		if ctx.Err() != nil {
			d.tracker.SetConnected(false, "")
			return false
		}

		subOK, err := d.runSubscriptionSession(ctx, emit)
		if ctx.Err() != nil {
			d.tracker.SetConnected(false, "")
			return false
		}

		if subOK {
			everSucceeded = true
			consecSetupFailures = 0
			backoff = 2 * time.Second
		} else {
			consecSetupFailures++
		}

		d.tracker.SetConnected(false, err.Error())
		if maxConsecFailures > 0 && !everSucceeded && (isFatalONVIFError(err) || consecSetupFailures >= maxConsecFailures) {
			return true
		}

		d.logger.Printf("[motion][%s] ONVIF session ended (%v); retrying in %s", d.camID, err, backoff)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (d *ONVIFDetector) runSubscriptionSession(ctx context.Context, emit func(Event)) (bool, error) {
	baseURL, err := url.Parse(d.deviceURL)
	if err != nil {
		return false, fmt.Errorf("invalid onvif_url %q: %w", d.deviceURL, err)
	}

	// Step 1: Sync camera clock offset (best effort, unauthenticated).
	clockOffset := d.fetchClockOffset(ctx)

	// Step 2: Locate Events service XAddr & check WSPullPointSupport via GetCapabilities / GetServices.
	eventsURL, pullPointSupported := d.discoverEventsCapability(ctx, baseURL, clockOffset)
	if pullPointSupported != nil && !*pullPointSupported {
		return false, errors.New("ONVIF camera reports WSPullPointSupport=false (PullPoint motion events unsupported)")
	}

	// Step 3: CreatePullPointSubscription.
	subURL, err := d.createPullPointSubscription(ctx, eventsURL, baseURL, clockOffset)
	if err != nil {
		return false, err
	}

	defer func() {
		unsubCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = d.unsubscribe(unsubCtx, subURL, clockOffset)
		cancel()
	}()

	d.tracker.SetConnected(true, "")
	d.logger.Printf("[motion][%s] ONVIF PullPoint subscribed (%s)", d.camID, subURL)

	lastRenew := time.Now()
	for {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}

		if time.Since(lastRenew) >= 35*time.Second {
			if err := d.renewSubscription(ctx, subURL, clockOffset); err != nil {
				return true, fmt.Errorf("renew failed: %w", err)
			}
			lastRenew = time.Now()
		}

		signals, err := d.pullMessages(ctx, subURL, clockOffset)
		if err != nil {
			return true, err
		}

		now := time.Now()
		for _, sig := range signals {
			score := 0.0
			if sig.Active {
				score = 100.0
			}
			ev := Event{
				CameraID: d.camID,
				Active:   sig.Active,
				Source:   config.MotionModeONVIF,
				Score:    score,
				Time:     now,
			}
			d.tracker.RecordEvent(ev)
			emit(ev)
		}
	}
}

func (d *ONVIFDetector) fetchClockOffset(ctx context.Context) time.Duration {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	body := `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
  <s:Body><tds:GetSystemDateAndTime/></s:Body>
</s:Envelope>`

	respBytes, err := d.doSOAP(reqCtx, d.deviceURL, "http://www.onvif.org/ver10/device/wsdl/GetSystemDateAndTime", body, false)
	if err != nil {
		return 0
	}
	camUTC, err := parseSystemDateAndTime(respBytes)
	if err != nil {
		return 0
	}
	return camUTC.Sub(time.Now().UTC())
}

func (d *ONVIFDetector) discoverEventsCapability(ctx context.Context, baseURL *url.URL, clockOffset time.Duration) (string, *bool) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	sec := buildWSSecurityHeader(d.username, d.password, clockOffset, nil, time.Now())
	var hdr string
	if sec != "" {
		hdr = "<s:Header>" + sec + "</s:Header>"
	}

	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
  %s
  <s:Body><tds:GetCapabilities><tds:Category>All</tds:Category></tds:GetCapabilities></s:Body>
</s:Envelope>`, hdr)

	if respBytes, err := d.doSOAP(reqCtx, d.deviceURL, "http://www.onvif.org/ver10/device/wsdl/GetCapabilities", body, true); err == nil {
		xaddr, pullSupport := parseCapabilitiesEvents(respBytes)
		if pullSupport != nil && !*pullSupport {
			return rewriteURLHost(xaddr, baseURL), pullSupport
		}
		if xaddr != "" {
			return rewriteURLHost(xaddr, baseURL), pullSupport
		}
	}

	// Fallback: GetServices
	sec = buildWSSecurityHeader(d.username, d.password, clockOffset, nil, time.Now())
	hdr = ""
	if sec != "" {
		hdr = "<s:Header>" + sec + "</s:Header>"
	}
	bodySvc := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl">
  %s
  <s:Body><tds:GetServices><tds:IncludeCapability>false</tds:IncludeCapability></tds:GetServices></s:Body>
</s:Envelope>`, hdr)

	if respBytes, err := d.doSOAP(reqCtx, d.deviceURL, "http://www.onvif.org/ver10/device/wsdl/GetServices", bodySvc, true); err == nil {
		if xaddr := parseEventsXAddrFromServices(respBytes); xaddr != "" {
			return rewriteURLHost(xaddr, baseURL), nil
		}
	}

	return d.deviceURL, nil
}

func parseCapabilitiesEvents(xmlBytes []byte) (xaddr string, pullPointSupported *bool) {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	inEvents := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "Events" {
				inEvents = true
			} else if inEvents && t.Name.Local == "XAddr" {
				var txt string
				if dec.DecodeElement(&txt, &t) == nil && xaddr == "" {
					xaddr = strings.TrimSpace(txt)
				}
			} else if inEvents && t.Name.Local == "WSPullPointSupport" {
				var txt string
				if dec.DecodeElement(&txt, &t) == nil {
					if b, ok := parseBoolValue(strings.ToLower(strings.TrimSpace(txt))); ok {
						val := b
						pullPointSupported = &val
					}
				}
			}
		case xml.EndElement:
			if t.Name.Local == "Events" {
				inEvents = false
			}
		}
	}
	return xaddr, pullPointSupported
}

func parseEventsXAddrFromServices(xmlBytes []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	inService := false
	var ns, xaddr string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "Service":
				inService = true
				ns, xaddr = "", ""
			case "Namespace":
				if inService {
					_ = dec.DecodeElement(&ns, &t)
				}
			case "XAddr":
				if inService {
					_ = dec.DecodeElement(&xaddr, &t)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "Service" && inService {
				inService = false
				if strings.Contains(strings.ToLower(ns), "/events/wsdl") && strings.TrimSpace(xaddr) != "" {
					return strings.TrimSpace(xaddr)
				}
			}
		}
	}
	return ""
}

func (d *ONVIFDetector) createPullPointSubscription(ctx context.Context, eventsURL string, baseURL *url.URL, clockOffset time.Duration) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	action := "http://www.onvif.org/ver10/events/wsdl/EventPortType/CreatePullPointSubscriptionRequest"
	sec := buildWSSecurityHeader(d.username, d.password, clockOffset, nil, time.Now())
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
            xmlns:wsa="http://www.w3.org/2005/08/addressing"
            xmlns:tev="http://www.onvif.org/ver10/events/wsdl">
  <s:Header>
    <wsa:Action>%s</wsa:Action>
    <wsa:To>%s</wsa:To>
    %s
  </s:Header>
  <s:Body>
    <tev:CreatePullPointSubscription>
      <tev:InitialTerminationTime>PT60S</tev:InitialTerminationTime>
    </tev:CreatePullPointSubscription>
  </s:Body>
</s:Envelope>`, action, xmlEscape(eventsURL), sec)

	respBytes, err := d.doSOAP(reqCtx, eventsURL, action, body, true)
	if err != nil {
		return "", err
	}
	if fault := extractSOAPFault(respBytes); fault != "" {
		return "", &soapFaultError{msg: fault}
	}

	addr := parseSubscriptionAddress(respBytes)
	if addr == "" {
		return "", errors.New("CreatePullPointSubscription returned empty SubscriptionReference Address")
	}
	return rewriteURLHost(addr, baseURL), nil
}

func parseSubscriptionAddress(xmlBytes []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(xmlBytes))
	inSubRef := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "SubscriptionReference" {
				inSubRef = true
			} else if inSubRef && t.Name.Local == "Address" {
				var txt string
				if dec.DecodeElement(&txt, &t) == nil {
					return strings.TrimSpace(txt)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "SubscriptionReference" {
				inSubRef = false
			}
		}
	}
	return ""
}

func (d *ONVIFDetector) pullMessages(ctx context.Context, subURL string, clockOffset time.Duration) ([]ONVIFMotionSignal, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 23*time.Second)
	defer cancel()

	action := "http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/PullMessagesRequest"
	sec := buildWSSecurityHeader(d.username, d.password, clockOffset, nil, time.Now())
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
            xmlns:wsa="http://www.w3.org/2005/08/addressing"
            xmlns:tev="http://www.onvif.org/ver10/events/wsdl">
  <s:Header>
    <wsa:Action>%s</wsa:Action>
    <wsa:To>%s</wsa:To>
    %s
  </s:Header>
  <s:Body>
    <tev:PullMessages>
      <tev:Timeout>PT15S</tev:Timeout>
      <tev:MessageLimit>20</tev:MessageLimit>
    </tev:PullMessages>
  </s:Body>
</s:Envelope>`, action, xmlEscape(subURL), sec)

	respBytes, err := d.doSOAP(reqCtx, subURL, action, body, true)
	if err != nil {
		return nil, err
	}
	return parsePullMessagesEvents(respBytes)
}

func (d *ONVIFDetector) renewSubscription(ctx context.Context, subURL string, clockOffset time.Duration) error {
	reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	action := "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/RenewRequest"
	sec := buildWSSecurityHeader(d.username, d.password, clockOffset, nil, time.Now())
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
            xmlns:wsa="http://www.w3.org/2005/08/addressing"
            xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2">
  <s:Header>
    <wsa:Action>%s</wsa:Action>
    <wsa:To>%s</wsa:To>
    %s
  </s:Header>
  <s:Body>
    <wsnt:Renew>
      <wsnt:TerminationTime>PT60S</wsnt:TerminationTime>
    </wsnt:Renew>
  </s:Body>
</s:Envelope>`, action, xmlEscape(subURL), sec)

	respBytes, err := d.doSOAP(reqCtx, subURL, action, body, true)
	if err != nil {
		return err
	}
	if fault := extractSOAPFault(respBytes); fault != "" {
		return &soapFaultError{msg: fault}
	}
	return nil
}

func (d *ONVIFDetector) unsubscribe(ctx context.Context, subURL string, clockOffset time.Duration) error {
	action := "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/UnsubscribeRequest"
	sec := buildWSSecurityHeader(d.username, d.password, clockOffset, nil, time.Now())
	body := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"
            xmlns:wsa="http://www.w3.org/2005/08/addressing"
            xmlns:wsnt="http://docs.oasis-open.org/wsn/b-2">
  <s:Header>
    <wsa:Action>%s</wsa:Action>
    <wsa:To>%s</wsa:To>
    %s
  </s:Header>
  <s:Body>
    <wsnt:Unsubscribe/>
  </s:Body>
</s:Envelope>`, action, xmlEscape(subURL), sec)
	_, err := d.doSOAP(ctx, subURL, action, body, true)
	return err
}

func (d *ONVIFDetector) doSOAP(ctx context.Context, endpoint, action, xmlBody string, allowHTTPAuth bool) ([]byte, error) {
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
		resp, err := d.client.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
		if readErr != nil {
			return resp.StatusCode, resp.Header, nil, readErr
		}
		return resp.StatusCode, resp.Header, b, nil
	}

	status, hdr, body, err := send("")
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && allowHTTPAuth && d.username != "" {
		wwwAuth := hdr.Get("WWW-Authenticate")
		if authHdr := buildHTTPAuthHeader(wwwAuth, http.MethodPost, endpoint, d.username, d.password, &d.digestNC); authHdr != "" {
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

// ONVIFOrPixelDetector tries ONVIF PullPoint events first and automatically falls
// back to sub-stream pixel-diff detection if ONVIF events fail or are unsupported.
type ONVIFOrPixelDetector struct {
	camID   string
	onvif   *ONVIFDetector
	pixel   *PixelDetector
	logger  *log.Logger

	mu     sync.RWMutex
	active Detector
}

// NewONVIFOrPixelDetector builds a hybrid detector with ONVIF primary and Pixel fallback.
func NewONVIFOrPixelDetector(
	camID, onvifURL, username, password, subRTSP, ffmpegBin string,
	cfg config.MotionConfig, job *procjob.Manager, logger *log.Logger,
) *ONVIFOrPixelDetector {
	if logger == nil {
		logger = log.Default()
	}
	onv := NewONVIFDetector(camID, onvifURL, username, password, logger)
	var pix *PixelDetector
	if ffmpegBin != "" && subRTSP != "" {
		pix = NewPixelDetector(camID, subRTSP, ffmpegBin, cfg, job, logger)
	}
	return &ONVIFOrPixelDetector{
		camID:  camID,
		onvif:  onv,
		pixel:  pix,
		logger: logger,
		active: onv,
	}
}

// Stats returns the stats of whichever detector (ONVIF or Pixel) is currently active.
func (d *ONVIFOrPixelDetector) Stats() Stats {
	d.mu.RLock()
	cur := d.active
	d.mu.RUnlock()
	return cur.Stats()
}

// Run runs ONVIF first; on persistent/unsupported failure, switches to PixelDetector.
func (d *ONVIFOrPixelDetector) Run(ctx context.Context, emit func(Event)) {
	maxFail := 3
	if d.pixel == nil {
		maxFail = 0 // keep retrying ONVIF if ffmpeg is unavailable
	}
	fallback := d.onvif.runWithFailureCount(ctx, emit, maxFail)
	if !fallback || ctx.Err() != nil || d.pixel == nil {
		return
	}

	d.logger.Printf("[motion][%s] ONVIF events unavailable (%s); falling back to sub-stream pixel motion", d.camID, d.onvif.Stats().LastError)
	d.mu.Lock()
	d.active = d.pixel
	d.mu.Unlock()
	d.pixel.Run(ctx, emit)
}

// buildHTTPAuthHeader constructs Basic or Digest Authorization headers from WWW-Authenticate.
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
		*nc++
		ncStr := fmt.Sprintf("%08x", *nc)

		ha1 := hashFn(username + ":" + realm + ":" + password)
		if strings.HasSuffix(algo, "-SESS") {
			ha1 = hashFn(ha1 + ":" + nonce + ":" + cnonce)
		}
		ha2 := hashFn(method + ":" + uri)

		qop := selectQOP(params["qop"])
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

func selectQOP(qopList string) string {
	for _, part := range strings.Split(qopList, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "auth") {
			return "auth"
		}
	}
	return ""
}
