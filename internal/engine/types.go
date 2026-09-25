// Package engine hosts the audio push kernels the bridge can drive.
//
// Two kernels ship with the bridge:
//
//   - cliairplay (external): the Music Assistant unified AirPlay 1 + AirPlay 2
//     CLI. It is the recommended kernel for real HomePods because it already
//     absorbs the firmware quirks of the AirPlay 2 native flow.
//   - native (built-in): a pure-Go RAOP sender that needs no external binary
//     and therefore keeps the bridge usable on hosts where cliairplay has no
//     build (Windows, musl-less NAS images).
package engine

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State is the lifecycle state of a streaming session.
type State string

// Session lifecycle states. The string values are part of the REST contract.
const (
	StateBuffering State = "buffering"
	StatePlaying   State = "playing"
	StatePaused    State = "paused"
	StateStopped   State = "stopped"
	StateError     State = "error"
)

// Control actions accepted by /api/player/control.
const (
	ActionPause  = "pause"
	ActionResume = "resume"
	ActionStop   = "stop"
	ActionSeek   = "seek"
)

// Errors returned by engines.
var (
	// ErrNotSupported is returned for an action a kernel cannot perform.
	ErrNotSupported = errors.New("action not supported by this engine")
	// ErrNoSession means no session exists for the requested device.
	ErrNoSession = errors.New("no active session for device")
	// ErrAlreadyActive means a session already exists for the device.
	ErrAlreadyActive = errors.New("device already has an active session")
)

// PlayRequest is a fully resolved playback request.
type PlayRequest struct {
	StreamURL       string
	TrackID         int64
	Title           string
	Artist          string
	Album           string
	DurationMS      int64
	StartPositionMS int64
	Volume          int
}

// Status is the observable state of one session.
type Status struct {
	Active     bool      `json:"active"`
	State      State     `json:"state"`
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name,omitempty"`
	TrackID    int64     `json:"track_id"`
	Title      string    `json:"title,omitempty"`
	Artist     string    `json:"artist,omitempty"`
	Album      string    `json:"album,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	PositionMS int64     `json:"position_ms"`
	Volume     int       `json:"volume"`
	Engine     string    `json:"engine,omitempty"`
	Error      *string   `json:"error"`
	StartedAt  time.Time `json:"started_at,omitempty"`
}

// Engine drives one streaming session for one device.
type Engine interface {
	// Kind is the kernel identifier, e.g. "cliairplay" or "native".
	Kind() string
	// Start begins streaming. It returns once the session is established, not
	// when playback finishes.
	Start(ctx context.Context, request PlayRequest) error
	// Control applies a transport action.
	Control(action string, positionMS int64) error
	// SetVolume applies a 0..100 volume.
	SetVolume(percent int) error
	// Status returns a snapshot.
	Status() Status
	// Close stops streaming and releases every resource.
	Close() error
}

// sessionBase carries the state every kernel implementation shares.
type sessionBase struct {
	kind       string
	deviceID   string
	deviceName string

	mu        sync.Mutex
	state     State
	request   PlayRequest
	err       error
	startedAt time.Time
	// playingSince marks when the session last entered the playing state;
	// position is derived from it so a pause freezes the progress clock.
	playingSince time.Time
	accumulated  time.Duration
	finished     bool
}

func newSessionBase(kind, deviceID, deviceName string) sessionBase {
	return sessionBase{
		kind:       kind,
		deviceID:   deviceID,
		deviceName: deviceName,
		state:      StateBuffering,
		startedAt:  time.Now(),
	}
}

func (s *sessionBase) setState(state State) {
	s.mu.Lock()
	s.setStateLocked(state)
	s.mu.Unlock()
}

func (s *sessionBase) setStateLocked(state State) {
	now := time.Now()
	if s.state == StatePlaying && state != StatePlaying {
		s.accumulated += now.Sub(s.playingSince)
		s.playingSince = time.Time{}
	}
	if state == StatePlaying && s.playingSince.IsZero() {
		s.playingSince = now
	}
	s.state = state
}

func (s *sessionBase) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		return
	}
	s.err = err
	s.setStateLocked(StateError)
}

// markFinished records that the source reached its end.
func (s *sessionBase) markFinished() {
	s.mu.Lock()
	s.finished = true
	s.setStateLocked(StateStopped)
	s.mu.Unlock()
}

// resetPosition re-bases the progress clock after a seek.
func (s *sessionBase) resetPosition(positionMS int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.request.StartPositionMS = positionMS
	s.accumulated = 0
	if s.state == StatePlaying {
		s.playingSince = time.Now()
	}
}

// beginPlaybackClock starts the progress clock while the session is still
// buffering, so the reported position does not lag by the buffering time.
func (s *sessionBase) beginPlaybackClock() {
	s.mu.Lock()
	if s.playingSince.IsZero() {
		s.playingSince = time.Now()
	}
	s.mu.Unlock()
}

// snapshot renders the shared fields of a Status.
func (s *sessionBase) snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := Status{
		Active:     s.state == StateBuffering || s.state == StatePlaying || s.state == StatePaused,
		State:      s.state,
		DeviceID:   s.deviceID,
		DeviceName: s.deviceName,
		TrackID:    s.request.TrackID,
		Title:      s.request.Title,
		Artist:     s.request.Artist,
		Album:      s.request.Album,
		DurationMS: s.request.DurationMS,
		Volume:     s.request.Volume,
		Engine:     s.kind,
		StartedAt:  s.startedAt,
	}
	if s.err != nil {
		message := s.err.Error()
		status.Error = &message
	}

	elapsed := s.accumulated
	if s.state == StatePlaying && !s.playingSince.IsZero() {
		elapsed += time.Since(s.playingSince)
	}
	position := s.request.StartPositionMS + elapsed.Milliseconds()
	if s.request.DurationMS > 0 && position > s.request.DurationMS {
		position = s.request.DurationMS
	}
	if position < 0 {
		position = 0
	}
	status.PositionMS = position
	return status
}
