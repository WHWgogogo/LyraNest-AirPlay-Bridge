package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// rtspRequest is a parsed RTSP/1.0 request.
type rtspRequest struct {
	Method  string
	URI     string
	Proto   string
	Headers map[string]string
	Body    []byte
}

// rtspResponse is a reply to be serialised onto the control connection.
type rtspResponse struct {
	Code            int
	Message         string
	Headers         map[string]string
	Body            []byte
	closeConnection bool
}

// requestReader decodes RTSP requests, which share HTTP's framing: a request
// line, headers, a blank line, then Content-Length bytes of body.
type requestReader struct {
	conn   net.Conn
	reader *bufio.Reader
}

func newRequestReader(conn net.Conn) *requestReader {
	return &requestReader{conn: conn, reader: bufio.NewReaderSize(conn, 16*1024)}
}

func (r *requestReader) read() (*rtspRequest, error) {
	if err := r.setDeadline(); err != nil {
		return nil, err
	}

	requestLine, err := r.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	requestLine = strings.TrimRight(requestLine, "\r\n")
	for requestLine == "" {
		if requestLine, err = r.reader.ReadString('\n'); err != nil {
			return nil, err
		}
		requestLine = strings.TrimRight(requestLine, "\r\n")
	}

	parts := strings.SplitN(requestLine, " ", 3)
	if len(parts) < 3 {
		return nil, fmt.Errorf("malformed request line %q", requestLine)
	}

	request := &rtspRequest{
		Method:  strings.ToUpper(parts[0]),
		URI:     parts[1],
		Proto:   parts[2],
		Headers: map[string]string{},
	}

	for {
		if err := r.setDeadline(); err != nil {
			return nil, err
		}
		line, lineErr := r.reader.ReadString('\n')
		if lineErr != nil {
			return nil, lineErr
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		request.Headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}

	if raw, ok := request.Headers["content-length"]; ok {
		length, convErr := strconv.Atoi(raw)
		if convErr == nil && length > 0 {
			body := make([]byte, length)
			if err := r.setDeadline(); err != nil {
				return nil, err
			}
			if _, err := io.ReadFull(r.reader, body); err != nil {
				return nil, err
			}
			request.Body = body
		}
	}
	return request, nil
}

func (r *requestReader) setDeadline() error {
	if r.conn == nil {
		return nil
	}
	return r.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
}

func writeResponse(w io.Writer, response *rtspResponse) error {
	var builder strings.Builder
	fmt.Fprintf(&builder, "RTSP/1.0 %d %s\r\n", response.Code, response.Message)
	if _, ok := response.Headers["Server"]; !ok {
		builder.WriteString("Server: AirTunes/130.14\r\n")
	}
	for key, value := range response.Headers {
		fmt.Fprintf(&builder, "%s: %s\r\n", key, value)
	}
	if len(response.Body) > 0 {
		fmt.Fprintf(&builder, "Content-Length: %d\r\n", len(response.Body))
	}
	builder.WriteString("\r\n")

	payload := make([]byte, 0, builder.Len()+len(response.Body))
	payload = append(payload, builder.String()...)
	payload = append(payload, response.Body...)
	_, err := w.Write(payload)
	return err
}

// parseTransportOptions splits an RTSP Transport header into its key=value
// parameters.
func parseTransportOptions(transport string) map[string]string {
	options := map[string]string{}
	for _, part := range strings.Split(transport, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		options[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return options
}
