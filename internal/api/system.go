package api

import (
	"net"
	"net/http"
	"runtime"
)

func (s *Server) publicInfo(r *http.Request) map[string]any {
	return map[string]any{
		"LocalAddress":           "http://" + r.Host,
		"ServerName":             s.Name,
		"Version":                Version,
		"ProductName":            "jellyscale",
		"OperatingSystem":        "",
		"Id":                     s.ServerID,
		"StartupWizardCompleted": true,
	}
}

func (s *Server) systemInfoPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.publicInfo(r))
}

func (s *Server) systemInfo(w http.ResponseWriter, r *http.Request) {
	info := s.publicInfo(r)
	for key, value := range map[string]any{
		"OperatingSystemDisplayName": runtime.GOOS,
		"HasPendingRestart":          false,
		"IsShuttingDown":             false,
		"SupportsLibraryMonitor":     false,
		"WebSocketPortNumber":        8096,
		"CompletedInstallations":     []any{},
		"CanSelfRestart":             false,
		"CanLaunchWebBrowser":        false,
		"ProgramDataPath":            "",
		"WebPath":                    "",
		"ItemsByNamePath":            "",
		"CachePath":                  "",
		"LogPath":                    "",
		"InternalMetadataPath":       "",
		"TranscodingTempPath":        "",
		"CastReceiverApplications":   []any{},
		"HasUpdateAvailable":         false,
		"EncoderLocation":            "System",
		"SystemArchitecture":         runtime.GOARCH,
	} {
		info[key] = value
	}
	writeJSON(w, info)
}

func (s *Server) ping(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Name)
}

func (s *Server) systemEndpoint(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	local := ip != nil && (ip.IsLoopback() || ip.IsPrivate())
	writeJSON(w, map[string]any{"IsLocal": local, "IsInNetwork": local})
}

func (s *Server) branding(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"SplashscreenEnabled": false})
}

func quickConnectDisabled(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, false)
}
