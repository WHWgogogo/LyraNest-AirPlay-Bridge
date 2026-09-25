// Package api exposes the bridge's RESTful control plane.
//
// Every endpoint is defined by the architecture document
// docs/04-技术调研/34-lyranest-airplay-bridge架构与接口设计方案.md.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lyranest/lyranest-airplay-bridge/internal/discovery"
	"github.com/lyranest/lyranest-airplay-bridge/internal/engine"
)

// Version is the bridge version reported by /healthz and /api/status.
var Version = "0.1.0"

// SessionManager is the subset of engine.Manager the REST layer depends on.
// Declaring it here keeps the HTTP contract testable without a live kernel.
type SessionManager interface {
	EngineName() string
	Start(ctx context.Context, deviceID string, request engine.PlayRequest) (engine.Status, error)
	Control(deviceID, action string, positionMS int64) (engine.Status, error)
	SetVolume(deviceID string, volume int) (engine.Status, error)
	Status(deviceID string) (engine.Status, bool)
	ActiveSessions() int
	ActiveDeviceID() string
}

// Server holds the HTTP dependencies.
type Server struct {
	registry *discovery.Registry
	browser  *discovery.Browser
	manager  SessionManager
	logger   *slog.Logger

	token     string
	startedAt time.Time
	// serverURL is the LyraNest base URL used to resolve relative stream URLs.
	serverURL string
	// discoveryEnabled reports whether the mDNS browser is running.
	discoveryEnabled bool
}

// Options configures a Server.
type Options struct {
	Registry         *discovery.Registry
	Browser          *discovery.Browser
	Manager          SessionManager
	Logger           *slog.Logger
	Token            string
	ServerURL        string
	DiscoveryEnabled bool
}

// New creates the REST server.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		registry:         opts.Registry,
		browser:          opts.Browser,
		manager:          opts.Manager,
		logger:           logger,
		token:            strings.TrimSpace(opts.Token),
		startedAt:        time.Now(),
		serverURL:        strings.TrimRight(strings.TrimSpace(opts.ServerURL), "/"),
		discoveryEnabled: opts.DiscoveryEnabled,
	}
}

// Routes builds the HTTP handler tree.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Health is deliberately unauthenticated: it is the probe the LyraNest
	// server and container orchestrators use to decide whether the bridge is
	// alive.
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/healthz", s.healthz)

	mux.HandleFunc("GET /api/status", s.guard(s.status))
	mux.HandleFunc("GET /api/devices", s.guard(s.devices))
	mux.HandleFunc("POST /api/devices/manual", s.guard(s.addManualDevice))
	mux.HandleFunc("POST /api/player/play", s.guard(s.play))
	mux.HandleFunc("POST /api/player/control", s.guard(s.control))
	mux.HandleFunc("POST /api/player/volume", s.guard(s.volume))
	mux.HandleFunc("GET /api/player/status", s.guard(s.playerStatus))

	return s.withCORS(s.withLogging(mux))
}

// guard enforces the optional bridge token.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "invalid or missing bridge token")
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	candidate := strings.TrimSpace(r.Header.Get("x-bridge-token"))
	if candidate == "" {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
			candidate = strings.TrimSpace(header[7:])
		}
	}
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.token)) == 1
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		s.logger.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-bridge-token, X-LyraNest-Username")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// ---------------------------------------------------------------------------
// Response helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"success": false, "error": message})
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) error {
	if r.Body == nil {
		return errors.New("empty request body")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	return decoder.Decode(target)
}
