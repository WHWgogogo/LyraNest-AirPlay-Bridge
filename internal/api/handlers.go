package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lyranest/lyranest-airplay-bridge/internal/discovery"
	"github.com/lyranest/lyranest-airplay-bridge/internal/engine"
)

// engineLabel maps the selected kernel onto the label published by the
// architecture document's /healthz example.
func engineLabel(kind string) string {
	if kind == "cliairplay" {
		return "airplay-cli-unified"
	}
	return "lyranest-native-raop"
}

// healthz implements GET /healthz.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	kind := s.manager.EngineName()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        Version,
		"engine":         engineLabel(kind),
		"kernel":         kind,
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
	})
}

// status implements GET /api/status.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	kind := s.manager.EngineName()
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds":   int64(time.Since(s.startedAt).Seconds()),
		"active_sessions":  s.manager.ActiveSessions(),
		"active_device_id": s.manager.ActiveDeviceID(),
		"devices_count":    s.registry.Count(),
		"version":          Version,
		"engine":           engineLabel(kind),
		"kernel":           kind,
		"discovery":        s.discoveryEnabled,
	})
}

// devices implements GET /api/devices.
func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	if wantsRefresh(r.URL.Query().Get("refresh")) && s.browser != nil {
		if err := s.browser.Refresh(r.Context(), 2*time.Second); err != nil {
			s.logger.Debug("mDNS refresh failed", "error", err)
		}
	}
	devices := s.registry.List()
	views := make([]deviceView, 0, len(devices))
	for _, device := range devices {
		views = append(views, newDeviceView(deviceLikeFromDiscovery(device)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": views})
}

// addManualDevice implements POST /api/devices/manual. It exists for hosts
// where multicast mDNS is filtered, so an operator can still pin a speaker.
func (s *Server) addManualDevice(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Address string `json:"address"`
		Port    int    `json:"port"`
	}
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	address := strings.TrimSpace(request.Address)
	if address == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	if request.Port > 0 {
		address = fmt.Sprintf("%s:%d", address, request.Port)
	}
	device, err := s.registry.UpsertStatic(address, 7000)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"success": true, "device": newDeviceView(deviceLikeFromDiscovery(device))})
}

// play implements POST /api/player/play.
func (s *Server) play(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DeviceID        string        `json:"device_id"`
		StreamURL       string        `json:"stream_url"`
		TrackID         flexibleInt64 `json:"track_id"`
		Title           string        `json:"title"`
		Artist          string        `json:"artist"`
		Album           string        `json:"album"`
		DurationMS      int64         `json:"duration_ms"`
		StartPositionMS int64         `json:"start_position_ms"`
		Volume          *int          `json:"volume"`
	}
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if strings.TrimSpace(request.DeviceID) == "" {
		writeError(w, http.StatusBadRequest, "device_id is required")
		return
	}
	streamURL := s.resolveStreamURL(request.StreamURL)
	if streamURL == "" {
		writeError(w, http.StatusBadRequest, "stream_url is required")
		return
	}
	if request.StartPositionMS < 0 {
		request.StartPositionMS = 0
	}
	if request.DurationMS < 0 {
		request.DurationMS = 0
	}
	volume := -1
	if request.Volume != nil {
		volume = *request.Volume
	}

	status, err := s.manager.Start(r.Context(), request.DeviceID, engine.PlayRequest{
		StreamURL:       streamURL,
		TrackID:         request.TrackID.Int64(),
		Title:           request.Title,
		Artist:          request.Artist,
		Album:           request.Album,
		DurationMS:      request.DurationMS,
		StartPositionMS: request.StartPositionMS,
		Volume:          volume,
	})
	if err != nil {
		writeError(w, s.playErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"state":     string(status.State),
		"device_id": status.DeviceID,
		"message":   "session initialized and streaming started",
	})
}

// control implements POST /api/player/control.
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DeviceID   string `json:"device_id"`
		Action     string `json:"action"`
		PositionMS int64  `json:"position_ms"`
	}
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	action := strings.ToLower(strings.TrimSpace(request.Action))
	switch action {
	case engine.ActionPause, engine.ActionResume, engine.ActionStop, engine.ActionSeek:
	default:
		writeError(w, http.StatusBadRequest, "action must be one of pause, resume, stop, seek")
		return
	}
	if action == engine.ActionSeek && request.PositionMS < 0 {
		writeError(w, http.StatusBadRequest, "position_ms must not be negative for seek")
		return
	}

	status, err := s.manager.Control(request.DeviceID, action, request.PositionMS)
	if err != nil {
		writeError(w, s.sessionErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":             true,
		"state":               string(status.State),
		"device_id":           status.DeviceID,
		"current_position_ms": status.PositionMS,
	})
}

