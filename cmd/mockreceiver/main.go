// Command mockreceiver is a headless AirPlay 1 (RAOP) receiver used as a
// stand-in for a HomePod when no Apple hardware is available.
//
// It advertises itself over mDNS exactly like a real receiver
// (_raop._tcp + _airplay._tcp with the same TXT records), speaks the RTSP
// handshake, receives RTP audio over UDP, answers the sender's clock probes
// with its own timing requests, and writes every decoded sample to a WAV file.
//
// The WAV is the acoustic evidence: it can be played back, compared against the
// source track, and analysed for dropouts. That is a stronger and more
// repeatable check than listening to a speaker, and it is the only option on a
// build host without an audio device.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/grandcat/zeroconf"

	"github.com/lyranest/lyranest-airplay-bridge/internal/raop"
)

func main() {
	var (
		name        = flag.String("name", "Mock-HomePod", "advertised receiver name")
		port        = flag.Int("port", 7000, "RTSP listen port")
		wavPath     = flag.String("wav", "capture.wav", "WAV file to write captured audio to")
		duration    = flag.Duration("duration", 0, "stop automatically after this long (0 = run until interrupted)")
		useMDNS     = flag.Bool("mdns", true, "advertise the receiver over mDNS")
		mac         = flag.String("mac", "AA:BB:CC:DD:EE:FF", "advertised hardware address")
		verbose     = flag.Bool("verbose", false, "log every RTSP request and periodic statistics")
		rate        = flag.Int("samplerate", 44100, "sample rate to advertise")
		reportEvery = flag.Duration("report", 10*time.Second, "statistics reporting interval (0 disables)")
		exitOnEnd   = flag.Bool("exit-after-session", false, "write the capture and exit once the RTSP session ends")
		grace       = flag.Duration("session-grace", 2*time.Second, "how long to keep capturing after the session ends")
	)
	flag.Parse()

	receiver := &receiver{
		name:       *name,
		mac:        strings.ToUpper(strings.ReplaceAll(*mac, "-", ":")),
		port:       *port,
		wavPath:    *wavPath,
		sampleRate: *rate,
		verbose:    *verbose,
		exitOnEnd:  *exitOnEnd,
		grace:      *grace,
	}

	if err := receiver.listen(); err != nil {
		log.Fatalf("mockreceiver: %v", err)
	}
	defer receiver.closeSockets()

	stopMDNS := func() {}
	if *useMDNS {
		shutdown, err := receiver.advertise()
		if err != nil {
			log.Printf("mockreceiver: mDNS advertisement failed: %v", err)
			log.Printf("mockreceiver: register the device manually instead, e.g. POST /api/devices/manual {\"address\":\"<this host ip>\",\"port\":%d}", *port)
		} else {
			stopMDNS = shutdown
		}
	}
	defer stopMDNS()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *duration > 0 {
		var cancelTimer context.CancelFunc
		ctx, cancelTimer = context.WithTimeout(ctx, *duration)
		defer cancelTimer()
	}
	receiver.stop = cancel

	if *reportEvery > 0 {
		go receiver.reportLoop(ctx, *reportEvery)
	}

	log.Printf("mockreceiver %q listening on 0.0.0.0:%d (mac %s, capture %s)", receiver.name, receiver.port, receiver.mac, receiver.wavPath)

	go receiver.acceptLoop(ctx)

	<-ctx.Done()
	receiver.shutdown()
	receiver.printSummary()
	if err := receiver.writeWAV(); err != nil {
		log.Printf("mockreceiver: writing capture failed: %v", err)
		os.Exit(1)
	}
	log.Printf("mockreceiver: capture written to %s (%s of audio)", receiver.wavPath, time.Duration(receiver.frames)*time.Second/time.Duration(receiver.sampleRate))
}

