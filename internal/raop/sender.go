package raop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Errors reported by the sender.
var (
	ErrClosed        = errors.New("raop session closed")
	ErrNotStreaming  = errors.New("raop session is not streaming")
	ErrALACOnly      = errors.New("receiver does not accept uncompressed L16 audio")
	ErrHandshakeFail = errors.New("raop handshake failed")
)

// Config describes one sender session.
type Config struct {
	// Address is the receiver IPv4/IPv6 literal.
	Address string
	// Port is the receiver RTSP port (7000 for AirPlay 2 devices).
	Port int
	// Format is the PCM layout that will be written to Write.
	Format AudioFormat
	// LocalIP optionally pins the local interface. When empty the interface
	// chosen by the TCP route to Address is used.
	LocalIP string
	// Volume is the initial 0..100 volume applied after RECORD.
	Volume int
	// FeedbackInterval is the /feedback keep-alive cadence. Defaults to 2s.
	FeedbackInterval time.Duration
	// Logger receives structured progress output.
	Logger *slog.Logger
	// OnProgress is invoked roughly once per second with the number of frames
	// delivered to the receiver.
	OnProgress func(frames uint64)
}

// Sender streams uncompressed L16 PCM to one RAOP receiver.
type Sender struct {
	cfg    Config
	logger *slog.Logger

	conn       *rtspConn
	audioConn  *net.UDPConn
	controlUDP *net.UDPConn
	timingUDP  *net.UDPConn

	audioAddr   *net.UDPAddr
	controlAddr *net.UDPAddr

	serverPort  int
	controlPort int
	timingPort  int

	stateMu sync.Mutex
	state   State
	lastErr error

	// volume is the current 0..100 volume.
	volume int

	// pause control for the streaming loop.
	pauseMu  sync.Mutex
	paused   bool
	pauseCh  chan struct{}
	resumeCh chan struct{}

	// flushRequested asks the streaming loop to emit RTSP FLUSH and re-anchor.
	flushMu        sync.Mutex
	flushRequested bool

	stopOnce sync.Once
	stopCh   chan struct{}

	// startTS anchors RTP timestamps to the wall clock.
	startTS uint32
	headTS  uint32
	seqno   uint16

	// framesSent counts audio frames handed to the receiver.
	framesSent uint64

	// pacing anchors the send loop to wall-clock time. anchorWall is the
	// instant at which anchorFrames had been delivered; both are re-based on
	// resume and flush so a pause never causes a catch-up burst.
	anchorWall   time.Time
	anchorFrames uint64

	// backlog keeps recent packets for retransmission.
	backlogMu sync.Mutex
	backlog   map[uint16][]byte
	backlogQ  []uint16

	rtpMu   sync.Mutex
	ssrc    uint32
	started bool
}

// State is the lifecycle state of a sender session.
type State string

// Sender lifecycle states.
const (
	StateIdle      State = "idle"
	StateBuffering State = "buffering"
	StatePlaying   State = "playing"
	StatePaused    State = "paused"
	StateStopped   State = "stopped"
	StateError     State = "error"
)

const backlogSize = 1000