// volume implements POST /api/player/volume.
func (s *Server) volume(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DeviceID string `json:"device_id"`
		Volume   *int   `json:"volume"`
	}
	if err := decodeBody(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if request.Volume == nil {
		writeError(w, http.StatusBadRequest, "volume is required")
		return
	}
	volume := *request.Volume
	if volume < 0 || volume > 100 {
		writeError(w, http.StatusBadRequest, "volume must be between 0 and 100")
		return
	}

	status, err := s.manager.SetVolume(request.DeviceID, volume)
	if err != nil {
		writeError(w, s.sessionErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"volume":    status.Volume,
		"device_id": status.DeviceID,
	})
}

// playerStatus implements GET /api/player/status.
func (s *Server) playerStatus(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimSpace(r.URL.Query().Get("device_id"))
	status, ok := s.manager.Status(deviceID)
	if !ok {
		// An idle bridge is not an error: the LyraNest server polls this
		// endpoint continuously and expects a well-formed idle document.
		writeJSON(w, http.StatusOK, engine.Status{
			Active:   false,
			State:    engine.StateStopped,
			DeviceID: deviceID,
			Engine:   s.manager.EngineName(),
		})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) resolveStreamURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.IsAbs() || s.serverURL == "" {
		return raw
	}
	base, err := url.Parse(s.serverURL + "/")
	if err != nil {
		return raw
	}
	return base.ResolveReference(parsed).String()
}

func (s *Server) playErrorStatus(err error) int {
	switch {
	case errors.Is(err, engine.ErrAlreadyActive):
		return http.StatusConflict
	case errors.Is(err, engine.ErrNotSupported):
		return http.StatusNotImplemented
	default:
		if strings.Contains(err.Error(), "not found") {
			return http.StatusNotFound
		}
		return http.StatusBadGateway
	}
}

func (s *Server) sessionErrorStatus(err error) int {
	switch {
	case errors.Is(err, engine.ErrNoSession):
		return http.StatusNotFound
	case errors.Is(err, engine.ErrNotSupported):
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}

func wantsRefresh(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "1" || value == "true" || value == "yes"
}

// deviceView is the device document published by GET /api/devices.
type deviceView struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Model            string            `json:"model"`
	Address          string            `json:"address"`
	Port             int               `json:"port"`
	Features         string            `json:"features"`
	Protocol         string            `json:"protocol"`
	RequiresPassword bool              `json:"requires_password"`
	IsBusy           bool              `json:"is_busy"`
	LastSeen         string            `json:"last_seen"`
	Source           string            `json:"source,omitempty"`
	MAC              string            `json:"mac,omitempty"`
	Codecs           []int             `json:"codecs,omitempty"`
	TXTCodecs        string            `json:"txt_cn,omitempty"`
	TXT              map[string]string `json:"txt,omitempty"`
}

func newDeviceView(device deviceLike) deviceView {
	view := deviceView{
		ID:               device.ID,
		Name:             device.Name,
		Model:            device.Model,
		Address:          device.Address,
		Port:             device.Port,
		Features:         device.Features,
		Protocol:         device.Protocol,
		RequiresPassword: device.RequiresPassword,
		IsBusy:           device.IsBusy,
		Source:           device.Source,
		MAC:              device.MAC,
		Codecs:           device.Codecs(),
		TXTCodecs:        device.TXT["cn"],
		TXT:              device.TXT,
	}
	if !device.LastSeen.IsZero() {
		view.LastSeen = device.LastSeen.UTC().Format(time.RFC3339)
	}
	return view
}

// deviceLikeFromDiscovery projects a discovered device onto the API view model.
func deviceLikeFromDiscovery(device discovery.Device) deviceLike {
	return deviceLike{
		ID:               device.ID,
		Name:             device.Name,
		Model:            device.Model,
		Address:          device.Address,
		Port:             device.Port,
		Features:         device.Features,
		Protocol:         device.Protocol,
		RequiresPassword: device.RequiresPassword,
		IsBusy:           device.IsBusy,
		LastSeen:         device.LastSeen,
		Source:           device.Source,
		MAC:              device.MAC,
		TXT:              device.TXT,
	}
}

// deviceLike is the subset of discovery.Device the API serialises. It keeps the
// view layer testable with a plain struct.
type deviceLike struct {
	ID               string
	Name             string
	Model            string
	Address          string
	Port             int
	Features         string
	Protocol         string
	RequiresPassword bool
	IsBusy           bool
	LastSeen         time.Time
	Source           string
	MAC              string
	TXT              map[string]string
}

// Codecs returns the RAOP codec bitmask advertised in the `cn` TXT record.
func (d deviceLike) Codecs() []int {
	raw := strings.TrimSpace(d.TXT["cn"])
	if raw == "" {
		return nil
	}
	var codecs []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		value := 0
		valid := true
		for _, r := range part {
			if r < '0' || r > '9' {
				valid = false
				break
			}
			value = value*10 + int(r-'0')
		}
		if valid {
			codecs = append(codecs, value)
		}
	}
	return codecs
}
