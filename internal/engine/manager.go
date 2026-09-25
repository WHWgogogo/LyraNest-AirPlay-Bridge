package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/lyranest/lyranest-airplay-bridge/internal/discovery"
)

// ManagerConfig describes how kernels should be created.
type ManagerConfig struct {
	// Engine is auto, cliairplay or native.
	Engine string
	// CLIAirplayPath is the external kernel binary.
	CLIAirplayPath string
	// FFmpegPath decodes stream URLs into PCM.
	FFmpegPath string
	// SampleRate / Channels / BitDepth describe the PCM handed to the kernel.
	SampleRate int
	Channels   int
	BitDepth   int
	// LatencyMS is the receiver queue depth hint.
	LatencyMS int
	// IfaceIP pins kernel sockets to one local interface.
	IfaceIP string
	// PublishIP is the address advertised to receivers behind NAT.
	PublishIP string
	// Protocol is the cliairplay route selection hint.
	Protocol string
	// DefaultVolume is used when a play request carries no volume.
	DefaultVolume int
	// FFmpegHeaders are extra HTTP headers for the stream fetch.
	FFmpegHeaders map[string]string
}

// Manager owns every live streaming session and picks the kernel to use.
type Manager struct {
	registry *discovery.Registry
	cfg      ManagerConfig
	logger   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*managedSession
	// resolvedKind caches the kernel chosen by `auto` after the first probe.
	resolvedKind string
}

type managedSession struct {
	engine    Engine
	target    Target
	startedAt time.Time
}

// NewManager creates a session manager.
func NewManager(registry *discovery.Registry, cfg ManagerConfig, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		registry: registry,
		cfg:      cfg,
		logger:   logger,
		sessions: make(map[string]*managedSession),
	}
}

// EngineName reports the kernel the manager will use, resolving `auto`.
func (m *Manager) EngineName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolveKindLocked()
}

func (m *Manager) resolveKindLocked() string {
	if m.resolvedKind != "" {
		return m.resolvedKind
	}
	switch m.cfg.Engine {
	case "cliairplay":
		m.resolvedKind = "cliairplay"
	case "native":
		m.resolvedKind = "native"
	default:
		probe := NewCLIAirplayEngine(Target{}, 0, CLIAirplayOptions{
			BinaryPath: m.cfg.CLIAirplayPath,
			Logger:     m.logger,
		})
		if probe.Available() {
			m.resolvedKind = "cliairplay"
		} else {
			m.resolvedKind = "native"
			m.logger.Info("cliairplay kernel not found, using the built-in native RAOP kernel",
				"cliairplay_path", m.cfg.CLIAirplayPath)
		}
	}
	return m.resolvedKind
}

// Start resolves the device and begins a session.
func (m *Manager) Start(ctx context.Context, deviceID string, request PlayRequest) (Status, error) {
	target, err := m.resolveTarget(deviceID)
	if err != nil {
		return Status{}, err
	}

	m.mu.Lock()
	if existing, ok := m.sessions[target.ID]; ok {
		state := existing.engine.Status().State
		if state == StatePlaying || state == StateBuffering || state == StatePaused {
			m.mu.Unlock()
			return Status{}, fmt.Errorf("%w: %s", ErrAlreadyActive, target.Name)
		}
		delete(m.sessions, target.ID)
	}
	kind := m.resolveKindLocked()
	m.mu.Unlock()

	// A negative volume means "not supplied by the caller": fall back to the
	// bridge default. An explicit 0 is a real mute request and is preserved.
	if request.Volume < 0 {
		request.Volume = m.cfg.DefaultVolume
	}
	if request.Volume > 100 {
		request.Volume = 100
	}

	var kernel Engine
	switch kind {
	case "cliairplay":
		kernel = NewCLIAirplayEngine(target, request.Volume, CLIAirplayOptions{
			BinaryPath: m.cfg.CLIAirplayPath,
			FFmpegPath: m.cfg.FFmpegPath,
			SampleRate: m.cfg.SampleRate,
			Channels:   m.cfg.Channels,
			BitDepth:   m.cfg.BitDepth,
			LatencyMS:  m.cfg.LatencyMS,
			IfaceIP:    m.cfg.IfaceIP,
			PublishIP:  m.cfg.PublishIP,
			Protocol:   m.cfg.Protocol,
			Headers:    m.cfg.FFmpegHeaders,
			Logger:     m.logger,
		})
	default:
		kernel = NewNativeEngine(target, request.Volume, NativeOptions{
			FFmpegPath: m.cfg.FFmpegPath,
			SampleRate: m.cfg.SampleRate,
			Channels:   m.cfg.Channels,
			IfaceIP:    m.cfg.IfaceIP,
			Headers:    m.cfg.FFmpegHeaders,
			Logger:     m.logger,
		})
	}

	if err := kernel.Start(ctx, request); err != nil {
		_ = kernel.Close()
		return Status{}, err
	}

	m.mu.Lock()
	m.sessions[target.ID] = &managedSession{engine: kernel, target: target, startedAt: time.Now()}
	m.mu.Unlock()

	if m.registry != nil {
		m.registry.SetBusy(target.ID, true)
	}
	m.logger.Info("streaming session started",
		"device", target.Name,
		"device_id", target.ID,
		"engine", kernel.Kind(),
		"track_id", request.TrackID,
	)
	return kernel.Status(), nil
}

