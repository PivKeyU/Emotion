package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/PivKeyU/Emotion/internal/config"
)

// System provides /System/* endpoints that Emby clients call at startup to
// discover server identity and capabilities.
type System struct {
	cfg *config.Config
}

// NewSystem constructs a system handler.
func NewSystem(cfg *config.Config) *System {
	return &System{cfg: cfg}
}

// Info returns full server info. Mirrors emya system.controller.ts /Info.
// Transcoding-related flags are returned but set to "not supported" to keep
// clients that probe capabilities from crashing.
func (s *System) Info(w http.ResponseWriter, r *http.Request) {
	addr := "http://" + s.cfg.EmbyID
	WriteJSON(w, http.StatusOK, map[string]any{
		"SystemUpdateLevel":                    "Release",
		"OperatingSystemDisplayName":           "Linux",
		"HasPendingRestart":                    false,
		"IsShuttingDown":                       false,
		"HasImageEnhancers":                    false,
		"OperatingSystem":                      "Linux",
		"SupportsLibraryMonitor":               true,
		"SupportsLocalPortConfiguration":       true,
		"SupportsWakeServer":                   false,
		"WebSocketPortNumber":                  s.cfg.ServerPort,
		"CompletedInstallations":               []any{},
		"CanSelfRestart":                       false,
		"CanSelfUpdate":                        false,
		"CanLaunchWebBrowser":                  false,
		"ProgramDataPath":                      "/emotion",
		"ItemsByNamePath":                      "/emotion/metadata",
		"CachePath":                            "/emotion/cache",
		"LogPath":                              "/emotion/logs",
		"InternalMetadataPath":                 "/emotion/metadata",
		"TranscodingTempPath":                  "/emotion/transcoding-temp",
		"HttpServerPortNumber":                 s.cfg.ServerPort,
		"SupportsHttps":                        false,
		"HttpsPortNumber":                      8920,
		"HasUpdateAvailable":                   false,
		"SupportsAutoRunAtStartup":             false,
		"HardwareAccelerationRequiresPremiere": true,
		"WakeOnLanInfo": map[string]any{
			"MacAddress":       "FFFFFFFFFFFF",
			"BroadcastAddress": "255.255.255.255",
			"Port":             9,
		},
		"IsInMaintenanceMode": false,
		"LocalAddress":        addr,
		"LocalAddresses":      []any{addr},
		"WanAddress":          addr,
		"RemoteAddresses":     []any{addr},
		"ServerName":          s.cfg.AppName,
		"Version":             s.cfg.EmbyVersion,
		"Id":                  s.cfg.EmbyID,
		"StorageType":         s.cfg.StorageType,
		"MediaProbeWorkers":   s.cfg.MediaProbeWorkers,
	})
}

// InfoPublic is the unauthenticated subset of /Info.
func (s *System) InfoPublic(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]any{
		"LocalAddresses":  []any{},
		"RemoteAddresses": []any{},
		"ServerName":      s.cfg.AppName,
		"Version":         s.cfg.EmbyVersion,
		"Id":              s.cfg.EmbyID,
	})
}

// Ping is Emby's keep-alive endpoint. Real Emby returns plain text "Emby Server".
func (s *System) Ping(w http.ResponseWriter, r *http.Request) {
	WriteText(w, http.StatusOK, "Emotion Server")
}

// ExtServerDomains returns the optional UHD-Now "server domains" list.
// https://github.com/uhdnow/emby_ext_domains
func (s *System) ExtServerDomains(w http.ResponseWriter, r *http.Request) {
	if s.cfg.EmbyExtServerDomains == "" {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	var parsed any
	if err := json.Unmarshal([]byte(s.cfg.EmbyExtServerDomains), &parsed); err != nil {
		WriteStatus(w, http.StatusNotFound)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"data": parsed,
		"ok":   true,
	})
}