type receiver struct {
	name       string
	mac        string
	port       int
	wavPath    string
	sampleRate int
	verbose    bool
	// exitOnEnd makes the receiver shut down once its RTSP session ends, so a
	// verification run never has to guess a wall-clock lifetime.
	exitOnEnd bool
	grace     time.Duration
	// stop cancels the run; set by main.
	stop func()
	// sessionsServed counts completed sessions, used by exitOnEnd.
	sessionsServed atomic.Int64

	listener net.Listener

	mu       sync.Mutex
	sessions map[string]*session

	// capture is the decoded interleaved L16 audio, in arrival order.
	captureMu sync.Mutex
	pcm       []byte
	frames    int64

	stats stats
}

type stats struct {
	audioPackets   atomic.Int64
	audioDatagrams atomic.Int64
	audioBytes     atomic.Int64
	gaps           atomic.Int64
	duplicates     atomic.Int64
	retransmits    atomic.Int64
	syncPackets    atomic.Int64
	timingReplies  atomic.Int64
	timingSent     atomic.Int64
	rtspRequests   atomic.Int64
	lastSeq        atomic.Int64
}

func (r *receiver) listen() error {
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", r.port))
	if err != nil {
		return fmt.Errorf("listen on %d: %w", r.port, err)
	}
	r.listener = listener
	r.sessions = map[string]*session{}
	return nil
}

func (r *receiver) closeSockets() {
	if r.listener != nil {
		_ = r.listener.Close()
	}
}

// sessionEnded stops the receiver once its session is over, after a short
// grace period so any in-flight flush tail still lands in the capture.
func (r *receiver) sessionEnded() {
	if !r.exitOnEnd || r.stop == nil {
		return
	}
	if r.sessionsServed.Add(1) > 1 {
		return
	}
	grace := r.grace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	go func() {
		time.Sleep(grace)
		log.Printf("mockreceiver: session ended, shutting down to write the capture")
		r.stop()
	}()
}

// advertise publishes the two mDNS services a real AirPlay receiver exposes.
func (r *receiver) advertise() (func(), error) {
	raopTXT := []string{
		"txtvers=1",
		"ch=2",
		"cn=0,1", // codec 0 = uncompressed L16, 1 = ALAC
		"et=0,3,5",
		"sv=false",
		"da=true",
		"sr=" + strconv.Itoa(r.sampleRate),
		"ss=16",
		"pw=false",
		"vn=65537",
		"tp=UDP",
		"md=0,1,2",
		"sm=false",
		"ek=1",
		"vs=130.14",
		"am=AudioAccessory5,1",
	}
	instance := fmt.Sprintf("%s@%s", strings.ReplaceAll(r.mac, ":", ""), r.name)

	raopServer, err := zeroconf.Register(instance, "_raop._tcp", "local.", r.port, raopTXT, nil)
	if err != nil {
		return nil, fmt.Errorf("register _raop._tcp: %w", err)
	}

	airplayTXT := []string{
		"deviceid=" + r.mac,
		"features=0x4A7FCA00,0x3C356BD0",
		// 0x4 advertises audio support. The password bit is 0x80 and the
		// pairing-required bits are 0x8 / 0x200; none are set, which is what an
		// open receiver looks like.
		"flags=0x4",
		"sf=0x4",
		"model=AudioAccessory5,1",
		"srcvers=130.14",
		"protovers=1.1",
		"pk=" + strings.Repeat("0", 64),
		"pi=" + "00000000-0000-0000-0000-000000000000",
		"gid=" + "00000000-0000-0000-0000-000000000000",
	}
	airplayServer, err := zeroconf.Register(r.name, "_airplay._tcp", "local.", r.port, airplayTXT, nil)
	if err != nil {
		raopServer.Shutdown()
		return nil, fmt.Errorf("register _airplay._tcp: %w", err)
	}

	log.Printf("mockreceiver: advertised _raop._tcp %q and _airplay._tcp %q on port %d", instance, r.name, r.port)
	return func() {
		raopServer.Shutdown()
		airplayServer.Shutdown()
	}, nil
}

