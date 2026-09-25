// Package audio turns a LyraNest stream URL into the raw PCM stream a push
// kernel expects.
//
// LyraNest serves tracks as MP3/FLAC/AAC/Opus containers, so the bridge always
// pipes the URL through ffmpeg. ffmpeg is already a hard dependency of the
// container image and its `-reconnect` handling gives us resilience against a
// slow or briefly interrupted server connection.
package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Format describes the PCM produced by a Decoder.
type Format struct {
	SampleRate int
	Channels   int
	BitDepth   int
	// BigEndian selects s16be output (RAOP L16) instead of s16le
	// (what cliairplay consumes).
	BigEndian bool
}

// SampleFormat is the ffmpeg raw muxer name for the configured layout. Only
// 16- and 24-bit output is supported: RAOP L16 is 16-bit and cliairplay's
// high-resolution path is 24-bit-in-32-bit.
func (f Format) SampleFormat() string {
	switch {
	case f.BitDepth == 24 && f.BigEndian:
		return "s32be"
	case f.BitDepth == 24:
		return "s32le"
	case f.BigEndian:
		return "s16be"
	default:
		return "s16le"
	}
}

// FrameSize is the byte size of one PCM frame across all channels.
func (f Format) FrameSize() int {
	depth := f.BitDepth / 8
	if f.BitDepth == 24 {
		depth = 4
	}
	return f.Channels * depth
}

// Decoder is a running ffmpeg process producing raw PCM on stdout.
type Decoder struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  *ringBuffer
	url     string
	closeMu sync.Mutex
	closed  bool
	waitCh  chan struct{}
	waitErr error
}

// DecoderOptions configures a Decoder.
type DecoderOptions struct {
	// FFmpegPath is the ffmpeg binary, resolved through PATH when bare.
	FFmpegPath string
	// Format is the desired raw PCM layout.
	Format Format
	// StartPositionMS seeks into the source before decoding.
	StartPositionMS int64
	// Headers are extra HTTP headers sent by ffmpeg, e.g. Authorization.
	Headers map[string]string
	// Logger receives the ffmpeg stderr tail on failure.
	Logger func(format string, args ...any)
}

// NewDecoder starts ffmpeg and returns a reader for its raw PCM output.
func NewDecoder(ctx context.Context, streamURL string, opts DecoderOptions) (*Decoder, error) {
	if strings.TrimSpace(streamURL) == "" {
		return nil, errors.New("empty stream url")
	}
	ffmpegPath := strings.TrimSpace(opts.FFmpegPath)
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}

	format := opts.Format
	if format.SampleRate <= 0 {
		format.SampleRate = 44100
	}
	if format.Channels <= 0 {
		format.Channels = 2
	}
	if format.BitDepth == 0 {
		format.BitDepth = 16
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-nostdin",
		"-reconnect", "1",
		"-reconnect_streamed", "1",
		"-reconnect_delay_max", "5",
	}
	if len(opts.Headers) > 0 {
		var headerBlock strings.Builder
		for key, value := range opts.Headers {
			headerBlock.WriteString(key)
			headerBlock.WriteString(": ")
			headerBlock.WriteString(value)
			headerBlock.WriteString("\r\n")
		}
		args = append(args, "-headers", headerBlock.String())
	}
	if opts.StartPositionMS > 0 {
		args = append(args, "-ss", strconv.FormatFloat(float64(opts.StartPositionMS)/1000.0, 'f', 3, 64))
	}
	args = append(args,
		"-i", streamURL,
		"-vn",
		"-map", "a:0?",
		"-acodec", pcmCodec(format),
		"-f", format.SampleFormat(),
		"-ar", strconv.Itoa(format.SampleRate),
		"-ac", strconv.Itoa(format.Channels),
		"-",
	)

	cmd := exec.Command(ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr := newRingBuffer(8 * 1024)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	decoder := &Decoder{
		cmd:    cmd,
		stdout: stdout,
		stderr: stderr,
		url:    streamURL,
		waitCh: make(chan struct{}),
	}

	go func() {
		decoder.waitErr = cmd.Wait()
		close(decoder.waitCh)
		// A non-zero exit almost always means the stream URL or the transcode
		// arguments were wrong. Surfacing the ffmpeg tail here is what turns a
		// silent "no audio" into an actionable log line.
		if decoder.waitErr != nil && opts.Logger != nil {
			opts.Logger("ffmpeg exited: %v: %s", decoder.waitErr, stderr.String())
		}
	}()

	go func() {
		<-ctx.Done()
		_ = decoder.Close()
	}()

	return decoder, nil
}