// Dial performs the full RAOP handshake: TCP connect, OPTIONS, ANNOUNCE and
// SETUP. It does not start audio; call Record then Stream.
func Dial(ctx context.Context, cfg Config) (*Sender, error) {
	if cfg.Format.SampleRate == 0 {
		cfg.Format = AudioFormat{SampleRate: RTPTimeScale, Channels: 2, BitDepth: 16}
	}
	if err := cfg.Format.Validate(); err != nil {
		return nil, err
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	if cfg.FeedbackInterval <= 0 {
		cfg.FeedbackInterval = 2 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dialer := &net.Dialer{Timeout: 8 * time.Second}
	rawConn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s:%d: %v", ErrHandshakeFail, cfg.Address, cfg.Port, err)
	}

	localIP := cfg.LocalIP
	if localIP == "" {
		if tcpAddr, ok := rawConn.LocalAddr().(*net.TCPAddr); ok {
			localIP = tcpAddr.IP.String()
		}
	}

	conn, err := newRTSPConn(rawConn, localIP, cfg.Address)
	if err != nil {
		_ = rawConn.Close()
		return nil, err
	}

	sender := &Sender{
		cfg:        cfg,
		logger:     logger,
		conn:       conn,
		state:      StateIdle,
		volume:     cfg.Volume,
		pauseCh:    make(chan struct{}),
		resumeCh:   make(chan struct{}),
		stopCh:     make(chan struct{}),
		backlog:    make(map[uint16][]byte),
		ssrc:       conn.SessionID(),
		startTS:    NTPToRTP(NTPNow(time.Now().Unix(), int64(time.Now().Nanosecond())), cfg.Format.SampleRate),
		timingPort: 0,
	}
	sender.headTS = sender.startTS

	if err := sender.handshake(ctx); err != nil {
		_ = sender.Close()
		return nil, err
	}
	return sender, nil
}

func (s *Sender) handshake(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, err := s.conn.Do("OPTIONS", "*", "", nil, nil); err != nil {
		return fmt.Errorf("%w: OPTIONS: %v", ErrHandshakeFail, err)
	}

	sdp := BuildSDP(s.conn.SessionID(), s.conn.localIP, s.conn.remoteIP, s.cfg.Format)
	announce, err := s.conn.Do("ANNOUNCE", "", "application/sdp", nil, []byte(sdp))
	if err != nil {
		return fmt.Errorf("%w: ANNOUNCE: %v", ErrHandshakeFail, err)
	}
	if announce.Code == 401 {
		return fmt.Errorf("%w: ANNOUNCE rejected with 401 (device requires a password)", ErrHandshakeFail)
	}
	if announce.Code >= 400 {
		return fmt.Errorf("%w: ANNOUNCE returned %d %s", ErrHandshakeFail, announce.Code, announce.Message)
	}

	if err := s.openUDPSockets(); err != nil {
		return err
	}

	transport := fmt.Sprintf(
		"RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=%d;timing_port=%d",
		s.controlPort, s.timingPort,
	)
	setup, err := s.conn.Do("SETUP", "", "", map[string]string{"Transport": transport}, nil)
	if err != nil {
		return fmt.Errorf("%w: SETUP: %v", ErrHandshakeFail, err)
	}
	if setup.Code >= 400 {
		return fmt.Errorf("%w: SETUP returned %d %s", ErrHandshakeFail, setup.Code, setup.Message)
	}

	params, options := parseTransport(setup.Header("Transport"))
	if !containsToken(params, "RTP/AVP/UDP") && !containsToken(params, "RTP/AVP/TCP") {
		s.logger.Debug("receiver returned unexpected transport", "transport", setup.Header("Transport"))
	}

	serverPort, err := strconv.Atoi(options["server_port"])
	if err != nil || serverPort <= 0 {
		return fmt.Errorf("%w: SETUP response has no usable server_port (%q)", ErrHandshakeFail, setup.Header("Transport"))
	}
	s.serverPort = serverPort

	if raw := options["control_port"]; raw != "" {
		if port, convErr := strconv.Atoi(raw); convErr == nil {
			s.controlPort = port
		}
	}
	if raw := options["timing_port"]; raw != "" {
		if port, convErr := strconv.Atoi(raw); convErr == nil {
			s.timingPort = port
		}
	}

	remoteIP := net.ParseIP(s.conn.remoteIP)
	s.audioAddr = &net.UDPAddr{IP: remoteIP, Port: s.serverPort}
	s.controlAddr = &net.UDPAddr{IP: remoteIP, Port: s.controlPort}

	s.logger.Info("raop session established",
		"device", s.conn.remoteIP,
		"rtsp_port", s.cfg.Port,
		"audio_port", s.serverPort,
		"control_port", s.controlPort,
		"timing_port", s.timingPort,
		"session_id", s.conn.SessionID(),
		"local_ip", s.conn.localIP,
	)
	return nil
}

func (s *Sender) openUDPSockets() error {
	var err error
	bindIP := net.ParseIP(s.conn.localIP)

	s.controlUDP, err = net.ListenUDP("udp", &net.UDPAddr{IP: bindIP, Port: 0})
	if err != nil {
		return fmt.Errorf("%w: control socket: %v", ErrHandshakeFail, err)
	}
	s.controlPort = s.controlUDP.LocalAddr().(*net.UDPAddr).Port

	s.timingUDP, err = net.ListenUDP("udp", &net.UDPAddr{IP: bindIP, Port: 0})
	if err != nil {
		return fmt.Errorf("%w: timing socket: %v", ErrHandshakeFail, err)
	}
	s.timingPort = s.timingUDP.LocalAddr().(*net.UDPAddr).Port

	s.audioConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: bindIP, Port: 0})
	if err != nil {
		return fmt.Errorf("%w: audio socket: %v", ErrHandshakeFail, err)
	}

	go s.serveTiming()
	go s.serveControl()
	return nil
}

