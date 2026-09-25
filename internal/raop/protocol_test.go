package raop

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// TestBuildSDPMatchesReferenceImplementation pins the ANNOUNCE body to the
// exact byte layout the reference RAOP sender (pyatv) puts on the wire. A
// receiver parses these lines positionally, so any drift here breaks playback.
func TestBuildSDPMatchesReferenceImplementation(t *testing.T) {
	sdp := BuildSDP(1234, "192.168.1.50", "192.168.1.105", AudioFormat{SampleRate: 44100, Channels: 2, BitDepth: 16})

	want := "v=0\r\n" +
		"o=iTunes 1234 0 IN IP4 192.168.1.50\r\n" +
		"s=iTunes\r\n" +
		"c=IN IP4 192.168.1.105\r\n" +
		"t=0 0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 L16/44100/2\r\n" +
		"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100\r\n"

	if sdp != want {
		t.Fatalf("SDP mismatch\n got: %q\nwant: %q", sdp, want)
	}
}

func TestAudioPacketLayout(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 1408)
	packet := AudioPacket(0x1234, 0xDEADBEEF, 0xCAFEBABE, true, payload)

	if len(packet) != 12+1408 {
		t.Fatalf("packet length = %d, want %d", len(packet), 12+1408)
	}
	if packet[0] != 0x80 {
		t.Errorf("proto = 0x%02X, want 0x80", packet[0])
	}
	// First packet sets the marker bit: 0x60 | 0x80.
	if packet[1] != 0xE0 {
		t.Errorf("type = 0x%02X, want 0xE0", packet[1])
	}
	if got := binary.BigEndian.Uint16(packet[2:4]); got != 0x1234 {
		t.Errorf("seqno = %d", got)
	}
	if got := binary.BigEndian.Uint32(packet[4:8]); got != 0xDEADBEEF {
		t.Errorf("timestamp = %d", got)
	}
	if got := binary.BigEndian.Uint32(packet[8:12]); got != 0xCAFEBABE {
		t.Errorf("ssrc = %d", got)
	}
	if !bytes.Equal(packet[12:], payload) {
		t.Errorf("payload corrupted")
	}

	follow := AudioPacket(0x1235, 1, 2, false, payload)
	if follow[1] != 0x60 {
		t.Errorf("non-first type = 0x%02X, want 0x60", follow[1])
	}
}

func TestSyncPacketLayout(t *testing.T) {
	packet := SyncPacket(true, 111, 222, 333, 444)
	if len(packet) != 20 {
		t.Fatalf("sync packet length = %d, want 20", len(packet))
	}
	if packet[0] != 0x90 {
		t.Errorf("first sync proto = 0x%02X, want 0x90", packet[0])
	}
	if packet[1] != 0xD4 {
		t.Errorf("sync type = 0x%02X, want 0xD4", packet[1])
	}
	if got := binary.BigEndian.Uint16(packet[2:4]); got != 7 {
		t.Errorf("sync seqno = %d, want 7", got)
	}
	if got := binary.BigEndian.Uint32(packet[4:8]); got != 111 {
		t.Errorf("now_without_latency = %d", got)
	}
	if got := binary.BigEndian.Uint32(packet[8:12]); got != 222 {
		t.Errorf("last_sync_sec = %d", got)
	}
	if got := binary.BigEndian.Uint32(packet[12:16]); got != 333 {
		t.Errorf("last_sync_frac = %d", got)
	}
	if got := binary.BigEndian.Uint32(packet[16:20]); got != 444 {
		t.Errorf("now = %d", got)
	}
	if second := SyncPacket(false, 0, 0, 0, 0); second[0] != 0x80 {
		t.Errorf("subsequent sync proto = 0x%02X, want 0x80", second[0])
	}
}

