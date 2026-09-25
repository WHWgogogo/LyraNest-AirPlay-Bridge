package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lyranest/lyranest-airplay-bridge/internal/audio"
)

// CLIAirplayOptions configures the external Music Assistant kernel.
type CLIAirplayOptions struct {
	// BinaryPath is the cliairplay executable.
	BinaryPath string
	// FFmpegPath decodes the stream URL into s16le PCM on the kernel's stdin.
	FFmpegPath string
	// SampleRate / Channels describe the PCM handed to the kernel.
	SampleRate int
	Channels   int
	// BitDepth is 16 or 24.
	BitDepth int
	// LatencyMS is the receiver queue depth hint.
	LatencyMS int
	// IfaceIP pins kernel sockets to one local interface.
	IfaceIP string
	// Protocol is auto, raop, airplay2 or airplay2-compat.
	Protocol string
	// PublishIP is the address advertised to the receiver when it differs from
	// the bind address (Docker bridge / NAT).
	PublishIP string
	// Headers are extra HTTP headers passed to ffmpeg.
	Headers map[string]string
	// Logger receives kernel diagnostics.
	Logger *slog.Logger
}

// CLIAirplayEngine drives the external cliairplay kernel over a named pipe.
//
// The contract implemented here is the one documented by the upstream project:
// one long-lived process per device, raw interleaved PCM on stdin for the whole
// process lifetime, and newline-terminated KEY=VALUE commands on --cmdpipe.
type CLIAirplayEngine struct {
	sessionBase
	target Target
	opts   CLIAirplayOptions

	cancel  context.CancelFunc
	cmd     *exec.Cmd
	pipe    *cmdPipe
	source  *audio.SwitchableReader
	stderr  *tailBuffer
	stdout  *tailBuffer
	startMu sync.Mutex

	runMu   sync.Mutex
	running bool
	doneCh  chan struct{}
}

