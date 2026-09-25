package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lyranest/lyranest-airplay-bridge/internal/audio"
	"github.com/lyranest/lyranest-airplay-bridge/internal/raop"
)

// NativeOptions configures the built-in RAOP kernel.
type NativeOptions struct {
	// FFmpegPath decodes the LyraNest stream URL into raw PCM.
	FFmpegPath string
	// SampleRate / Channels describe the PCM handed to the receiver.
	SampleRate int
	Channels   int
	// IfaceIP optionally pins the sender sockets to one local interface.
	IfaceIP string
	// Logger receives kernel diagnostics.
	Logger *slog.Logger
	// Headers are extra HTTP headers passed to ffmpeg.
	Headers map[string]string
}

// NativeEngine streams to a receiver with the bridge's own RAOP sender.
type NativeEngine struct {
	sessionBase
	target Target
	opts   NativeOptions

	cancel context.CancelFunc
	source *audio.SwitchableReader
	sender *raop.Sender

	runMu   sync.Mutex
	running bool
	doneCh  chan struct{}
}

// NewNativeEngine creates a kernel for one device.
func NewNativeEngine(target Target, volume int, opts NativeOptions) *NativeEngine {
	if opts.SampleRate <= 0 {
		opts.SampleRate = 44100
	}
	if opts.Channels <= 0 {
		opts.Channels = 2
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	engine := &NativeEngine{
		sessionBase: newSessionBase("native", target.ID, target.Name),
		target:      target,
		opts:        opts,
	}
	engine.request.Volume = volume
	return engine
}

// Kind implements Engine.
func (e *NativeEngine) Kind() string { return "native" }

// Start opens the receiver session and begins streaming.
func (e *NativeEngine) Start(ctx context.Context, request PlayRequest) error {
	if !e.target.AcceptsPCM() {
		return fmt.Errorf("%w: %s advertises RAOP codecs %v; install cliairplay or select the cliairplay engine",
			raop.ErrALACOnly, e.target.Name, e.target.Codecs())
	}
	if request.Volume < 0 || request.Volume > 100 {
		request.Volume = 60
	}

	e.mu.Lock()
	e.request = request
	e.state = StateBuffering
	e.mu.Unlock()

	streamCtx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel

	format := audio.Format{
		SampleRate: e.opts.SampleRate,
		Channels:   e.opts.Channels,
		BitDepth:   16,
		// RAOP L16 is network byte order, i.e. big-endian PCM.
		BigEndian: true,
	}

	decoder, err := audio.NewDecoder(streamCtx, request.StreamURL, audio.DecoderOptions{
		FFmpegPath:      e.opts.FFmpegPath,
		Format:          format,
		StartPositionMS: request.StartPositionMS,
		Headers:         e.opts.Headers,
		Logger: func(message string, args ...any) {
			e.opts.Logger.Warn("decoder failed", "device", e.target.Name, "detail", fmt.Sprintf(message, args...))
		},
	})
	if err != nil {
		cancel()
		e.setError(err)
		return err
	}

	source := audio.NewSwitchableReader(decoder)
	e.source = source

	sender, err := raop.Dial(streamCtx, raop.Config{
		Address: e.target.Address,
		Port:    e.target.Port,
		Format: raop.AudioFormat{
			SampleRate: e.opts.SampleRate,
			Channels:   e.opts.Channels,
			BitDepth:   16,
		},
		LocalIP: e.opts.IfaceIP,
		Volume:  request.Volume,
		Logger:  e.opts.Logger,
		OnProgress: func(frames uint64) {
			e.mu.Lock()
			state := e.state
			e.mu.Unlock()
			if state == StateBuffering {
				e.setState(StatePlaying)
			}
		},
	})
	if err != nil {
		_ = source.Close()
		cancel()
		e.setError(err)
		return err
	}
	e.sender = sender

	if request.Volume > 0 {
		if err := sender.SetVolume(request.Volume); err != nil {
			e.opts.Logger.Warn("initial volume failed", "error", err)
		}
	}
	if err := sender.Record(streamCtx); err != nil {
		_ = sender.Close()
		_ = source.Close()
		cancel()
		e.setError(err)
		return err
	}

	// The session stays in "buffering" until the first second of audio has
	// actually reached the receiver, matching the documented play response.
	e.setState(StateBuffering)
	e.beginPlaybackClock()

	done := make(chan struct{})
	e.doneCh = done
	go e.stream(streamCtx, sender, source, done)
	return nil
}

func (e *NativeEngine) stream(ctx context.Context, sender *raop.Sender, source *audio.SwitchableReader, done chan struct{}) {
	defer close(done)

	err := sender.Stream(ctx, source)
	switch {
	case err == nil:
		e.opts.Logger.Info("stream finished", "device", e.target.Name, "frames", sender.FramesSent())
		e.markFinished()
	case ctx.Err() != nil:
		// Cancelled by Stop/Close: the state was already set there.
		e.opts.Logger.Debug("stream cancelled", "device", e.target.Name, "frames", sender.FramesSent())
	default:
		e.opts.Logger.Error("stream failed", "device", e.target.Name, "frames", sender.FramesSent(), "error", err)
		e.setError(err)
	}
	_ = sender.Close()
	_ = source.Close()
}

// Control implements Engine.
func (e *NativeEngine) Control(action string, positionMS int64) error {
	e.runMu.Lock()
	sender := e.sender
	source := e.source
	e.runMu.Unlock()

	switch action {
	case ActionPause:
		if sender == nil {
			return ErrNoSession
		}
		sender.Pause()
		return nil

	case ActionResume:
		if sender == nil {
			return ErrNoSession
		}
		sender.Resume()
		return nil

	case ActionStop:
		return e.Close()

	case ActionSeek:
		if sender == nil || source == nil {
			return ErrNoSession
		}
		if positionMS < 0 {
			positionMS = 0
		}
		decoder, err := audio.NewDecoder(context.Background(), e.currentStreamURL(), audio.DecoderOptions{
			FFmpegPath:      e.opts.FFmpegPath,
			Format:          audio.Format{SampleRate: e.opts.SampleRate, Channels: e.opts.Channels, BitDepth: 16, BigEndian: true},
			StartPositionMS: positionMS,
			Headers:         e.opts.Headers,
		})
		if err != nil {
			return fmt.Errorf("seek: %w", err)
		}
		source.Swap(decoder)
		sender.RequestFlush()
		e.resetPosition(positionMS)
		return nil

	default:
		return fmt.Errorf("%w: %s", ErrNotSupported, action)
	}
}

// SetVolume implements Engine.
func (e *NativeEngine) SetVolume(percent int) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	e.mu.Lock()
	e.request.Volume = percent
	e.mu.Unlock()

	e.runMu.Lock()
	sender := e.sender
	e.runMu.Unlock()
	if sender == nil {
		// Volume before a session is recorded and applied on Start.
		return nil
	}
	return sender.SetVolume(percent)
}

// Status implements Engine.
func (e *NativeEngine) Status() Status {
	status := e.snapshot()
	if e.sender != nil {
		if state, err := e.sender.State(); err == nil {
			switch state {
			case raop.StatePaused:
				status.State = StatePaused
			case raop.StateStopped:
				if status.State != StateError {
					status.State = StateStopped
				}
			}
		}
	}
	return status
}

// Close implements Engine.
func (e *NativeEngine) Close() error {
	e.runMu.Lock()
	sender := e.sender
	source := e.source
	done := e.doneCh
	e.runMu.Unlock()

	e.setState(StateStopped)

	if e.cancel != nil {
		e.cancel()
	}
	if sender != nil {
		_ = sender.Close()
	}
	if source != nil {
		_ = source.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	return nil
}

func (e *NativeEngine) currentStreamURL() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.request.StreamURL
}

// nativeSink is a compile-time assertion that the kernel satisfies Engine.
var _ Engine = (*NativeEngine)(nil)