func (r *receiver) acceptLoop(ctx context.Context) {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if strings.Contains(err.Error(), "use of closed") {
				return
			}
			log.Printf("mockreceiver: accept: %v", err)
			continue
		}
		go r.serveConn(ctx, conn)
	}
}

func (r *receiver) shutdown() {
	r.mu.Lock()
	sessions := make([]*session, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
}

func (r *receiver) reportLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("mockreceiver stats: datagrams=%d packets=%d bytes=%d gaps=%d dupes=%d retransmit_reqs=%d sync=%d timing=%d frames=%d",
				r.stats.audioDatagrams.Load(),
				r.stats.audioPackets.Load(),
				r.stats.audioBytes.Load(),
				r.stats.gaps.Load(),
				r.stats.duplicates.Load(),
				r.stats.retransmits.Load(),
				r.stats.syncPackets.Load(),
				r.stats.timingReplies.Load(),
				atomic.LoadInt64(&r.frames),
			)
		}
	}
}

func (r *receiver) printSummary() {
	log.Printf("mockreceiver summary: rtsp_requests=%d audio_datagrams=%d audio_packets=%d audio_bytes=%d gaps=%d duplicates=%d retransmit_requests=%d sync_packets=%d timing_probes=%d timing_replies=%d frames=%d",
		r.stats.rtspRequests.Load(),
		r.stats.audioDatagrams.Load(),
		r.stats.audioPackets.Load(),
		r.stats.audioBytes.Load(),
		r.stats.gaps.Load(),
		r.stats.duplicates.Load(),
		r.stats.retransmits.Load(),
		r.stats.syncPackets.Load(),
		r.stats.timingSent.Load(),
		r.stats.timingReplies.Load(),
		atomic.LoadInt64(&r.frames),
	)
}

// appendPCM stores decoded audio in arrival order, padding any sequence gap
// with silence so the capture keeps a truthful duration.
func (r *receiver) appendPCM(payload []byte) {
	r.captureMu.Lock()
	r.pcm = append(r.pcm, payload...)
	r.frames += int64(len(payload) / 4)
	r.captureMu.Unlock()
}

func (r *receiver) appendSilence(frames int) {
	if frames <= 0 {
		return
	}
	silence := make([]byte, frames*4)
	r.captureMu.Lock()
	r.pcm = append(r.pcm, silence...)
	r.frames += int64(frames)
	r.captureMu.Unlock()
}

// writeWAV renders the capture as a 16-bit stereo PCM WAV file.
func (r *receiver) writeWAV() error {
	r.captureMu.Lock()
	pcm := make([]byte, len(r.pcm))
	copy(pcm, r.pcm)
	r.captureMu.Unlock()

	if len(pcm) == 0 {
		return fmt.Errorf("no audio captured")
	}

	file, err := os.Create(r.wavPath)
	if err != nil {
		return err
	}
	defer file.Close()

	const (
		channels      = 2
		bitsPerSample = 16
	)
	byteRate := r.sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8

	header := make([]byte, 0, 44)
	header = append(header, []byte("RIFF")...)
	header = binary.LittleEndian.AppendUint32(header, uint32(36+len(pcm)))
	header = append(header, []byte("WAVEfmt ")...)
	header = binary.LittleEndian.AppendUint32(header, 16)
	header = binary.LittleEndian.AppendUint16(header, 1)
	header = binary.LittleEndian.AppendUint16(header, channels)
	header = binary.LittleEndian.AppendUint32(header, uint32(r.sampleRate))
	header = binary.LittleEndian.AppendUint32(header, uint32(byteRate))
	header = binary.LittleEndian.AppendUint16(header, uint16(blockAlign))
	header = binary.LittleEndian.AppendUint16(header, bitsPerSample)
	header = append(header, []byte("data")...)
	header = binary.LittleEndian.AppendUint32(header, uint32(len(pcm)))

	if _, err := file.Write(header); err != nil {
		return err
	}
	_, err = file.Write(pcm)
	return err
}

