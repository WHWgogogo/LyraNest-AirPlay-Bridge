package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lyranest/lyranest-airplay-bridge/internal/discovery"
	"github.com/lyranest/lyranest-airplay-bridge/internal/engine"
)

// fakeManager records what the REST layer asked the kernel to do.
type fakeManager struct {
	kind     string
	started  []engine.PlayRequest
	startOn  string
	actions  []string
	volumes  []int
	status   engine.Status
	haveSt   bool
	startErr error
	ctrlErr  error
	volErr   error
}

func (f *fakeManager) EngineName() string { return f.kind }

func (f *fakeManager) Start(_ context.Context, deviceID string, request engine.PlayRequest) (engine.Status, error) {
	if f.startErr != nil {
		return engine.Status{}, f.startErr
	}
	f.started = append(f.started, request)
	f.startOn = deviceID
	return f.status, nil
}

func (f *fakeManager) Control(deviceID, action string, positionMS int64) (engine.Status, error) {
	if f.ctrlErr != nil {
		return engine.Status{}, f.ctrlErr
	}
	f.actions = append(f.actions, action)
	return f.status, nil
}

func (f *fakeManager) SetVolume(deviceID string, volume int) (engine.Status, error) {
	if f.volErr != nil {
		return engine.Status{}, f.volErr
	}
	f.volumes = append(f.volumes, volume)
	status := f.status
	status.Volume = volume
	return status, nil
}

func (f *fakeManager) Status(deviceID string) (engine.Status, bool) { return f.status, f.haveSt }

func (f *fakeManager) ActiveSessions() int { return len(f.actions) }

func (f *fakeManager) ActiveDeviceID() string { return f.status.DeviceID }

func newTestServer(t *testing.T, manager *fakeManager, token string) (*Server, *discovery.Registry) {
	t.Helper()
	registry := discovery.NewRegistry(60 * time.Second)
	server := New(Options{
		Registry:         registry,
		Manager:          manager,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Token:            token,
		ServerURL:        "http://192.168.1.50:8080",
		DiscoveryEnabled: true,
	})
	return server, registry
}

func do(t *testing.T, handler http.Handler, method, path, body, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("x-bridge-token", token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	payload := map[string]any{}
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	}
	return recorder, payload
}

func TestHealthzIsUnauthenticatedAndDocumentedShape(t *testing.T) {
	manager := &fakeManager{kind: "cliairplay"}
	server, _ := newTestServer(t, manager, "secret")
	handler := server.Routes()

	recorder, payload := do(t, handler, http.MethodGet, "/healthz", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz returned %d", recorder.Code)
	}
	if payload["status"] != "ok" {
		t.Errorf("status = %v", payload["status"])
	}
	if payload["version"] != Version {
		t.Errorf("version = %v", payload["version"])
	}
	// The architecture document's example reports this label.
	if payload["engine"] != "airplay-cli-unified" {
		t.Errorf("engine = %v, want airplay-cli-unified", payload["engine"])
	}
	if payload["kernel"] != "cliairplay" {
		t.Errorf("kernel = %v", payload["kernel"])
	}
}

func TestHealthzReportsNativeKernelLabel(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "")
	recorder, payload := do(t, server.Routes(), http.MethodGet, "/healthz", "", "")
	if payload["engine"] != "lyranest-native-raop" {
		t.Errorf("engine = %v", payload["engine"])
	}
	if payload["kernel"] != "native" {
		t.Errorf("kernel = %v", payload["kernel"])
	}
	_ = recorder
}

func TestBridgeTokenIsEnforced(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "s3cret")
	handler := server.Routes()

	for _, path := range []string{"/api/status", "/api/devices", "/api/player/status"} {
		recorder, _ := do(t, handler, http.MethodGet, path, "", "")
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token returned %d, want 401", path, recorder.Code)
		}
	}

	recorder, _ := do(t, handler, http.MethodGet, "/api/status", "", "s3cret")
	if recorder.Code != http.StatusOK {
		t.Errorf("GET /api/status with the token returned %d", recorder.Code)
	}

	// Bearer form must work too, because that is what the LyraNest proxy sends.
	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.Header.Set("Authorization", "Bearer s3cret")
	bearerRecorder := httptest.NewRecorder()
	handler.ServeHTTP(bearerRecorder, request)
	if bearerRecorder.Code != http.StatusOK {
		t.Errorf("bearer auth returned %d", bearerRecorder.Code)
	}

	recorder, _ = do(t, handler, http.MethodGet, "/api/status", "", "wrong")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("wrong token returned %d, want 401", recorder.Code)
	}
}