// serveTiming answers the receiver's NTP-style clock probes.
func (s *Sender) serveTiming() {
	buf := make([]byte, 128)
	for {
		_ = s.timingUDP.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, addr, err := s.timingUDP.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return
		}
		request, ok := ParseTimingRequest(buf[:n])
		if !ok {
			continue
		}
		now := time.Now()
		nowSec, nowFrac := NTPParts(NTPNow(now.Unix(), int64(now.Nanosecond())))
		reply := TimingReply(request.Proto, request.SendTimeSec, request.SendTimeFrac, nowSec, nowFrac)
		_, _ = s.timingUDP.WriteToUDP(reply, addr)
	}
}

// serveControl answers retransmission requests from the receiver.
func (s *Sender) serveControl() {
	buf := make([]byte, 2048)
	for {
		_ = s.controlUDP.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, addr, err := s.controlUDP.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return
		}
		if n < 8 {
			continue
		}
		// Retransmission request: type 0x55 (marker bit masked off).
		if buf[1]&0x7F != 0x55 {
			continue
		}
		lostSeq := uint16(buf[4])<<8 | uint16(buf[5])
		lostCount := int(uint16(buf[6])<<8 | uint16(buf[7]))
		if lostCount > 128 {
			lostCount = 128
		}
		for i := 0; i < lostCount; i++ {
			packet, ok := s.lookupBacklog(lostSeq + uint16(i))
			if !ok {
				continue
			}
			// Retransmit reply: proto/type 0xD6 then the original packet.
			reply := make([]byte, 0, 4+len(packet))
			reply = append(reply, 0x80, 0xD6, packet[2], packet[3])
			reply = append(reply, packet...)
			_, _ = s.audioConn.WriteToUDP(reply, addr)
		}
	}
}

func (s *Sender) lookupBacklog(seqno uint16) ([]byte, bool) {
	s.backlogMu.Lock()
	defer s.backlogMu.Unlock()
	packet, ok := s.backlog[seqno]
	return packet, ok
}

func (s *Sender) rememberPacket(seqno uint16, packet []byte) {
	s.backlogMu.Lock()
	defer s.backlogMu.Unlock()
	if _, exists := s.backlog[seqno]; !exists {
		s.backlogQ = append(s.backlogQ, seqno)
	}
	s.backlog[seqno] = packet
	for len(s.backlogQ) > backlogSize {
		oldest := s.backlogQ[0]
		s.backlogQ = s.backlogQ[1:]
		delete(s.backlog, oldest)
	}
}

// State returns the current lifecycle state and last error.
func (s *Sender) State() (State, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.state, s.lastErr
}

func (s *Sender) setState(state State, err error) {
	s.stateMu.Lock()
	s.state = state
	if err != nil {
		s.lastErr = err
	}
	s.stateMu.Unlock()
}

