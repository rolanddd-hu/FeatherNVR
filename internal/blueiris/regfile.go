package blueiris

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ReadRegFile reads and parses a Blue Iris Windows Registry export (.reg) file.
func ReadRegFile(path string) ([]RawCamera, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read reg file %q: %w", path, err)
	}
	return ParseRegBytes(data)
}

// ParseRegBytes parses UTF-16LE, UTF-16BE, or UTF-8 Windows .reg file contents into RawCamera entries.
func ParseRegBytes(data []byte) ([]RawCamera, error) {
	text := decodeRegText(data)
	lines := joinRegContinuations(text)

	cams := make(map[string]*RawCamera)
	var cur *RawCamera
	inPTZ := false

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := line[1 : len(line)-1]
			cur = nil
			inPTZ = false
			if camName, isPTZ, ok := extractBlueIrisCameraSection(section); ok {
				c, exists := cams[camName]
				if !exists {
					c = &RawCamera{KeyName: camName, Enabled: 1, CamNo: 1}
					cams[camName] = c
				}
				cur = c
				inPTZ = isPTZ
			}
			continue
		}

		if cur == nil {
			continue
		}

		key, val, ok := splitRegKeyValue(line)
		if !ok {
			continue
		}
		if inPTZ {
			applyPTZRegValue(cur, key, val)
		} else {
			applyRegValue(cur, key, val)
		}
	}

	out := make([]RawCamera, 0, len(cams))
	for _, c := range cams {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].KeyName < out[j].KeyName
	})
	return out, nil
}

func decodeRegText(data []byte) string {
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		return decodeUTF16(data[2:], binary.LittleEndian)
	}
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		return decodeUTF16(data[2:], binary.BigEndian)
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	return string(data)
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	n := len(b) / 2
	u16 := make([]uint16, n)
	for i := 0; i < n; i++ {
		u16[i] = order.Uint16(b[i*2 : i*2+2])
	}
	return string(utf16.Decode(u16))
}

func joinRegContinuations(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	rawLines := strings.Split(text, "\n")

	out := make([]string, 0, len(rawLines))
	var acc strings.Builder
	continuing := false

	for _, line := range rawLines {
		if continuing {
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasSuffix(trimmed, `\`) {
				acc.WriteString(strings.TrimSuffix(trimmed, `\`))
				continue
			}
			acc.WriteString(trimmed)
			out = append(out, acc.String())
			acc.Reset()
			continuing = false
			continue
		}

		if strings.HasSuffix(line, `\`) && !strings.HasPrefix(strings.TrimSpace(line), "[") {
			acc.WriteString(strings.TrimSuffix(line, `\`))
			continuing = true
			continue
		}
		out = append(out, line)
	}
	if continuing {
		out = append(out, acc.String())
	}
	return out
}

func extractBlueIrisCameraSection(section string) (camName string, isPTZ bool, ok bool) {
	prefixes := []string{
		`HKEY_LOCAL_MACHINE\SOFTWARE\Perspective Software\Blue Iris\Cameras\`,
		`HKEY_LOCAL_MACHINE\SOFTWARE\WOW6432Node\Perspective Software\Blue Iris\Cameras\`,
	}
	lower := strings.ToLower(section)
	for _, p := range prefixes {
		if strings.HasPrefix(lower, strings.ToLower(p)) {
			rest := section[len(p):]
			if rest == "" {
				return "", false, false
			}
			if idx := strings.IndexByte(rest, '\\'); idx >= 0 {
				sub := rest[idx+1:]
				if strings.EqualFold(sub, "PTZ") && idx > 0 {
					return rest[:idx], true, true
				}
				return "", false, false
			}
			return rest, false, true
		}
	}
	return "", false, false
}

func splitRegKeyValue(line string) (string, string, bool) {
	if !strings.HasPrefix(line, `"`) {
		return "", "", false
	}
	i := 1
	var kb strings.Builder
	for i < len(line) {
		ch := line[i]
		if ch == '\\' && i+1 < len(line) {
			kb.WriteByte(line[i+1])
			i += 2
			continue
		}
		if ch == '"' {
			i++
			break
		}
		kb.WriteByte(ch)
		i++
	}
	if i >= len(line) || line[i] != '=' {
		return "", "", false
	}
	return strings.ToLower(kb.String()), strings.TrimSpace(line[i+1:]), true
}

func applyRegValue(c *RawCamera, key, rawVal string) {
	parseStr := func() (string, bool) {
		if !strings.HasPrefix(rawVal, `"`) || !strings.HasSuffix(rawVal, `"`) || len(rawVal) < 2 {
			return "", false
		}
		inner := rawVal[1 : len(rawVal)-1]
		var out strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				out.WriteByte(inner[i+1])
				i++
			} else {
				out.WriteByte(inner[i])
			}
		}
		return out.String(), true
	}

	parseDword := func() (int, bool) {
		if !strings.HasPrefix(strings.ToLower(rawVal), "dword:") {
			return 0, false
		}
		v, err := strconv.ParseUint(rawVal[6:], 16, 32)
		if err != nil {
			return 0, false
		}
		return int(int32(v)), true
	}

	switch key {
	case "shortname":
		if s, ok := parseStr(); ok {
			c.ShortName = s
		}
	case "enabled":
		if v, ok := parseDword(); ok {
			c.Enabled = v
		}
	case "ip":
		if s, ok := parseStr(); ok {
			c.IP = s
		}
	case "ip_port":
		if v, ok := parseDword(); ok {
			c.Port = v
		}
	case "ip_port2":
		if v, ok := parseDword(); ok {
			c.RTSPPort = v
		}
	case "ip_onvif":
		if v, ok := parseDword(); ok {
			c.ONVIFPort = v
		}
	case "ip_camno":
		if v, ok := parseDword(); ok {
			c.CamNo = v
		}
	case "ip_path":
		if s, ok := parseStr(); ok {
			c.Path = s
		}
	case "ip_subpath":
		if s, ok := parseStr(); ok {
			c.SubPath = s
		}
	case "ip_params":
		if s, ok := parseStr(); ok {
			c.Params = s
		}
	case "events_page":
		if s, ok := parseStr(); ok {
			c.EventsPage = s
		}
	case "pullevents":
		if v, ok := parseDword(); ok {
			c.PullEvents = v
		}
	case "https":
		if v, ok := parseDword(); ok {
			c.HTTPS = v
		}
	case "ipid":
		if s, ok := parseStr(); ok {
			c.Username = s
		}
	case "ippw":
		if s, ok := parseStr(); ok {
			c.PasswordRaw = s
		}
	case "ippwencode":
		if v, ok := parseDword(); ok {
			c.PasswordEncoded = v
		}
	case "profile_token":
		if s, ok := parseStr(); ok {
			c.ProfileToken = s
		}
	case "profile_token2":
		if s, ok := parseStr(); ok {
			c.ProfileToken2 = s
		}
	case "ptz_page":
		if s, ok := parseStr(); ok {
			c.PTZPage = s
		}
	}
}

func applyPTZRegValue(c *RawCamera, key, rawVal string) {
	if !strings.HasPrefix(strings.ToLower(rawVal), "dword:") {
		return
	}
	v, err := strconv.ParseUint(rawVal[6:], 16, 32)
	if err != nil {
		return
	}
	iv := int(int32(v))
	switch key {
	case "enabled":
		c.PTZEnabled = iv
	case "protocol":
		c.PTZProtocol = iv
	}
}