// Read returns raw PCM bytes.
func (d *Decoder) Read(p []byte) (int, error) {
	if d.stdout == nil {
		return 0, io.EOF
	}
	n, err := d.stdout.Read(p)
	return n, normalizeReadError(err)
}

// normalizeReadError collapses the "decoder finished" conditions onto io.EOF.
//
// os/exec closes the StdoutPipe as soon as the child exits, so the reader races
// Wait: the last read can fail with "read |0: file already closed" instead of
// io.EOF. Both mean exactly the same thing - the decoder produced everything it
// is going to produce - and reporting the former as a failure would surface a
// spurious playback error at the end of every track.
func normalizeReadError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		return io.EOF
	default:
		return err
	}
}

// Stderr returns the captured ffmpeg diagnostics.
func (d *Decoder) Stderr() string {
	if d.stderr == nil {
		return ""
	}
	return d.stderr.String()
}

// Close stops ffmpeg and waits for it to exit.
func (d *Decoder) Close() error {
	d.closeMu.Lock()
	if d.closed {
		d.closeMu.Unlock()
		<-d.waitCh
		return nil
	}
	d.closed = true
	d.closeMu.Unlock()

	if d.stdout != nil {
		_ = d.stdout.Close()
	}
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
	<-d.waitCh
	return nil
}

// URL returns the source URL being decoded.
func (d *Decoder) URL() string { return d.url }

// pcmCodec returns the ffmpeg codec that matches the raw muxer. The raw PCM
// muxers reject a mismatched byte order ("s16be muxer supports only codec
// pcm_s16be"), so the two must always be chosen together.
func pcmCodec(format Format) string {
	switch format.SampleFormat() {
	case "s16be":
		return "pcm_s16be"
	case "s32be":
		return "pcm_s32be"
	case "s32le":
		return "pcm_s32le"
	default:
		return "pcm_s16le"
	}
}

// ringBuffer keeps the tail of a stream in memory without unbounded growth.
type ringBuffer struct {
	mu   sync.Mutex
	data []byte
	max  int
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{max: max}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
	if len(r.data) > r.max {
		r.data = r.data[len(r.data)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(bytes.TrimSpace(r.data))
}

// SwitchableReader lets a streaming loop change its PCM source in place.
// Swap records the replacement; the change takes effect on the next Read call,
// which the caller performs at a packet boundary.
type SwitchableReader struct {
	mu      sync.Mutex
	current io.ReadCloser
	pending io.ReadCloser
	closed  bool
	// generation increments on every swap so callers can detect a seek.
	generation uint64
}

// NewSwitchableReader wraps an initial source.
func NewSwitchableReader(initial io.ReadCloser) *SwitchableReader {
	return &SwitchableReader{current: initial}
}

// Read applies any pending swap, then reads from the active source.
func (r *SwitchableReader) Read(p []byte) (int, error) {
	r.applyPending()

	r.mu.Lock()
	current := r.current
	r.mu.Unlock()
	if current == nil {
		return 0, io.EOF
	}
	return current.Read(p)
}

func (r *SwitchableReader) applyPending() {
	r.mu.Lock()
	if r.pending == nil {
		r.mu.Unlock()
		return
	}
	previous := r.current
	r.current = r.pending
	r.pending = nil
	r.generation++
	r.mu.Unlock()

	if previous != nil {
		_ = previous.Close()
	}
}

// Swap installs a new source. The previous source is closed once the swap is
// applied.
func (r *SwitchableReader) Swap(next io.ReadCloser) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = next.Close()
		return
	}
	if r.pending != nil {
		_ = r.pending.Close()
	}
	r.pending = next
	r.mu.Unlock()
}

// Generation returns the number of swaps applied so far.
func (r *SwitchableReader) Generation() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generation
}

// Close releases both the active and any pending source.
func (r *SwitchableReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	current := r.current
	pending := r.pending
	r.current = nil
	r.pending = nil
	r.mu.Unlock()

	if pending != nil {
		_ = pending.Close()
	}
	if current != nil {
		return current.Close()
	}
	return nil
}