// FramesSent returns the number of audio frames delivered to the receiver.
func (s *Sender) FramesSent() uint64 {
	s.rtpMu.Lock()
	defer s.rtpMu.Unlock()
	return s.framesSent
}

// Record sends RECORD, which tells the receiver to start rendering.
func (s *Sender) Record(ctx context.Context) error {
	response, err := s.conn.Do("RECORD", "", "", nil, nil)
	if err != nil {
		return fmt.Errorf("%w: RECORD: %v", ErrHandshakeFail, err)
	}
	if response.Code >= 400 {
		return fmt.Errorf("%w: RECORD returned %d %s", ErrHandshakeFail, response.Code, response.Message)
	}
	return nil
}

// SetVolume applies a 0..100 volume to the receiver.
func (s *Sender) SetVolume(percent int) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	dbfs := PercentToDBFS(percent)
	body := []byte("volume: " + strconv.FormatFloat(dbfs, 'f', 6, 64))
	response, err := s.conn.Do("SET_PARAMETER", "", "text/parameters", nil, body)
	if err != nil {
		return fmt.Errorf("SET_PARAMETER volume: %w", err)
	}
	if response.Code >= 400 {
		return fmt.Errorf("SET_PARAMETER volume returned %d %s", response.Code, response.Message)
	}
	s.stateMu.Lock()
	s.volume = percent
	s.stateMu.Unlock()
	return nil
}

// Volume returns the last volume applied through this session.
func (s *Sender) Volume() int {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.volume
}

// Flush tells the receiver to discard buffered audio and re-anchors the RTP
// timeline to the current instant. It is used for seek and for the native
// engine's pause/resume.
func (s *Sender) Flush(ctx context.Context) error {
	headers := map[string]string{
		"Session":  strconv.FormatUint(uint64(s.conn.SessionID()), 10),
		"RTP-Info": fmt.Sprintf("seq=%d;rtptime=%d", s.seqno, s.rtptime()),
	}
	response, err := s.conn.Do("FLUSH", "", "", headers, nil)
	if err != nil {
		return fmt.Errorf("FLUSH: %w", err)
	}
	if response.Code >= 400 {
		return fmt.Errorf("FLUSH returned %d %s", response.Code, response.Message)
	}
	s.reanchor()
	return nil
}

// RequestFlush asks the running Stream loop to flush at the next packet
// boundary. Safe to call from another goroutine.
func (s *Sender) RequestFlush() {
	s.flushMu.Lock()
	s.flushRequested = true
	s.flushMu.Unlock()
}

func (s *Sender) takeFlushRequest() bool {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	requested := s.flushRequested
	s.flushRequested = false
	return requested
}

// reanchor resets the RTP timestamp base so that the next packet is scheduled
// `latency` frames in the future, exactly like a fresh session start.
func (s *Sender) reanchor() {
	now := time.Now()
	ts := NTPToRTP(NTPNow(now.Unix(), int64(now.Nanosecond())), s.cfg.Format.SampleRate)
	s.rtpMu.Lock()
	s.startTS = ts
	s.headTS = ts
	s.rtpMu.Unlock()
}

func (s *Sender) rtptime() uint32 {
	s.rtpMu.Lock()
	defer s.rtpMu.Unlock()
	return s.headTS - s.startTS + LatencyFrames
}

// Pause stops audio delivery and sync packets while keeping the RTSP session
// and the receiver connection alive. RAOP has no native pause verb, so the
// sender simply stops feeding the receiver, which is what the AirPlay
// ecosystem's senders do on the RAOP-compatible path.
func (s *Sender) Pause() {
	s.pauseMu.Lock()
	if !s.paused {
		s.paused = true
		close(s.pauseCh)
		s.resumeCh = make(chan struct{})
	}
	s.pauseMu.Unlock()
	s.setState(StatePaused, nil)
}

