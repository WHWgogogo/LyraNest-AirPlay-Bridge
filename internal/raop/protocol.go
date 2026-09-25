// Package raop implements a self-contained AirPlay 1 (RAOP) audio sender.
//
// The wire format mirrors the reference implementation shipped with pyatv
// (pyatv/protocols/raop + pyatv/support/rtsp.py) byte for byte, so a receiver
// that works with pyatv works with this sender:
//
//   - RTSP/1.0 control channel with CSeq / DACP-ID / Active-Remote /
//     Client-Instance headers and User-Agent "AirPlay/550.10".
//   - ANNOUNCE carries an SDP body advertising L16 uncompressed PCM at
//     44100 Hz / 16 bit / 2 channels in 352-frame RTP packets.
//   - Audio is sent as RTP (payload type 96) over UDP to the server_port
//     returned by SETUP.
//   - A 20-byte sync packet is sent to the control_port once per second.
//   - Timing requests from the receiver are answered on the local timing_port
//     with a 32-byte NTP-style reply.
//
// Only uncompressed L16 is produced: it needs no ALAC encoder and every
// receiver that advertises RAOP codec 0 accepts it. Devices that only
// advertise ALAC are served by the external cliairplay kernel instead.
package raop

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Protocol constants.
const (
	// FramesPerPacket is the fixed RAOP audio packet payload size in frames.
	FramesPerPacket = 352
	// PayloadType is the RTP dynamic payload type used by RAOP.
	PayloadType = 96
	// DefaultPort is the RTSP control port of an AirPlay 2 device.
	DefaultPort = 7000
	// UserAgent identifies the sender to the receiver.
	UserAgent = "AirPlay/550.10"
	// RTPTimeScale is the RTP timestamp clock, equal to the sample rate.
	RTPTimeScale = 44100
	// NTPEpochOffset converts a Unix timestamp to an NTP timestamp.
	NTPEpochOffset = 0x83AA7E80

	// LatencyFrames is the receiver buffer depth in frames: 22050 + rate.
	// This is the value pyatv (and RAOP-Player before it) uses.
	LatencyFrames = 22050 + RTPTimeScale

	// SyncInterval is how often a sync packet is emitted.
	SyncInterval = 1
)

// RTSP methods and RTP payload types used on the wire.
const (
	rtpTypeAudio     = 0x60
	rtpTypeSync      = 0xD4
	rtpTypeTiming    = 0x53
	rtpMarkerBit     = 0x80
	rtpProtoVersion  = 0x80
	rtpProtoExtended = 0x90
	syncSeqNo        = 7
	timingSeqNo      = 7
)

// AudioFormat describes the PCM stream fed into the sender.
type AudioFormat struct {
	SampleRate int
	Channels   int
	BitDepth   int
}

// Validate reports whether the format is one RAOP L16 can carry.
func (f AudioFormat) Validate() error {
	if f.SampleRate <= 0 {
		return fmt.Errorf("invalid sample rate %d", f.SampleRate)
	}
	if f.Channels != 1 && f.Channels != 2 {
		return fmt.Errorf("unsupported channel count %d (want 1 or 2)", f.Channels)
	}
	if f.BitDepth != 16 {
		return fmt.Errorf("unsupported bit depth %d (L16 requires 16)", f.BitDepth)
	}
	return nil
}

// FrameSize is the byte size of one PCM frame across all channels.
func (f AudioFormat) FrameSize() int { return f.Channels * (f.BitDepth / 8) }

// PacketSize is the byte size of one full RTP audio payload.
func (f AudioFormat) PacketSize() int { return FramesPerPacket * f.FrameSize() }

// rtpmap returns the SDP rtpmap value, e.g. "L16/44100/2".
func (f AudioFormat) rtpmap() string {
	return fmt.Sprintf("L16/%d/%d", f.SampleRate, f.Channels)
}

// fmtp returns the SDP fmtp parameter list for uncompressed audio. The layout
// (352 0 <bits> 40 10 14 <channels> 255 0 0 <rate>) is what RAOP senders have
// always advertised and what receivers parse positionally.
func (f AudioFormat) fmtp() string {
	return fmt.Sprintf("%d 0 %d 40 10 14 %d 255 0 0 %d",
		FramesPerPacket, f.BitDepth, f.Channels, f.SampleRate)
}

// BuildSDP renders the ANNOUNCE session description.
func BuildSDP(sessionID uint32, localIP, remoteIP string, format AudioFormat) string {
	var b strings.Builder
	b.WriteString("v=0\r\n")
	fmt.Fprintf(&b, "o=iTunes %d 0 IN IP4 %s\r\n", sessionID, localIP)
	b.WriteString("s=iTunes\r\n")
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", remoteIP)
	b.WriteString("t=0 0\r\n")
	fmt.Fprintf(&b, "m=audio 0 RTP/AVP %d\r\n", PayloadType)
	fmt.Fprintf(&b, "a=rtpmap:%d %s\r\n", PayloadType, format.rtpmap())
	fmt.Fprintf(&b, "a=fmtp:%d %s\r\n", PayloadType, format.fmtp())
	return b.String()
}

// ---------------------------------------------------------------------------
// NTP <-> RTP timestamp conversion
// ---------------------------------------------------------------------------

// NTPNow returns the current time as a 64-bit NTP timestamp (32.32 fixed
// point). uint64 is required: the value is about 1.7e19 and does not fit in
// an int64.
func NTPNow(unixSeconds int64, unixNanos int64) uint64 {
	seconds := uint64(unixSeconds + NTPEpochOffset)
	frac := uint64((unixNanos << 32) / 1e9)
	return seconds<<32 | frac
}

