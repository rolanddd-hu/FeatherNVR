package blueiris

import "feathernvr/internal/config"

// RawCamera represents the camera fields extracted from Blue Iris's Windows Registry or .reg export.
type RawCamera struct {
	KeyName         string
	ShortName       string
	Enabled         int
	IP              string
	Port            int
	RTSPPort        int
	ONVIFPort       int
	CamNo           int
	Path            string
	SubPath         string
	Params          string
	EventsPage      string
	PullEvents      int
	HTTPS           int
	Username        string
	PasswordRaw     string
	PasswordEncoded int
	ProfileToken    string
	ProfileToken2   string
	PTZEnabled      int
	PTZProtocol     int
	PTZPage         string
}

// ImportOptions controls how Blue Iris cameras are converted into FeatherNVR config.
type ImportOptions struct {
	MaxGB           float64
	IncludeDisabled bool
}

// ImportResult summarizes the converted FeatherNVR configuration.
type ImportResult struct {
	Config        *config.Config
	EnabledCount  int
	DisabledCount int
	DVRCount      int
	Warnings      []string
}