// session is one RTSP control connection and its UDP sockets.
type session struct {
	receiver *receiver
	conn     net.Conn

	id                uint32
	remoteIP          string
	remoteTimingPort  int
	remoteControlPort int

	audioConn   *net.UDPConn
	controlConn *net.UDPConn
	timingConn  *net.UDPConn

	// remoteControlAddr is the sender's control port, taken from its SETUP
	// request. Retransmission requests must go there, not to our own socket.
	remoteControlAddr *net.UDPAddr

	codec      string
	sampleRate int
	channels   int
	bitDepth   int

	recording atomic.Bool
	closed    atomic.Bool

	nextSeq uint16
	haveSeq bool
	volume  string

	writeMu sync.Mutex
}

func (r *receiver) serveConn(ctx context.Context, conn net.Conn) {
	s := &session{
		receiver:   r,
		conn:       conn,
		id:         uint32(time.Now().UnixNano() & 0xFFFFFFFF),
		sampleRate: r.sampleRate,
		channels:   2,
		bitDepth:   16,
	}
	r.mu.Lock()
	r.sessions[fmt.Sprintf("%d", s.id)] = s
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.sessions, fmt.Sprintf("%d", s.id))
		r.mu.Unlock()
		s.close()
		r.sessionEnded()
	}()

	reader := newRequestReader(conn)
	for {
		request, err := reader.read()
		if err != nil {
			if err != io.EOF && ctx.Err() == nil && !s.closed.Load() {
				log.Printf("mockreceiver: rtsp read: %v", err)
			}
			return
		}
		r.stats.rtspRequests.Add(1)
		if r.verbose {
			log.Printf("mockreceiver <- %s %s (cseq=%s, %d body bytes)", request.Method, request.URI, request.Headers["cseq"], len(request.Body))
		}
		response := s.handle(request)
		if err := writeResponse(conn, response); err != nil {
			return
		}
		if response.closeConnection {
			return
		}
	}
}

func (s *session) handle(request *rtspRequest) *rtspResponse {
	cseq := request.Headers["cseq"]
	base := map[string]string{"CSeq": cseq, "Server": "AirTunes/130.14"}

	switch request.Method {
	case "OPTIONS":
		base["Public"] = "ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, TEARDOWN, OPTIONS, GET_PARAMETER, SET_PARAMETER, POST, GET"
		return &rtspResponse{Code: 200, Message: "OK", Headers: base}

	case "ANNOUNCE":
		s.parseSDP(string(request.Body))
		base["Content-Type"] = "application/sdp"
		return &rtspResponse{Code: 200, Message: "OK", Headers: base}

	case "SETUP":
		return s.handleSetup(base, request)

	case "RECORD":
		s.recording.Store(true)
		base["Audio-Latency"] = strconv.Itoa(raop.LatencyFrames)
		return &rtspResponse{Code: 200, Message: "OK", Headers: base}

	case "FLUSH":
		// A flush discards everything the receiver has buffered; the capture
		// keeps what was already written so the evidence stays complete.
		s.haveSeq = false
		return &rtspResponse{Code: 200, Message: "OK", Headers: base}

	case "SET_PARAMETER":
		if contentType := strings.ToLower(request.Headers["content-type"]); strings.Contains(contentType, "text/parameters") {
			s.parseParameters(string(request.Body))
		}
		return &rtspResponse{Code: 200, Message: "OK", Headers: base}

	case "GET_PARAMETER":
		base["Content-Type"] = "text/parameters"
		return &rtspResponse{Code: 200, Message: "OK", Headers: base, Body: []byte("volume: " + s.volume + "\r\n")}

	case "POST":
		if strings.HasPrefix(request.URI, "/feedback") {
			return &rtspResponse{Code: 200, Message: "OK", Headers: base}
		}
		if strings.HasPrefix(request.URI, "/command") {
			return &rtspResponse{Code: 200, Message: "OK", Headers: base}
		}
		return &rtspResponse{Code: 404, Message: "Not Found", Headers: base}

	case "TEARDOWN":
		s.recording.Store(false)
		response := &rtspResponse{Code: 200, Message: "OK", Headers: base, closeConnection: true}
		return response

	case "GET":
		if strings.HasPrefix(request.URI, "/info") {
			// A real receiver answers with a binary plist. Returning 404 is a
			// documented, supported outcome: pyatv logs "device does not
			// support /info" and continues.
			return &rtspResponse{Code: 404, Message: "Not Found", Headers: base}
		}
		return &rtspResponse{Code: 404, Message: "Not Found", Headers: base}

	default:
		return &rtspResponse{Code: 405, Message: "Method Not Allowed", Headers: base}
	}
}