func TestDevicesEndpointShape(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, registry := newTestServer(t, manager, "")
	registry.Upsert(discovery.Device{
		ID:               "40:5B:D8:12:34:56@LivingRoom",
		MAC:              "40:5B:D8:12:34:56",
		Name:             "客厅 HomePod mini",
		Model:            "AudioAccessory5,1",
		Address:          "192.168.1.105",
		Port:             7000,
		Features:         "0x4A7FCA00,0x3C356BD0",
		Protocol:         "airplay2",
		RequiresPassword: false,
		LastSeen:         time.Date(2026, 9, 24, 10, 45, 0, 0, time.UTC),
		Source:           discovery.SourceMDNS,
		TXT:              map[string]string{"cn": "0,1"},
	})

	recorder, payload := do(t, server.Routes(), http.MethodGet, "/api/devices", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("devices returned %d", recorder.Code)
	}
	devices, ok := payload["devices"].([]any)
	if !ok || len(devices) != 1 {
		t.Fatalf("unexpected devices payload: %v", payload)
	}
	device := devices[0].(map[string]any)
	for _, field := range []string{"id", "name", "model", "address", "port", "features", "protocol", "requires_password", "is_busy", "last_seen"} {
		if _, present := device[field]; !present {
			t.Errorf("device document is missing %q", field)
		}
	}
	if device["last_seen"] != "2026-09-24T10:45:00Z" {
		t.Errorf("last_seen = %v", device["last_seen"])
	}
	if device["name"] != "客厅 HomePod mini" {
		t.Errorf("name = %v", device["name"])
	}
	if device["protocol"] != "airplay2" {
		t.Errorf("protocol = %v", device["protocol"])
	}
}

func TestDevicesEmptyIsAnEmptyArray(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "")
	recorder, payload := do(t, server.Routes(), http.MethodGet, "/api/devices", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("devices returned %d", recorder.Code)
	}
	if body := strings.TrimSpace(recorder.Body.String()); body != `{"devices":[]}` {
		t.Errorf("empty registry must serialise as an empty array, got %s", body)
	}
	_ = payload
}

func TestManualDeviceRegistration(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "")
	handler := server.Routes()

	recorder, payload := do(t, handler, http.MethodPost, "/api/devices/manual", `{"address":"127.0.0.1","port":7000}`, "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("manual registration returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if payload["success"] != true {
		t.Errorf("success = %v", payload["success"])
	}

	recorder, _ = do(t, handler, http.MethodPost, "/api/devices/manual", `{"address":""}`, "")
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("empty address returned %d, want 400", recorder.Code)
	}

	recorder, _ = do(t, handler, http.MethodPost, "/api/devices/manual", `not json`, "")
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("bad json returned %d, want 400", recorder.Code)
	}
}

func TestPlayForwardsTheDocumentedRequest(t *testing.T) {
	manager := &fakeManager{kind: "native", status: engine.Status{Active: true, State: engine.StateBuffering, DeviceID: "40:5B:D8:12:34:56@LivingRoom"}}
	server, _ := newTestServer(t, manager, "")

	body := `{"device_id":"40:5B:D8:12:34:56@LivingRoom","stream_url":"/api/v1/tracks/1024/stream?access_token=xyz","track_id":1024,"title":"晴天","artist":"周杰伦","album":"叶惠美","duration_ms":269000,"start_position_ms":0,"volume":60}`
	recorder, payload := do(t, server.Routes(), http.MethodPost, "/api/player/play", body, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("play returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if payload["success"] != true {
		t.Errorf("success = %v", payload["success"])
	}
	if payload["state"] != "buffering" {
		t.Errorf("state = %v", payload["state"])
	}
	if payload["device_id"] != "40:5B:D8:12:34:56@LivingRoom" {
		t.Errorf("device_id = %v", payload["device_id"])
	}
	if len(manager.started) != 1 {
		t.Fatalf("expected one Start call, got %d", len(manager.started))
	}
	request := manager.started[0]
	if request.TrackID != 1024 || request.Title != "晴天" || request.Artist != "周杰伦" || request.Album != "叶惠美" {
		t.Errorf("metadata not forwarded: %+v", request)
	}
	if request.DurationMS != 269000 || request.Volume != 60 {
		t.Errorf("duration/volume not forwarded: %+v", request)
	}
	// A relative stream URL must be resolved against the LyraNest server.
	if request.StreamURL != "http://192.168.1.50:8080/api/v1/tracks/1024/stream?access_token=xyz" {
		t.Errorf("stream_url = %q", request.StreamURL)
	}
}

func TestPlayAbsoluteStreamURLIsPreserved(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "")
	body := `{"device_id":"dev","stream_url":"https://cdn.example.test/a.mp3","volume":0}`
	recorder, _ := do(t, server.Routes(), http.MethodPost, "/api/player/play", body, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("play returned %d", recorder.Code)
	}
	if got := manager.started[0].StreamURL; got != "https://cdn.example.test/a.mp3" {
		t.Errorf("stream_url = %q", got)
	}
	// An explicit 0 volume is a mute request and must survive.
	if got := manager.started[0].Volume; got != 0 {
		t.Errorf("volume = %d, want an explicit 0", got)
	}
}