// Control applies a transport action to a device's session.
func (m *Manager) Control(deviceID, action string, positionMS int64) (Status, error) {
	session, err := m.session(deviceID)
	if err != nil {
		return Status{}, err
	}
	if err := session.engine.Control(action, positionMS); err != nil {
		return Status{}, err
	}
	if action == ActionStop {
		m.forget(session.target.ID)
	}
	return session.engine.Status(), nil
}

// SetVolume applies a 0..100 volume to a device's session.
func (m *Manager) SetVolume(deviceID string, volume int) (Status, error) {
	session, err := m.session(deviceID)
	if err != nil {
		return Status{}, err
	}
	if err := session.engine.SetVolume(volume); err != nil {
		return Status{}, err
	}
	return session.engine.Status(), nil
}

// Status returns the session status for a device. An empty deviceID selects
// the most recently started session.
func (m *Manager) Status(deviceID string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.sessions) == 0 {
		return Status{}, false
	}
	if deviceID == "" {
		mostRecent := m.mostRecentLocked()
		return mostRecent.engine.Status(), true
	}
	if session, ok := m.sessions[deviceID]; ok {
		return session.engine.Status(), true
	}
	needle := normalizeID(deviceID)
	for id, session := range m.sessions {
		if normalizeID(id) == needle || normalizeID(session.target.Name) == needle || session.target.Address == deviceID {
			return session.engine.Status(), true
		}
	}
	return Status{}, false
}

// ActiveSessions returns the number of live sessions.
func (m *Manager) ActiveSessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	return len(m.sessions)
}

// ActiveDeviceID returns the device id of the most recent live session.
func (m *Manager) ActiveDeviceID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) == 0 {
		return ""
	}
	return m.mostRecentLocked().target.ID
}

// SessionEngine reports which kernel serves a device.
func (m *Manager) SessionEngine(deviceID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[deviceID]; ok {
		return session.engine.Kind()
	}
	return ""
}

func (m *Manager) mostRecentLocked() *managedSession {
	var newest *managedSession
	for _, session := range m.sessions {
		if newest == nil || session.startedAt.After(newest.startedAt) {
			newest = session
		}
	}
	return newest
}

func (m *Manager) session(deviceID string) (*managedSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.sessions) == 0 {
		return nil, ErrNoSession
	}
	if deviceID == "" {
		return m.mostRecentLocked(), nil
	}
	if session, ok := m.sessions[deviceID]; ok {
		return session, nil
	}
	needle := normalizeID(deviceID)
	for id, session := range m.sessions {
		if normalizeID(id) == needle || normalizeID(session.target.Name) == needle || session.target.Address == deviceID {
			return session, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoSession, deviceID)
}

func (m *Manager) forget(deviceID string) {
	m.mu.Lock()
	session, ok := m.sessions[deviceID]
	if ok {
		delete(m.sessions, deviceID)
	}
	m.mu.Unlock()

	if ok && m.registry != nil {
		m.registry.SetBusy(deviceID, false)
	}
	_ = session
}

// pruneLocked drops sessions whose kernel reached a terminal state.
func (m *Manager) pruneLocked() {
	for id, session := range m.sessions {
		status := session.engine.Status()
		if status.State == StateStopped || status.State == StateError {
			_ = session.engine.Close()
			delete(m.sessions, id)
			if m.registry != nil {
				m.registry.SetBusy(id, false)
			}
		}
	}
}

// Reap periodically closes finished sessions so /api/status stays truthful.
func (m *Manager) Reap(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			m.pruneLocked()
			m.mu.Unlock()
		}
	}
}

// Shutdown stops every session.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	sessions := make([]*managedSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = make(map[string]*managedSession)
	m.mu.Unlock()

	for _, session := range sessions {
		_ = session.engine.Close()
		if m.registry != nil {
			m.registry.SetBusy(session.target.ID, false)
		}
	}
}

// Targets returns the device snapshot the manager resolves against.
func (m *Manager) Targets() []Target {
	if m.registry == nil {
		return nil
	}
	devices := m.registry.List()
	targets := make([]Target, 0, len(devices))
	for _, device := range devices {
		targets = append(targets, TargetFromDevice(device))
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets
}

func (m *Manager) resolveTarget(deviceID string) (Target, error) {
	if m.registry == nil {
		return Target{}, errors.New("device registry unavailable")
	}
	device, ok := m.registry.Get(deviceID)
	if !ok {
		return Target{}, fmt.Errorf("device %q not found", deviceID)
	}
	return TargetFromDevice(device), nil
}

func normalizeID(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range value {
		switch r {
		case ':', '-', ' ', '.':
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out)
}