func (s *session) parseSDP(body string) {
	for _, line := range strings.Split(body, "\r\n") {
		switch {
		case strings.HasPrefix(line, "c=IN IP4 "):
			s.remoteIP = strings.TrimSpace(strings.TrimPrefix(line, "c=IN IP4 "))
		case strings.HasPrefix(line, "a=rtpmap:"):
			if _, value, found := strings.Cut(line, " "); found {
				s.codec = strings.TrimSpace(value)
				parts := strings.Split(s.codec, "/")
				if len(parts) >= 2 {
					if rate, err := strconv.Atoi(parts[1]); err == nil {
						s.sampleRate = rate
					}
				}
				if len(parts) >= 3 {
					if channels, err := strconv.Atoi(parts[2]); err == nil {
						s.channels = channels
					}
				}
			}
		}
	}
	if s.remoteIP == "" {
		s.remoteIP = strings.TrimSpace(strings.Split(s.conn.RemoteAddr().String(), ":")[0])
	}
	if s.receiver.verbose {
		log.Printf("mockreceiver: ANNOUNCE codec=%q rate=%d channels=%d from %s", s.codec, s.sampleRate, s.channels, s.remoteIP)
	}
}

func (s *session) parseParameters(body string) {
	for _, line := range strings.Split(body, "\r\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "volume":
			s.volume = strings.TrimSpace(value)
			log.Printf("mockreceiver: volume set to %s dB", s.volume)
		case "progress":
			if s.receiver.verbose {
				log.Printf("mockreceiver: progress %s", strings.TrimSpace(value))
			}
		}
	}
}

func (s *session) handleSetup(base map[string]string, request *rtspRequest) *rtspResponse {
	options := parseTransportOptions(request.Headers["transport"])

	var err error
	s.audioConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return &rtspResponse{Code: 500, Message: "Internal Server Error", Headers: base}
	}
	s.controlConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return &rtspResponse{Code: 500, Message: "Internal Server Error", Headers: base}
	}
	s.timingConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return &rtspResponse{Code: 500, Message: "Internal Server Error", Headers: base}
	}

	remoteIP := net.ParseIP(s.remoteIP)
	// The sender's own control and timing ports come from its SETUP request.
	// Retransmission requests must be sent back to the sender's control port,
	// not to our own.
	if raw, ok := options["control_port"]; ok {
		if port, convErr := strconv.Atoi(raw); convErr == nil {
			s.remoteControlPort = port
		}
	}
	if raw, ok := options["timing_port"]; ok {
		if port, convErr := strconv.Atoi(raw); convErr == nil {
			s.remoteTimingPort = port
		}
	}
	s.remoteControlAddr = &net.UDPAddr{IP: remoteIP, Port: s.remoteControlPort}

	go s.readAudio()
	go s.readControl()
	go s.readTiming()
	go s.probeClock()

	audioPort := s.audioConn.LocalAddr().(*net.UDPAddr).Port
	controlPort := s.controlConn.LocalAddr().(*net.UDPAddr).Port
	timingPort := s.timingConn.LocalAddr().(*net.UDPAddr).Port

	base["Session"] = strconv.FormatUint(uint64(s.id), 10)
	base["Transport"] = fmt.Sprintf(
		"RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;server_port=%d;control_port=%d;timing_port=%d",
		audioPort, controlPort, timingPort,
	)
	log.Printf("mockreceiver: SETUP session=%d audio_port=%d control_port=%d timing_port=%d sender_timing_port=%d",
		s.id, audioPort, controlPort, timingPort, s.remoteTimingPort)
	return &rtspResponse{Code: 200, Message: "OK", Headers: base}
}