func TestPlayValidation(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "")
	handler := server.Routes()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing device", `{"stream_url":"http://x/a.mp3"}`, http.StatusBadRequest},
		{"missing stream", `{"device_id":"dev"}`, http.StatusBadRequest},
		{"bad json", `{`, http.StatusBadRequest},
	}
	for _, testCase := range cases {
		recorder, _ := do(t, handler, http.MethodPost, "/api/player/play", testCase.body, "")
		if recorder.Code != testCase.want {
			t.Errorf("%s: got %d, want %d", testCase.name, recorder.Code, testCase.want)
		}
	}
}

// TestPlayAcceptsTrackIDAsNumberOrString covers the cross-service contract: the
// LyraNest gateway forwards track_id as a string, while the published REST
// example uses a number.
func TestPlayAcceptsTrackIDAsNumberOrString(t *testing.T) {
	for _, body := range []string{
		`{"device_id":"dev","stream_url":"http://x/a.mp3","track_id":1024}`,
		`{"device_id":"dev","stream_url":"http://x/a.mp3","track_id":"1024"}`,
	} {
		manager := &fakeManager{kind: "native"}
		server, _ := newTestServer(t, manager, "")
		recorder, _ := do(t, server.Routes(), http.MethodPost, "/api/player/play", body, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("body %s returned %d: %s", body, recorder.Code, recorder.Body.String())
		}
		if len(manager.started) != 1 || manager.started[0].TrackID != 1024 {
			t.Fatalf("track_id was not forwarded: %+v", manager.started)
		}
	}
}

func TestPlayErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{engine.ErrAlreadyActive, http.StatusConflict},
		{engine.ErrNotSupported, http.StatusNotImplemented},
		{errors.New(`device "x" not found`), http.StatusNotFound},
		{errors.New("boom"), http.StatusBadGateway},
	}
	for _, testCase := range cases {
		manager := &fakeManager{kind: "native", startErr: testCase.err}
		server, _ := newTestServer(t, manager, "")
		recorder, _ := do(t, server.Routes(), http.MethodPost, "/api/player/play", `{"device_id":"d","stream_url":"http://x/a.mp3"}`, "")
		if recorder.Code != testCase.want {
			t.Errorf("error %v mapped to %d, want %d", testCase.err, recorder.Code, testCase.want)
		}
	}
}