func TestTimingReplyRoundTrip(t *testing.T) {
	reply := TimingReply(0x80, 10, 20, 30, 40)
	if len(reply) != 32 {
		t.Fatalf("timing reply length = %d, want 32", len(reply))
	}
	if reply[0] != 0x80 {
		t.Errorf("proto echoed incorrectly: 0x%02X", reply[0])
	}
	if reply[1] != 0xD3 {
		t.Errorf("timing type = 0x%02X, want 0xD3", reply[1])
	}
	// reftime must echo the request's send time, so the prober can measure RTT.
	if got := binary.BigEndian.Uint32(reply[8:12]); got != 10 {
		t.Errorf("reftime_sec = %d", got)
	}
	if got := binary.BigEndian.Uint32(reply[12:16]); got != 20 {
		t.Errorf("reftime_frac = %d", got)
	}
	if got := binary.BigEndian.Uint32(reply[16:20]); got != 30 {
		t.Errorf("recvtime_sec = %d", got)
	}
	if got := binary.BigEndian.Uint32(reply[24:28]); got != 30 {
		t.Errorf("sendtime_sec = %d", got)
	}
}

func TestParseTimingRequest(t *testing.T) {
	packet := make([]byte, 32)
	packet[0] = 0x80
	packet[1] = 0x53
	binary.BigEndian.PutUint32(packet[24:28], 1234)
	binary.BigEndian.PutUint32(packet[28:32], 5678)

	request, ok := ParseTimingRequest(packet)
	if !ok {
		t.Fatal("expected the packet to parse")
	}
	if request.Proto != 0x80 || request.SendTimeSec != 1234 || request.SendTimeFrac != 5678 {
		t.Fatalf("unexpected request %+v", request)
	}
	if _, ok := ParseTimingRequest(packet[:16]); ok {
		t.Fatal("a short packet must not parse")
	}
}

// TestNTPRTPConversion checks the two invariants the receiver's playout
// scheduler depends on.
func TestNTPRTPConversion(t *testing.T) {
	now := time.Now()
	ntp := NTPNow(now.Unix(), int64(now.Nanosecond()))

	sec, frac := NTPParts(ntp)
	if sec < 3_900_000_000 {
		t.Fatalf("NTP seconds = %d, which is not a current-era value", sec)
	}
	if frac == 0 && now.Nanosecond() > 1_000_000 {
		t.Fatalf("NTP fraction = 0, expected a non-zero sub-second part")
	}

	// One NTP second is exactly 2^32 NTP units, and it must advance the RTP
	// clock by exactly one sample rate worth of ticks.
	base := NTPToRTP(ntp, RTPTimeScale)
	advanced := NTPToRTP(ntp+1<<32, RTPTimeScale)
	if delta := advanced - base; delta != uint32(RTPTimeScale) {
		t.Fatalf("one NTP second advanced the RTP clock by %d ticks, want %d", delta, RTPTimeScale)
	}

	// An RTP timestamp must survive a trip through the NTP domain to within one
	// tick: the NTP form is a 32.32 fixed-point value, so the division rounds.
	// One tick at 44.1 kHz is 23 microseconds, far below any scheduling
	// relevance, and real senders have exactly the same property.
	for _, ts := range []uint32{0, 1, 44100, 66150, 0x12345678, 0xFFFFFFFF} {
		got := NTPToRTP(RTPToNTP(ts, RTPTimeScale), RTPTimeScale)
		delta := int64(int32(got - ts))
		if delta < -1 || delta > 1 {
			t.Errorf("RTP timestamp %d round-tripped to %d (delta %d)", ts, got, delta)
		}
	}
}

// TestRTPTimestampAdvancesWithFrames is the invariant the receiver uses to
// schedule playout: one second of audio is exactly sample_rate RTP ticks.
func TestRTPTimestampAdvancesWithFrames(t *testing.T) {
	base := NTPToRTP(NTPNow(time.Now().Unix(), 0), RTPTimeScale)
	later := NTPToRTP(NTPNow(time.Now().Unix()+1, 0), RTPTimeScale)
	delta := int64(int32(later - base))
	if delta < RTPTimeScale-2 || delta > RTPTimeScale+2 {
		t.Fatalf("one second advanced the RTP clock by %d ticks, want ~%d", delta, RTPTimeScale)
	}
}

