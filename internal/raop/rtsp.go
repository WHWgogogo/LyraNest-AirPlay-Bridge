package raop

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Response is a parsed RTSP reply.
type Response struct {
	Proto   string
	Code    int
	Message string
	Headers map[string]string
	Body    []byte
}

// Header returns a header value using a case-insensitive key lookup.
func (r *Response) Header(name string) string {
	if r == nil {
		return ""
	}
	return r.Headers[strings.ToLower(name)]
}

// rtspConn is a minimal RTSP/1.0 client connection. RTSP is HTTP-shaped, so the
// framing is a request line, headers, a blank line and an optional body whose
// length comes from Content-Length.
type rtspConn struct {
	conn    net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex

	cseq           int
	sessionID      uint32
	dacpID         string
	activeRemote   uint32
	localIP        string
	remoteIP       string
	readTimeout    time.Duration
	writeTimeout   time.Duration
	pendingRequest []byte
}

func newRTSPConn(conn net.Conn, localIP, remoteIP string) (*rtspConn, error) {
	sessionID, err := randomUint32()
	if err != nil {
		return nil, err
	}
	activeRemote, err := randomUint32()
	if err != nil {
		return nil, err
	}
	dacp, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	return &rtspConn{
		conn:         conn,
		reader:       bufio.NewReaderSize(conn, 16*1024),
		sessionID:    sessionID,
		dacpID:       strings.ToUpper(dacp),
		activeRemote: activeRemote,
		localIP:      localIP,
		remoteIP:     remoteIP,
		readTimeout:  10 * time.Second,
		writeTimeout: 10 * time.Second,
	}, nil
}

// SessionID is the RTSP session identifier, also used as the RTP SSRC.
func (c *rtspConn) SessionID() uint32 { return c.sessionID }

// URI is the request URI used after OPTIONS, e.g. rtsp://10.0.0.2/1234.
func (c *rtspConn) URI() string { return fmt.Sprintf("rtsp://%s/%d", c.localIP, c.sessionID) }

// Do sends one RTSP request and reads its reply. `uri` defaults to the session
// URI when empty.
func (c *rtspConn) Do(method, uri, contentType string, headers map[string]string, body []byte) (*Response, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	cseq := c.cseq
	c.cseq++

	if uri == "" {
		uri = c.URI()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\n", method, uri)
	fmt.Fprintf(&b, "CSeq: %d\r\n", cseq)
	fmt.Fprintf(&b, "DACP-ID: %s\r\n", c.dacpID)
	fmt.Fprintf(&b, "Active-Remote: %d\r\n", c.activeRemote)
	fmt.Fprintf(&b, "Client-Instance: %s\r\n", c.dacpID)
	fmt.Fprintf(&b, "User-Agent: %s\r\n", UserAgent)
	if contentType != "" {
		fmt.Fprintf(&b, "Content-Type: %s\r\n", contentType)
	}
	for key, value := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", key, value)
	}
	if body != nil {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")

	payload := make([]byte, 0, b.Len()+len(body))
	payload = append(payload, b.String()...)
	payload = append(payload, body...)
	c.pendingRequest = payload

	if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(payload); err != nil {
		return nil, fmt.Errorf("rtsp %s write: %w", method, err)
	}
	return c.readResponse(cseq)
}

func (c *rtspConn) readResponse(expectedCSeq int) (*Response, error) {
	for {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
			return nil, err
		}

		statusLine, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("rtsp read status: %w", err)
		}
		statusLine = strings.TrimRight(statusLine, "\r\n")
		if statusLine == "" {
			continue
		}

		parts := strings.SplitN(statusLine, " ", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("malformed rtsp status line %q", statusLine)
		}
		proto := parts[0]
		code, convErr := strconv.Atoi(parts[1])
		if convErr != nil {
			return nil, fmt.Errorf("malformed rtsp status code in %q", statusLine)
		}
		message := ""
		if len(parts) == 3 {
			message = parts[2]
		}

		headers := map[string]string{}
		for {
			line, lineErr := c.reader.ReadString('\n')
			if lineErr != nil {
				return nil, fmt.Errorf("rtsp read header: %w", lineErr)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			key, value, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}

		response := &Response{Proto: proto, Code: code, Message: message, Headers: headers}

		if raw, ok := headers["content-length"]; ok {
			length, convErr := strconv.Atoi(raw)
			if convErr == nil && length > 0 {
				body := make([]byte, length)
				if _, err := io.ReadFull(c.reader, body); err != nil {
					return nil, fmt.Errorf("rtsp read body: %w", err)
				}
				response.Body = body
			}
		}

		// RTSP responses can be interleaved with server-initiated requests
		// (for example an Apple receiver's /command channel). Only the reply
		// matching our CSeq is the answer we are waiting for.
		responseCSeq, convErr := strconv.Atoi(headers["cseq"])
		if convErr == nil && responseCSeq != expectedCSeq {
			continue
		}
		return response, nil
	}
}

// Close shuts the RTSP connection down.
func (c *rtspConn) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func randomUint32() (uint32, error) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

func randomHex(bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 0, bytesLen*2)
	for _, b := range buf {
		out = append(out, digits[b>>4], digits[b&0x0F])
	}
	return string(out), nil
}