// NTPParts splits an NTP timestamp into its 32-bit seconds and fraction.
func NTPParts(ntp uint64) (uint32, uint32) {
	return uint32(ntp >> 32), uint32(ntp & 0xFFFFFFFF)
}

// NTPToRTP converts an NTP timestamp into an RTP timestamp at the given rate.
func NTPToRTP(ntp uint64, rate int) uint32 {
	return uint32(((ntp >> 16) * uint64(rate)) >> 16)
}

// RTPToNTP converts an RTP timestamp back into an NTP timestamp.
func RTPToNTP(ts uint32, rate int) uint64 {
	return uint64((uint64(ts)<<16)/uint64(rate)) << 16
}

// ---------------------------------------------------------------------------
// RTP packet encoding
// ---------------------------------------------------------------------------

// AudioPacket builds a complete RTP audio packet: a 12-byte header followed by
// `payload`. `first` sets the RTP marker bit on the very first packet of a
// session, exactly as pyatv does.
func AudioPacket(seqno uint16, timestamp, ssrc uint32, first bool, payload []byte) []byte {
	packet := make([]byte, 12+len(payload))
	packet[0] = rtpProtoVersion
	packet[1] = rtpTypeAudio
	if first {
		packet[1] |= rtpMarkerBit
	}
	binary.BigEndian.PutUint16(packet[2:4], seqno)
	binary.BigEndian.PutUint32(packet[4:8], timestamp)
	binary.BigEndian.PutUint32(packet[8:12], ssrc)
	copy(packet[12:], payload)
	return packet
}

// SyncPacket builds the 20-byte RTP control packet that tells the receiver
// which sample position is audible at a given wall-clock instant.
//
// Layout: proto(1) type(1) seqno(2) now_without_latency(4) last_sync_sec(4)
// last_sync_frac(4) now(4).
func SyncPacket(first bool, nowWithoutLatency uint32, lastSyncSec, lastSyncFrac, now uint32) []byte {
	packet := make([]byte, 20)
	if first {
		packet[0] = rtpProtoExtended
	} else {
		packet[0] = rtpProtoVersion
	}
	packet[1] = rtpTypeSync
	binary.BigEndian.PutUint16(packet[2:4], syncSeqNo)
	binary.BigEndian.PutUint32(packet[4:8], nowWithoutLatency)
	binary.BigEndian.PutUint32(packet[8:12], lastSyncSec)
	binary.BigEndian.PutUint32(packet[12:16], lastSyncFrac)
	binary.BigEndian.PutUint32(packet[16:20], now)
	return packet
}

// TimingReply builds the 32-byte NTP-style reply to a receiver timing request.
//
// Layout: proto(1) type(1) seqno(2) padding(4) reftime_sec(4) reftime_frac(4)
// recvtime_sec(4) recvtime_frac(4) sendtime_sec(4) sendtime_frac(4).
func TimingReply(proto byte, reftimeSec, reftimeFrac, nowSec, nowFrac uint32) []byte {
	packet := make([]byte, 32)
	packet[0] = proto
	packet[1] = rtpTypeTiming | rtpMarkerBit
	binary.BigEndian.PutUint16(packet[2:4], timingSeqNo)
	binary.BigEndian.PutUint32(packet[4:8], 0)
	binary.BigEndian.PutUint32(packet[8:12], reftimeSec)
	binary.BigEndian.PutUint32(packet[12:16], reftimeFrac)
	binary.BigEndian.PutUint32(packet[16:20], nowSec)
	binary.BigEndian.PutUint32(packet[20:24], nowFrac)
	binary.BigEndian.PutUint32(packet[24:28], nowSec)
	binary.BigEndian.PutUint32(packet[28:32], nowFrac)
	return packet
}

// TimingRequest is a decoded receiver timing probe.
type TimingRequest struct {
	Proto        byte
	SendTimeSec  uint32
	SendTimeFrac uint32
}

// ParseTimingRequest decodes a receiver timing probe. It returns false when
// the datagram is too short to be a timing packet.
func ParseTimingRequest(data []byte) (TimingRequest, bool) {
	if len(data) < 32 {
		return TimingRequest{}, false
	}
	return TimingRequest{
		Proto:        data[0],
		SendTimeSec:  binary.BigEndian.Uint32(data[24:28]),
		SendTimeFrac: binary.BigEndian.Uint32(data[28:32]),
	}, true
}

// ---------------------------------------------------------------------------
// Volume mapping
// ---------------------------------------------------------------------------

// PercentToDBFS maps a 0..100 UI volume onto the AirPlay dB scale.
//
// 100 -> 0.0 dB (full scale), 0 -> -144.0 dB (the ecosystem's "muted" value).
// Everything in between is linear in dB over the [-30, 0] window, which is the
// convention the design document and cliairplay both use.
func PercentToDBFS(percent int) float64 {
	if percent <= 0 {
		return -144.0
	}
	if percent >= 100 {
		return 0.0
	}
	return -30.0 + (float64(percent)/100.0)*30.0
}

// DBFSToPercent is the inverse of PercentToDBFS.
func DBFSToPercent(dbfs float64) int {
	if dbfs <= -30.0 {
		return 0
	}
	if dbfs >= 0 {
		return 100
	}
	return int((dbfs + 30.0) / 30.0 * 100.0)
}