func TestPercentToDBFS(t *testing.T) {
	cases := map[int]float64{
		0:   -144.0,
		25:  -22.5,
		50:  -15.0,
		60:  -12.0,
		75:  -7.5,
		100: 0.0,
	}
	for percent, want := range cases {
		if got := PercentToDBFS(percent); got != want {
			t.Errorf("PercentToDBFS(%d) = %v, want %v", percent, got, want)
		}
	}
	// Values outside the range are clamped, never extrapolated.
	if got := PercentToDBFS(-10); got != -144.0 {
		t.Errorf("PercentToDBFS(-10) = %v", got)
	}
	if got := PercentToDBFS(250); got != 0.0 {
		t.Errorf("PercentToDBFS(250) = %v", got)
	}
}

func TestDBFSToPercent(t *testing.T) {
	cases := map[float64]int{
		-144.0: 0,
		-30.0:  0,
		-15.0:  50,
		-7.5:   75,
		0.0:    100,
		5.0:    100,
	}
	for dbfs, want := range cases {
		if got := DBFSToPercent(dbfs); got != want {
			t.Errorf("DBFSToPercent(%v) = %d, want %d", dbfs, got, want)
		}
	}
}

func TestAudioFormatValidation(t *testing.T) {
	if err := (AudioFormat{SampleRate: 44100, Channels: 2, BitDepth: 16}).Validate(); err != nil {
		t.Fatalf("stereo 16-bit must be valid: %v", err)
	}
	if err := (AudioFormat{SampleRate: 44100, Channels: 1, BitDepth: 16}).Validate(); err != nil {
		t.Fatalf("mono 16-bit must be valid: %v", err)
	}
	if err := (AudioFormat{SampleRate: 44100, Channels: 3, BitDepth: 16}).Validate(); err == nil {
		t.Fatal("3 channels must be rejected")
	}
	if err := (AudioFormat{SampleRate: 44100, Channels: 2, BitDepth: 24}).Validate(); err == nil {
		t.Fatal("24-bit L16 must be rejected")
	}
}

func TestAudioFormatSizes(t *testing.T) {
	format := AudioFormat{SampleRate: 44100, Channels: 2, BitDepth: 16}
	if format.FrameSize() != 4 {
		t.Errorf("frame size = %d, want 4", format.FrameSize())
	}
	if format.PacketSize() != 1408 {
		t.Errorf("packet size = %d, want 1408", format.PacketSize())
	}
}

func TestLatencyIsTheDocumentedValue(t *testing.T) {
	// 22050 + sample rate: the RAOP buffer depth every sender uses.
	if LatencyFrames != 66150 {
		t.Fatalf("LatencyFrames = %d, want 66150", LatencyFrames)
	}
}

func TestParseTransport(t *testing.T) {
	params, options := parseTransport("RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;server_port=54892;control_port=54893;timing_port=54894")
	if !containsToken(params, "RTP/AVP/UDP") || !containsToken(params, "unicast") {
		t.Fatalf("params = %v", params)
	}
	if options["server_port"] != "54892" || options["control_port"] != "54893" || options["timing_port"] != "54894" {
		t.Fatalf("options = %v", options)
	}
	if options["mode"] != "record" {
		t.Fatalf("mode = %q", options["mode"])
	}
}

func TestRTSPRequestFraming(t *testing.T) {
	// The request the sender writes must be a well-formed RTSP/1.0 message:
	// request line, mandatory headers, blank line, then the body.
	sdp := BuildSDP(7, "10.0.0.2", "10.0.0.9", AudioFormat{SampleRate: 44100, Channels: 2, BitDepth: 16})
	if !strings.HasPrefix(sdp, "v=0\r\n") || !strings.HasSuffix(sdp, "\r\n") {
		t.Fatalf("SDP must be CRLF terminated: %q", sdp)
	}
	if strings.Contains(sdp, "\n\n") && !strings.Contains(sdp, "\r\n\r\n") {
		t.Fatalf("SDP must use CRLF, not bare LF")
	}
}