// readAudio receives RTP audio and decodes L16 PCM.
func (s *session) readAudio() {
	buf := make([]byte, 8192)
	reported := 0
	for {
		if s.closed.Load() {
			return
		}
		_ = s.audioConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, addr, err := s.audioConn.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			return
		}
		s.receiver.stats.audioDatagrams.Add(1)
		if n < 12 {
			if s.receiver.verbose && reported < 5 {
				reported++
				log.Printf("mockreceiver audio: short datagram (%d bytes) from %s", n, addr)
			}
			continue
		}
		packetType := buf[1] & 0x7F
		switch packetType {
		case 0x60: // audio
			s.handleAudioPacket(buf[:n])
		case 0x56: // retransmit reply: 4-byte prefix then the original packet
			if n > 16 {
				s.handleAudioPacket(buf[4:n])
			}
		default:
			if s.receiver.verbose && reported < 5 {
				reported++
				log.Printf("mockreceiver audio: unhandled packet type 0x%02X (%d bytes) from %s", packetType, n, addr)
			}
		}
	}
}

func (s *session) handleAudioPacket(packet []byte) {
	if len(packet) < 12 {
		return
	}
	seqno := binary.BigEndian.Uint16(packet[2:4])
	payload := packet[12:]

	s.receiver.stats.audioPackets.Add(1)
	s.receiver.stats.audioBytes.Add(int64(len(payload)))
	s.receiver.stats.lastSeq.Store(int64(seqno))

	if !s.haveSeq {
		s.haveSeq = true
		s.nextSeq = seqno
	} else if seqno != s.nextSeq {
		delta := int32(uint16(seqno - s.nextSeq))
		if delta > 0 && delta < 512 {
			// A forward gap: ask for a retransmission, and pad the capture so
			// the timeline length stays truthful.
			s.receiver.stats.gaps.Add(1)
			s.requestRetransmit(s.nextSeq, uint16(delta))
			s.receiver.appendSilence(int(delta) * raop.FramesPerPacket)
			s.nextSeq = seqno
		} else if delta < 0 {
			// Late or duplicated packet.
			s.receiver.stats.duplicates.Add(1)
			return
		}
	}

	s.receiver.appendPCM(decodeL16ToLE(payload))
	s.nextSeq = seqno + 1
}

// requestRetransmit exercises the sender's packet backlog.
func (s *session) requestRetransmit(seqno uint16, count uint16) {
	if s.controlConn == nil || s.remoteControlAddr == nil {
		return
	}
	packet := make([]byte, 8)
	packet[0] = 0x80
	packet[1] = 0x55
	binary.BigEndian.PutUint16(packet[2:4], 1)
	binary.BigEndian.PutUint16(packet[4:6], seqno)
	binary.BigEndian.PutUint16(packet[6:8], count)
	s.receiver.stats.retransmits.Add(1)
	_, _ = s.controlConn.WriteToUDP(packet, s.remoteControlAddr)
}

