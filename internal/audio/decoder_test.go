package audio

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"
)

func TestFormatSampleFormat(t *testing.T) {
	cases := []struct {
		format Format
		want   string
	}{
		{Format{SampleRate: 44100, Channels: 2, BitDepth: 16}, "s16le"},
		{Format{SampleRate: 44100, Channels: 2, BitDepth: 16, BigEndian: true}, "s16be"},
		{Format{SampleRate: 44100, Channels: 2, BitDepth: 24}, "s32le"},
		{Format{SampleRate: 44100, Channels: 2, BitDepth: 24, BigEndian: true}, "s32be"},
	}
	for _, testCase := range cases {
		if got := testCase.format.SampleFormat(); got != testCase.want {
			t.Errorf("SampleFormat(%+v) = %q, want %q", testCase.format, got, testCase.want)
		}
	}
}

// TestFormatFrameSizeMatchesRAOP is the invariant that keeps RTP packets at the
// 352-frame boundary RAOP receivers expect.
func TestFormatFrameSizeMatchesRAOP(t *testing.T) {
	stereo16 := Format{SampleRate: 44100, Channels: 2, BitDepth: 16}
	if stereo16.FrameSize() != 4 {
		t.Fatalf("stereo 16-bit frame size = %d, want 4", stereo16.FrameSize())
	}
	if packet := 352 * stereo16.FrameSize(); packet != 1408 {
		t.Fatalf("RAOP packet size = %d, want 1408", packet)
	}
	mono16 := Format{SampleRate: 44100, Channels: 1, BitDepth: 16}
	if mono16.FrameSize() != 2 {
		t.Fatalf("mono 16-bit frame size = %d, want 2", mono16.FrameSize())
	}
	// 24-bit audio is carried in 32-bit containers by ffmpeg's s32 muxers.
	stereo24 := Format{SampleRate: 44100, Channels: 2, BitDepth: 24}
	if stereo24.FrameSize() != 8 {
		t.Fatalf("stereo 24-bit frame size = %d, want 8", stereo24.FrameSize())
	}
}

func TestPCMCodecMatchesTheRawMuxer(t *testing.T) {
	// ffmpeg rejects a mismatch ("s16be muxer supports only codec pcm_s16be"),
	// so the muxer and the codec must always be derived together.
	cases := []struct {
		format Format
		want   string
	}{
		{Format{BitDepth: 16}, "pcm_s16le"},
		{Format{BitDepth: 16, BigEndian: true}, "pcm_s16be"},
		{Format{BitDepth: 24}, "pcm_s32le"},
		{Format{BitDepth: 24, BigEndian: true}, "pcm_s32be"},
	}
	for _, testCase := range cases {
		if got := pcmCodec(testCase.format); got != testCase.want {
			t.Errorf("pcmCodec(%+v) = %q, want %q", testCase.format, got, testCase.want)
		}
	}
}

type fakeSource struct {
	reader *bytes.Reader
	closed bool
}

func newFakeSource(data string) *fakeSource {
	return &fakeSource{reader: bytes.NewReader([]byte(data))}
}

func (f *fakeSource) Read(p []byte) (int, error) { return f.reader.Read(p) }

func (f *fakeSource) Close() error {
	f.closed = true
	return nil
}

// TestSwitchableReaderSwapsAtTheNextRead is what makes seek work without
// renegotiating the RTSP session: the running stream loop picks up the new
// source at its next packet boundary.
func TestSwitchableReaderSwapsAtTheNextRead(t *testing.T) {
	first := newFakeSource("AAAA")
	second := newFakeSource("BBBB")

	reader := NewSwitchableReader(first)
	buffer := make([]byte, 2)

	if n, err := reader.Read(buffer); err != nil || string(buffer[:n]) != "AA" {
		t.Fatalf("first read = %q, %v", buffer[:n], err)
	}

	reader.Swap(second)

	// The swap is applied on the next Read call, so the rest of the first
	// source is discarded rather than spliced into the new track.
	if n, err := reader.Read(buffer); err != nil || string(buffer[:n]) != "BB" {
		t.Fatalf("read after swap = %q, %v", buffer[:n], err)
	}
	if !first.closed {
		t.Errorf("the replaced source must be closed")
	}
	if reader.Generation() != 1 {
		t.Errorf("generation = %d, want 1", reader.Generation())
	}

	if n, err := reader.Read(buffer); err != nil || string(buffer[:n]) != "BB" {
		t.Fatalf("second read after swap = %q, %v", buffer[:n], err)
	}

	if _, err := reader.Read(buffer); err != io.EOF {
		t.Fatalf("expected EOF at the end of the source, got %v", err)
	}
}

func TestSwitchableReaderCloseReleasesBothSources(t *testing.T) {
	first := newFakeSource("AAAA")
	second := newFakeSource("BBBB")

	reader := NewSwitchableReader(first)
	reader.Swap(second)

	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !first.closed || !second.closed {
		t.Fatalf("both the active and the pending source must be closed (active=%v pending=%v)", first.closed, second.closed)
	}
	// Reads after Close report EOF instead of panicking.
	if _, err := reader.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("read after close = %v, want EOF", err)
	}
	// A late swap is dropped and its source closed.
	late := newFakeSource("CCCC")
	reader.Swap(late)
	if !late.closed {
		t.Errorf("a swap after close must close the incoming source")
	}
}

func TestSwitchableReaderWithoutSourceIsEOF(t *testing.T) {
	reader := NewSwitchableReader(nil)
	if _, err := reader.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("read = %v, want EOF", err)
	}
}

func TestRingBufferKeepsTheTail(t *testing.T) {
	buffer := newRingBuffer(8)
	if _, err := buffer.Write([]byte("1234567890")); err != nil {
		t.Fatal(err)
	}
	if got := buffer.String(); got != "34567890" {
		t.Fatalf("ring buffer = %q, want the last 8 bytes", got)
	}
	if _, err := buffer.Write([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	if got := buffer.String(); got != "567890ab" {
		t.Fatalf("ring buffer = %q", got)
	}
}

// TestNormalizeReadError pins the end-of-stream behaviour.
//
// os/exec closes the child's stdout pipe when the process exits, so the reader
// races Wait and the final read may report "file already closed" instead of
// io.EOF. Treating that as a failure would make every finished track look like
// a playback error.
func TestNormalizeReadError(t *testing.T) {
	closedErr := &fs.PathError{Op: "read", Path: "|0", Err: os.ErrClosed}
	if got := normalizeReadError(closedErr); got != io.EOF {
		t.Errorf("closed pipe mapped to %v, want EOF", got)
	}
	if got := normalizeReadError(os.ErrClosed); got != io.EOF {
		t.Errorf("os.ErrClosed mapped to %v, want EOF", got)
	}
	if got := normalizeReadError(io.ErrClosedPipe); got != io.EOF {
		t.Errorf("io.ErrClosedPipe mapped to %v, want EOF", got)
	}
	if got := normalizeReadError(io.EOF); got != io.EOF {
		t.Errorf("EOF mapped to %v", got)
	}
	if got := normalizeReadError(nil); got != nil {
		t.Errorf("nil mapped to %v", got)
	}
	// A genuine failure must still be reported.
	boom := errors.New("decoder exploded")
	if got := normalizeReadError(boom); got != boom {
		t.Errorf("a real error was swallowed: %v", got)
	}
}