// Resume restarts audio delivery, re-anchoring the RTP timeline.
func (s *Sender) Resume() {
	s.pauseMu.Lock()
	if s.paused {
		s.paused = false
		close(s.resumeCh)
		s.pauseCh = make(chan struct{})
	}
	s.pauseMu.Unlock()
	s.reanchor()
	s.setState(StatePlaying, nil)
}

func (s *Sender) pauseChannels() (chan struct{}, chan struct{}) {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	return s.pauseCh, s.resumeCh
}

// Stream writes PCM from r to the receiver until r reaches EOF, ctx is
// cancelled, or Close is called. It blocks.
//
// The caller must already have called Record. Each RTP packet carries
// FramesPerPacket frames of big-endian L16 audio and is paced against the wall
// clock so the receiver's latency buffer neither underruns nor overflows.
func (s *Sender) Stream(ctx context.Context, r io.Reader) error {
	if s.audioConn == nil || s.audioAddr == nil {
		return ErrClosed
	}

	packetSize := s.cfg.Format.PacketSize()
	buf := make([]byte, packetSize)

	s.setState(StatePlaying, nil)
	s.started = true

	feedbackCtx, cancelFeedback := context.WithCancel(ctx)
	defer cancelFeedback()
	go s.feedbackLoop(feedbackCtx)

	first := true
	s.rebasePacing()
	nextSync := time.Now().Add(time.Duration(SyncInterval) * time.Second)
	lastProgress := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-s.stopCh:
			return ErrClosed
		default:
		}

		// Honour a pause by parking here until Resume or shutdown.
		pauseCh, resumeCh := s.pauseChannels()
		select {
		case <-pauseCh:
			select {
			case <-resumeCh:
				s.rebasePacing()
				nextSync = time.Now().Add(time.Duration(SyncInterval) * time.Second)
			case <-s.stopCh:
				return ErrClosed
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
		}

		if s.takeFlushRequest() {
			if err := s.Flush(ctx); err != nil {
				s.logger.Warn("flush failed", "error", err)
			}
			s.rebasePacing()
		}

		n, readErr := io.ReadFull(r, buf)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			s.setState(StateError, readErr)
			return fmt.Errorf("read pcm: %w", readErr)
		}
		endOfStream := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
		if n == 0 && endOfStream {
			break
		}
		if n < packetSize {
			// Pad the final short packet so the RTP frame stays whole.
			for i := n; i < packetSize; i++ {
				buf[i] = 0
			}
		}

		packet := s.buildAudioPacket(buf, first)
		first = false

		if _, err := s.audioConn.WriteToUDP(packet, s.audioAddr); err != nil {
			s.setState(StateError, err)
			return fmt.Errorf("send audio: %w", err)
		}
		s.rememberPacket(s.seqno, packet)

		s.rtpMu.Lock()
		s.seqno++
		s.headTS += FramesPerPacket
		s.framesSent += FramesPerPacket
		frames := s.framesSent
		s.rtpMu.Unlock()

		if time.Now().After(nextSync) {
			s.sendSync()
			nextSync = time.Now().Add(time.Duration(SyncInterval) * time.Second)
		}
		if s.cfg.OnProgress != nil && time.Since(lastProgress) >= time.Second {
			s.cfg.OnProgress(frames)
			lastProgress = time.Now()
		}

		if endOfStream {
			break
		}

		// Pace to real time: sleep until this packet's slot has elapsed. When
		// we are already behind, skip the sleep and catch up naturally.
		if wait := time.Until(s.pacingTarget(frames)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-s.stopCh:
				timer.Stop()
				return ErrClosed
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}

	// Drain the receiver: keep the timeline alive for one latency window of
	// silence so the buffered tail is rendered instead of truncated.
	if err := s.drainSilence(ctx); err != nil {
		return err
	}

	s.setState(StateStopped, nil)
	return nil
}