// readControl receives sync packets and retransmission replies.
func (s *session) readControl() {
	buf := make([]byte, 2048)
	var (
		haveSync    bool
		lastNow     uint32
		lastSyncRTP uint32
	)
	for {
		if s.closed.Load() {
			return
		}
		_ = s.controlConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := s.controlConn.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			return
		}
		if n < 8 {
			continue
		}
		switch buf[1] & 0x7F {
		case 0x54: // sync packet
			if n < 20 {
				continue
			}
			s.receiver.stats.syncPackets.Add(1)
			nowWithoutLatency := binary.BigEndian.Uint32(buf[4:8])
			sec := binary.BigEndian.Uint32(buf[8:12])
			frac := binary.BigEndian.Uint32(buf[12:16])
			now := binary.BigEndian.Uint32(buf[16:20])
			ntp := uint64(sec)<<32 | uint64(frac)
			syncRTP := raop.NTPToRTP(ntp, s.sampleRate)

			// The sender's RTP timeline is relative (it starts near zero) while
			// the NTP field is absolute, so only the *deltas* between
			// consecutive sync packets are comparable. A correct sender keeps
			// them equal, and keeps now - now_without_latency at the latency.
			if haveSync && s.receiver.verbose {
				deltaNow := int64(int32(now - lastNow))
				deltaNTP := int64(int32(syncRTP - lastSyncRTP))
				log.Printf("mockreceiver sync: latency_frames=%d delta_now=%d delta_ntp=%d skew_frames=%d",
					int64(now)-int64(nowWithoutLatency), deltaNow, deltaNTP, deltaNow-deltaNTP)
			}
			haveSync = true
			lastNow = now
			lastSyncRTP = syncRTP
		case 0x56: // retransmit reply
			if n > 16 {
				s.handleAudioPacket(buf[4:n])
			}
		}
	}
}

// readTiming receives replies to the clock probes we send.
func (s *session) readTiming() {
	buf := make([]byte, 256)
	for {
		if s.closed.Load() {
			return
		}
		_ = s.timingConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := s.timingConn.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			return
		}
		if n < 32 {
			continue
		}
		s.receiver.stats.timingReplies.Add(1)
		if s.receiver.verbose {
			reftime := binary.BigEndian.Uint32(buf[8:12])
			recvtime := binary.BigEndian.Uint32(buf[16:20])
			log.Printf("mockreceiver timing reply: reftime=%d recvtime=%d delta=%d", reftime, recvtime, int64(recvtime)-int64(reftime))
		}
	}
}

// probeClock sends NTP-style probes to the sender, exactly as a real receiver
// does. The sender's responder is what keeps the audio timeline anchored.
func (s *session) probeClock() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-time.After(200 * time.Millisecond):
		}
		if s.closed.Load() {
			return
		}
		if s.remoteTimingPort == 0 || s.remoteIP == "" {
			continue
		}
		now := time.Now()
		sec, frac := raop.NTPParts(raop.NTPNow(now.Unix(), int64(now.Nanosecond())))
		packet := make([]byte, 32)
		packet[0] = 0x80
		packet[1] = 0x53
		binary.BigEndian.PutUint16(packet[2:4], 7)
		binary.BigEndian.PutUint32(packet[24:28], sec)
		binary.BigEndian.PutUint32(packet[28:32], frac)
		addr := &net.UDPAddr{IP: net.ParseIP(s.remoteIP), Port: s.remoteTimingPort}
		s.receiver.stats.timingSent.Add(1)
		_, _ = s.timingConn.WriteToUDP(packet, addr)
	}
}

func (s *session) close() {
	if s.closed.Swap(true) {
		return
	}
	for _, conn := range []*net.UDPConn{s.audioConn, s.controlConn, s.timingConn} {
		if conn != nil {
			_ = conn.Close()
		}
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// decodeL16ToLE converts big-endian L16 PCM into the little-endian layout a WAV
// file requires.
func decodeL16ToLE(payload []byte) []byte {
	out := make([]byte, len(payload)&^1)
	for i := 0; i+1 < len(payload); i += 2 {
		out[i] = payload[i+1]
		out[i+1] = payload[i]
	}
	return out
}

func isTimeout(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}