func TestControlActions(t *testing.T) {
	manager := &fakeManager{kind: "native", status: engine.Status{State: engine.StatePaused, DeviceID: "dev", PositionMS: 65000}}
	server, _ := newTestServer(t, manager, "")
	handler := server.Routes()

	recorder, payload := do(t, handler, http.MethodPost, "/api/player/control", `{"device_id":"dev","action":"pause"}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("pause returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if payload["state"] != "paused" {
		t.Errorf("state = %v", payload["state"])
	}
	if payload["current_position_ms"] != float64(65000) {
		t.Errorf("current_position_ms = %v", payload["current_position_ms"])
	}

	for _, action := range []string{"pause", "resume", "stop", "seek"} {
		body := `{"device_id":"dev","action":"` + action + `","position_ms":1000}`
		recorder, _ := do(t, handler, http.MethodPost, "/api/player/control", body, "")
		if recorder.Code != http.StatusOK {
			t.Errorf("action %s returned %d", action, recorder.Code)
		}
	}

	// Unknown actions are rejected before reaching the kernel.
	recorder, _ = do(t, handler, http.MethodPost, "/api/player/control", `{"device_id":"dev","action":"explode"}`, "")
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("unknown action returned %d, want 400", recorder.Code)
	}
	// One explicit pause plus the four documented actions, and nothing for the
	// rejected one.
	if len(manager.actions) != 5 {
		t.Errorf("kernel saw %d actions, want 5: %v", len(manager.actions), manager.actions)
	}
}

func TestControlMissingSession(t *testing.T) {
	manager := &fakeManager{kind: "native", ctrlErr: engine.ErrNoSession}
	server, _ := newTestServer(t, manager, "")
	recorder, _ := do(t, server.Routes(), http.MethodPost, "/api/player/control", `{"device_id":"dev","action":"pause"}`, "")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("missing session returned %d, want 404", recorder.Code)
	}
}

func TestVolumeMappingAndValidation(t *testing.T) {
	manager := &fakeManager{kind: "native", status: engine.Status{DeviceID: "dev"}}
	server, _ := newTestServer(t, manager, "")
	handler := server.Routes()

	recorder, payload := do(t, handler, http.MethodPost, "/api/player/volume", `{"device_id":"dev","volume":75}`, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("volume returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if payload["volume"] != float64(75) {
		t.Errorf("volume = %v", payload["volume"])
	}
	if len(manager.volumes) != 1 || manager.volumes[0] != 75 {
		t.Errorf("kernel volumes = %v", manager.volumes)
	}

	for _, body := range []string{
		`{"device_id":"dev"}`,
		`{"device_id":"dev","volume":-1}`,
		`{"device_id":"dev","volume":101}`,
		`{`,
	} {
		recorder, _ := do(t, handler, http.MethodPost, "/api/player/volume", body, "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %s returned %d, want 400", body, recorder.Code)
		}
	}
}

func TestPlayerStatusIdleDocument(t *testing.T) {
	manager := &fakeManager{kind: "native", haveSt: false}
	server, _ := newTestServer(t, manager, "")
	recorder, payload := do(t, server.Routes(), http.MethodGet, "/api/player/status", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("idle status returned %d", recorder.Code)
	}
	if payload["active"] != false {
		t.Errorf("active = %v", payload["active"])
	}
	if payload["state"] != "stopped" {
		t.Errorf("state = %v", payload["state"])
	}
	if payload["error"] != nil {
		t.Errorf("error = %v, want null", payload["error"])
	}
}

func TestPlayerStatusActiveDocument(t *testing.T) {
	manager := &fakeManager{
		kind:   "native",
		haveSt: true,
		status: engine.Status{
			Active:     true,
			State:      engine.StatePlaying,
			DeviceID:   "40:5B:D8:12:34:56@LivingRoom",
			TrackID:    1024,
			Title:      "晴天",
			Artist:     "周杰伦",
			DurationMS: 269000,
			PositionMS: 78200,
			Volume:     75,
		},
	}
	server, _ := newTestServer(t, manager, "")
	recorder, payload := do(t, server.Routes(), http.MethodGet, "/api/player/status?device_id=40:5B:D8:12:34:56@LivingRoom", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status returned %d", recorder.Code)
	}
	if payload["state"] != "playing" || payload["title"] != "晴天" {
		t.Errorf("unexpected payload: %v", payload)
	}
	if payload["position_ms"] != float64(78200) || payload["duration_ms"] != float64(269000) {
		t.Errorf("progress fields wrong: %v", payload)
	}
	if payload["error"] != nil {
		t.Errorf("error = %v, want null", payload["error"])
	}
}

func TestStatusEndpoint(t *testing.T) {
	manager := &fakeManager{kind: "native", status: engine.Status{DeviceID: "dev"}}
	server, registry := newTestServer(t, manager, "")
	registry.Upsert(discovery.Device{ID: "dev", Name: "dev", LastSeen: time.Now()})

	recorder, payload := do(t, server.Routes(), http.MethodGet, "/api/status", "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status returned %d", recorder.Code)
	}
	if payload["devices_count"] != float64(1) {
		t.Errorf("devices_count = %v", payload["devices_count"])
	}
	if payload["active_device_id"] != "dev" {
		t.Errorf("active_device_id = %v", payload["active_device_id"])
	}
	if _, ok := payload["uptime_seconds"]; !ok {
		t.Errorf("uptime_seconds missing")
	}
}

func TestCORSAndPreflight(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "")
	handler := server.Routes()

	request := httptest.NewRequest(http.MethodOptions, "/api/devices", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight returned %d", recorder.Code)
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("missing CORS origin header")
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "")
	recorder, _ := do(t, server.Routes(), http.MethodGet, "/api/nope", "", "")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("unknown route returned %d, want 404", recorder.Code)
	}
}

func TestResolveStreamURLEdgeCases(t *testing.T) {
	manager := &fakeManager{kind: "native"}
	server, _ := newTestServer(t, manager, "")

	if got := server.resolveStreamURL(""); got != "" {
		t.Errorf("empty url = %q", got)
	}
	if got := server.resolveStreamURL("http://a/b.mp3"); got != "http://a/b.mp3" {
		t.Errorf("absolute url = %q", got)
	}
	if got := server.resolveStreamURL("/api/v1/tracks/1/stream?access_token=t"); got != "http://192.168.1.50:8080/api/v1/tracks/1/stream?access_token=t" {
		t.Errorf("relative url = %q", got)
	}

	bare := New(Options{Registry: discovery.NewRegistry(0), Manager: manager, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if got := bare.resolveStreamURL("/relative"); got != "/relative" {
		t.Errorf("without a server URL the value must pass through, got %q", got)
	}
}

func TestWantsRefresh(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", " yes "} {
		if !wantsRefresh(value) {
			t.Errorf("wantsRefresh(%q) = false", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "maybe"} {
		if wantsRefresh(value) {
			t.Errorf("wantsRefresh(%q) = true", value)
		}
	}
}