// rebasePacing restarts the wall-clock pacing anchor at the current frame
// count. Called when the stream starts, resumes from a pause, or is flushed.
func (s *Sender) rebasePacing() {
	s.rtpMu.Lock()
	s.anchorWall = time.Now()
	s.anchorFrames = s.framesSent
	s.rtpMu.Unlock()
}

// pacingTarget returns the wall-clock instant at which `frames` should have
// been delivered.
func (s *Sender) pacingTarget(frames uint64) time.Time {
	s.rtpMu.Lock()
	anchorWall := s.anchorWall
	anchorFrames := s.anchorFrames
	s.rtpMu.Unlock()
	if anchorWall.IsZero() {
		return time.Now()
	}
	delivered := frames - anchorFrames
	return anchorWall.Add(time.Duration(delivered) * time.Second / time.Duration(s.cfg.Format.SampleRate))
}

func (s *Sender) buildAudioPacket(payload []byte, first bool) []byte {
	s.rtpMu.Lock()
	seqno := s.seqno
	timestamp := s.headTS - s.startTS + LatencyFrames
	ssrc := s.ssrc
	s.rtpMu.Unlock()
	return AudioPacket(seqno, timestamp, ssrc, first, payload)
}

func (s *Sender) sendSync() {
	s.rtpMu.Lock()
	head := s.headTS
	rtp := s.headTS - s.startTS + LatencyFrames
	s.rtpMu.Unlock()

	lastSec, lastFrac := NTPParts(RTPToNTP(head, s.cfg.Format.SampleRate))
	packet := SyncPacket(true, rtp-LatencyFrames, lastSec, lastFrac, rtp)
	if s.controlAddr != nil {
		_, _ = s.controlUDP.WriteToUDP(packet, s.controlAddr)
	}
}

func (s *Sender) drainSilence(ctx context.Context) error {
	packetSize := s.cfg.Format.PacketSize()
	silence := make([]byte, packetSize)
	remaining := LatencyFrames
	s.rebasePacing()
	for remaining > 0 {
		select {
		case <-s.stopCh:
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		packet := s.buildAudioPacket(silence, false)
		if _, err := s.audioConn.WriteToUDP(packet, s.audioAddr); err != nil {
			return nil
		}
		s.rtpMu.Lock()
		s.seqno++
		s.headTS += FramesPerPacket
		s.framesSent += FramesPerPacket
		frames := s.framesSent
		s.rtpMu.Unlock()
		remaining -= FramesPerPacket

		if wait := time.Until(s.pacingTarget(frames)); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-s.stopCh:
				timer.Stop()
				return ErrClosed
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
	return nil
}

func (s *Sender) feedbackLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.FeedbackInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			if _, err := s.conn.Do("POST", "/feedback", "", nil, nil); err != nil {
				s.logger.Debug("feedback failed", "error", err)
			}
		}
	}
}

// Close tears the session down: TEARDOWN, socket close, state stopped.
func (s *Sender) Close() error {
	var closeErr error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.conn != nil {
			headers := map[string]string{"Session": strconv.FormatUint(uint64(s.conn.SessionID()), 10)}
			if _, err := s.conn.Do("TEARDOWN", "", "", headers, nil); err != nil {
				s.logger.Debug("TEARDOWN failed", "error", err)
			}
			closeErr = s.conn.Close()
		}
		for _, conn := range []*net.UDPConn{s.audioConn, s.controlUDP, s.timingUDP} {
			if conn != nil {
				_ = conn.Close()
			}
		}
		s.setState(StateStopped, nil)
	})
	return closeErr
}

func parseTransport(transport string) ([]string, map[string]string) {
	var params []string
	options := map[string]string{}
	for _, part := range strings.Split(transport, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, found := strings.Cut(part, "=")
		if found {
			options[strings.TrimSpace(key)] = strings.TrimSpace(value)
			continue
		}
		params = append(params, part)
	}
	return params, options
}

func containsToken(params []string, token string) bool {
	for _, param := range params {
		if strings.EqualFold(param, token) {
			return true
		}
	}
	return false
}
