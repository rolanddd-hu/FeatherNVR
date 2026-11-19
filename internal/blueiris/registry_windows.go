//go:build windows

package blueiris

import (
	"fmt"
	"sort"

	"golang.org/x/sys/windows/registry"
)

var regCameraPaths = []string{
	`SOFTWARE\Perspective Software\Blue Iris\Cameras`,
	`SOFTWARE\WOW6432Node\Perspective Software\Blue Iris\Cameras`,
}

// ReadWindowsRegistry reads camera entries directly from Blue Iris's Windows Registry keys.
func ReadWindowsRegistry() ([]RawCamera, error) {
	var root registry.Key
	var err error
	for _, p := range regCameraPaths {
		root, err = registry.OpenKey(registry.LOCAL_MACHINE, p, registry.READ)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("Blue Iris registry key not found under HKLM\\%s: %w", regCameraPaths[0], err)
	}
	defer root.Close()

	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return nil, fmt.Errorf("enumerate Blue Iris camera subkeys: %w", err)
	}
	sort.Strings(names)

	out := make([]RawCamera, 0, len(names))
	for _, name := range names {
		sub, err := registry.OpenKey(root, name, registry.READ)
		if err != nil {
			continue
		}
		rc := readCameraSubkey(name, sub)
		_ = sub.Close()
		out = append(out, rc)
	}
	return out, nil
}

func readCameraSubkey(name string, k registry.Key) RawCamera {
	getStr := func(valName string) string {
		s, _, err := k.GetStringValue(valName)
		if err != nil {
			return ""
		}
		return s
	}
	getInt := func(valName string, def int) int {
		v, _, err := k.GetIntegerValue(valName)
		if err != nil {
			return def
		}
		return int(int32(v))
	}

	rc := RawCamera{
		KeyName:         name,
		ShortName:       getStr("shortname"),
		Enabled:         getInt("enabled", 1),
		IP:              getStr("ip"),
		Port:            getInt("ip_port", 0),
		RTSPPort:        getInt("ip_port2", 0),
		ONVIFPort:       getInt("ip_onvif", 0),
		CamNo:           getInt("ip_camno", 1),
		Path:            getStr("ip_path"),
		SubPath:         getStr("ip_subpath"),
		Params:          getStr("ip_params"),
		EventsPage:      getStr("events_page"),
		PullEvents:      getInt("pullevents", 0),
		HTTPS:           getInt("https", 0),
		Username:        getStr("ipid"),
		PasswordRaw:     getStr("ippw"),
		PasswordEncoded: getInt("ippwencode", 0),
		ProfileToken:    getStr("profile_token"),
		ProfileToken2:   getStr("profile_token2"),
		PTZPage:         getStr("ptz_page"),
	}

	if ptzKey, err := registry.OpenKey(k, "PTZ", registry.READ); err == nil {
		if v, _, err := ptzKey.GetIntegerValue("enabled"); err == nil {
			rc.PTZEnabled = int(int32(v))
		}
		if v, _, err := ptzKey.GetIntegerValue("protocol"); err == nil {
			rc.PTZProtocol = int(int32(v))
		}
		_ = ptzKey.Close()
	}

	return rc
}