// NewCLIAirplayEngine creates a kernel for one device.
func NewCLIAirplayEngine(target Target, volume int, opts CLIAirplayOptions) *CLIAirplayEngine {
	if opts.SampleRate <= 0 {
		opts.SampleRate = 44100
	}
	if opts.Channels <= 0 {
		opts.Channels = 2
	}
	if opts.BitDepth == 0 {
		opts.BitDepth = 16
	}
	if opts.LatencyMS <= 0 {
		opts.LatencyMS = 600
	}
	if strings.TrimSpace(opts.Protocol) == "" {
		opts.Protocol = "auto"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	engine := &CLIAirplayEngine{
		sessionBase: newSessionBase("cliairplay", target.ID, target.Name),
		target:      target,
		opts:        opts,
	}
	engine.request.Volume = volume
	return engine
}

// Kind implements Engine.
func (e *CLIAirplayEngine) Kind() string { return "cliairplay" }

// Available reports whether the configured kernel binary can be executed.
func (e *CLIAirplayEngine) Available() bool {
	path := strings.TrimSpace(e.opts.BinaryPath)
	if path == "" {
		return false
	}
	if strings.ContainsAny(path, `/\`) {
		info, err := os.Stat(path)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(path)
	return err == nil
}

// Start launches cliairplay and begins feeding PCM.
func (e *CLIAirplayEngine) Start(ctx context.Context, request PlayRequest) error {
	if !e.Available() {
		return fmt.Errorf("cliairplay binary %q not found", e.opts.BinaryPath)
	}

	e.mu.Lock()
	e.request = request
	e.state = StateBuffering
	e.mu.Unlock()

	pipePath := filepath.Join(os.TempDir(), fmt.Sprintf("cliairplay-%d-%d.pipe", os.Getpid(), time.Now().UnixNano()))
	pipe, err := openCmdPipe(pipePath)
	if err != nil {
		e.setError(err)
		return err
	}
	e.pipe = pipe

	streamCtx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel

	format := audio.Format{
		SampleRate: e.opts.SampleRate,
		Channels:   e.opts.Channels,
		BitDepth:   e.opts.BitDepth,
		// cliairplay consumes little-endian interleaved PCM on stdin.
		BigEndian: false,
	}
	decoder, err := audio.NewDecoder(streamCtx, request.StreamURL, audio.DecoderOptions{
		FFmpegPath:      e.opts.FFmpegPath,
		Format:          format,
		StartPositionMS: request.StartPositionMS,
		Headers:         e.opts.Headers,
	})
	if err != nil {
		_ = pipe.Close()
		cancel()
		e.setError(err)
		return err
	}
	source := audio.NewSwitchableReader(decoder)
	e.source = source

	args := e.buildArgs(pipePath)
	cmd := exec.CommandContext(streamCtx, e.opts.BinaryPath, args...)
	cmd.Stdin = source
	stdout := newTailBuffer(16 * 1024)
	stderr := newTailBuffer(32 * 1024)
	stdout.onLine = e.handleStatusLine
	stderr.onLine = e.handleStatusLine
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	e.stdout = stdout
	e.stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = source.Close()
		_ = pipe.Close()
		cancel()
		e.setError(fmt.Errorf("start cliairplay: %w", err))
		return err
	}
	e.cmd = cmd

	go e.attachPipe(pipe)
	go e.watchProcess(cmd, streamCtx)

	if err := e.command("VOLUME=" + strconv.Itoa(request.Volume) + "\n"); err != nil {
		e.opts.Logger.Debug("initial volume command failed", "error", err)
	}
	if err := e.command(fmt.Sprintf("START_UNIX_MS=%d\nACTION=START\n", time.Now().UnixMilli())); err != nil {
		e.opts.Logger.Debug("start command failed", "error", err)
	}
	e.sendMetadata(request)

	e.setState(StatePlaying)
	return nil
}

func (e *CLIAirplayEngine) buildArgs(pipePath string) []string {
	args := []string{
		"--protocol", e.opts.Protocol,
		"--port", strconv.Itoa(e.target.Port),
		"--volume", strconv.Itoa(e.request.Volume),
		"--samplerate", strconv.Itoa(e.opts.SampleRate),
		"--bitdepth", strconv.Itoa(e.opts.BitDepth),
		"--channels", strconv.Itoa(e.opts.Channels),
		"--latency", strconv.Itoa(e.opts.LatencyMS),
		"--cmdpipe", pipePath,
	}
	if e.opts.IfaceIP != "" {
		args = append(args, "--if", e.opts.IfaceIP)
	}
	if e.opts.PublishIP != "" {
		args = append(args, "--publish-ip", e.opts.PublishIP)
	}
	if e.target.Name != "" {
		args = append(args, "--name", e.target.Name, "--udn", e.target.Name)
	}
	if txt := e.txtArgs(); len(txt) > 0 {
		args = append(args, "--txt")
		args = append(args, txt...)
	}
	args = append(args, e.target.Address)
	return args
}

// txtArgs renders the receiver's mDNS TXT records as `--txt k=v` pairs so the
// kernel's route auto-selection sees the real device capabilities.
func (e *CLIAirplayEngine) txtArgs() []string {
	if len(e.target.TXT) == 0 {
		return nil
	}
	keys := make([]string, 0, len(e.target.TXT))
	for key := range e.target.TXT {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		value := e.target.TXT[key]
		if strings.TrimSpace(value) == "" {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
}

func (e *CLIAirplayEngine) attachPipe(pipe *cmdPipe) {
	if err := pipe.Attach(10 * time.Second); err != nil {
		e.opts.Logger.Warn("cmdpipe attach failed", "error", err)
	}
}

func (e *CLIAirplayEngine) watchProcess(cmd *exec.Cmd, ctx context.Context) {
	err := cmd.Wait()

	e.runMu.Lock()
	done := e.doneCh
	e.runMu.Unlock()

	switch {
	case ctx.Err() != nil:
		e.setState(StateStopped)
	case err != nil:
		e.setError(fmt.Errorf("cliairplay exited: %v: %s", err, e.diagnostics()))
	default:
		e.markFinished()
	}

	if done != nil {
		close(done)
	}
}

func (e *CLIAirplayEngine) diagnostics() string {
	var parts []string
	if tail := e.stderr.String(); tail != "" {
		parts = append(parts, tail)
	}
	if tail := e.stdout.String(); tail != "" {
		parts = append(parts, tail)
	}
	return strings.Join(parts, " | ")
}

// handleStatusLine maps the kernel's `[STATUS] ...` lines onto session state.
// The upstream contract emits lifecycle lines on stderr and the one-shot
// route/capabilities/latency lines on stdout, so both streams are parsed.
func (e *CLIAirplayEngine) handleStatusLine(line string) {
	if !strings.HasPrefix(line, "[STATUS]") {
		return
	}
	fields := strings.Fields(strings.TrimPrefix(line, "[STATUS]"))
	if len(fields) == 0 {
		return
	}
	e.opts.Logger.Debug("cliairplay status", "line", line)

	switch fields[0] {
	case "started", "playing":
		e.setState(StatePlaying)
	case "paused", "standby":
		e.setState(StatePaused)
	case "flushed":
		e.setState(StateBuffering)
	case "stopped", "eof":
		e.markFinished()
	case "error", "failed":
		e.setError(errors.New(line))
	}
}

func (e *CLIAirplayEngine) command(line string) error {
	e.startMu.Lock()
	pipe := e.pipe
	e.startMu.Unlock()
	if pipe == nil {
		return errors.New("cmdpipe not ready")
	}
	return pipe.Write(line)
}

func (e *CLIAirplayEngine) sendMetadata(request PlayRequest) {
	var b strings.Builder
	if request.Title != "" {
		fmt.Fprintf(&b, "TITLE=%s\n", request.Title)
	}
	if request.Artist != "" {
		fmt.Fprintf(&b, "ARTIST=%s\n", request.Artist)
	}
	if request.Album != "" {
		fmt.Fprintf(&b, "ALBUM=%s\n", request.Album)
	}
	if request.DurationMS > 0 {
		fmt.Fprintf(&b, "DURATION=%d\n", request.DurationMS/1000)
	}
	if request.TrackID > 0 {
		fmt.Fprintf(&b, "ITEMID=%d\n", request.TrackID)
	}
	if b.Len() == 0 {
		return
	}
	b.WriteString("ACTION=SENDMETA\n")
	if err := e.command(b.String()); err != nil {
		e.opts.Logger.Debug("metadata command failed", "error", err)
	}
}

// Control implements Engine.
func (e *CLIAirplayEngine) Control(action string, positionMS int64) error {
	if e.cmd == nil {
		return ErrNoSession
	}
	switch action {
	case ActionPause:
		if err := e.command("ACTION=PAUSE\n"); err != nil {
			return err
		}
		e.setState(StatePaused)
		return nil

	case ActionResume:
		if err := e.command("ACTION=PLAY\n"); err != nil {
			return err
		}
		e.setState(StatePlaying)
		return nil

	case ActionStop:
		return e.Close()

	case ActionSeek:
		if positionMS < 0 {
			positionMS = 0
		}
		e.runMu.Lock()
		source := e.source
		e.runMu.Unlock()
		if source == nil {
			return ErrNoSession
		}
		decoder, err := audio.NewDecoder(context.Background(), e.currentStreamURL(), audio.DecoderOptions{
			FFmpegPath:      e.opts.FFmpegPath,
			Format:          audio.Format{SampleRate: e.opts.SampleRate, Channels: e.opts.Channels, BitDepth: e.opts.BitDepth},
			StartPositionMS: positionMS,
			Headers:         e.opts.Headers,
		})
		if err != nil {
			return fmt.Errorf("seek: %w", err)
		}
		source.Swap(decoder)
		if err := e.command("ACTION=FLUSH\n"); err != nil {
			return err
		}
		if err := e.command(fmt.Sprintf("START_UNIX_MS=%d\nACTION=START\n", time.Now().UnixMilli())); err != nil {
			return err
		}
		if e.request.DurationMS > 0 {
			_ = e.command(fmt.Sprintf("PROGRESS=%d\n", positionMS/1000))
		}
		e.resetPosition(positionMS)
		return nil

	default:
		return fmt.Errorf("%w: %s", ErrNotSupported, action)
	}
}

// SetVolume implements Engine.
func (e *CLIAirplayEngine) SetVolume(percent int) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	e.mu.Lock()
	e.request.Volume = percent
	e.mu.Unlock()
	if e.cmd == nil {
		return nil
	}
	return e.command("VOLUME=" + strconv.Itoa(percent) + "\n")
}

// Status implements Engine.
func (e *CLIAirplayEngine) Status() Status {
	return e.snapshot()
}

// Close implements Engine.
func (e *CLIAirplayEngine) Close() error {
	e.runMu.Lock()
	cmd := e.cmd
	pipe := e.pipe
	source := e.source
	e.runMu.Unlock()

	// STOP is terminal for cliairplay: the process tears the session down and
	// exits 0. Fall back to the context cancel if the pipe is not attached.
	_ = e.command("ACTION=STOP\n")
	e.setState(StateStopped)

	if e.cancel != nil {
		e.cancel()
	}
	if pipe != nil {
		_ = pipe.Close()
	}
	if source != nil {
		_ = source.Close()
	}
	if cmd != nil && cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			_, _ = cmd.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
	return nil
}

func (e *CLIAirplayEngine) currentStreamURL() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.request.StreamURL
}

// tailBuffer keeps the last N bytes of a stream for diagnostics and, when
// onLine is set, reports every complete line to a parser. cliairplay splits its
// lifecycle lines across stderr and the one-shot setup lines across stdout, so
// both streams are watched.
type tailBuffer struct {
	mu     sync.Mutex
	data   []byte
	max    int
	onLine func(string)
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		b.data = b.data[len(b.data)-b.max:]
	}
	onLine := b.onLine
	lines := extractLines(&b.data)
	b.mu.Unlock()

	if onLine != nil {
		for _, line := range lines {
			onLine(line)
		}
	}
	return len(p), nil
}

// extractLines splits complete newline-terminated lines off the buffer.
func extractLines(data *[]byte) []string {
	var lines []string
	for {
		index := indexByte(*data, '\n')
		if index < 0 {
			return lines
		}
		lines = append(lines, strings.TrimSpace(string((*data)[:index])))
		*data = (*data)[index+1:]
	}
}

func indexByte(data []byte, target byte) int {
	for i, b := range data {
		if b == target {
			return i
		}
	}
	return -1
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

var _ Engine = (*CLIAirplayEngine)(nil)
