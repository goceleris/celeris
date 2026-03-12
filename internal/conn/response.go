package conn

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/protocol/h2/frame"
	"github.com/goceleris/celeris/protocol/h2/stream"

	"golang.org/x/net/http2"
)

// cachedDateValue holds the pre-formatted HTTP Date header line, updated every second.
var cachedDateValue atomic.Value

func init() {
	updateCachedDate()
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		for range ticker.C {
			updateCachedDate()
		}
	}()
}

func updateCachedDate() {
	now := time.Now().UTC()
	var buf [64]byte
	b := buf[:0]
	b = append(b, "date: "...)
	b = now.AppendFormat(b, time.RFC1123)
	b = append(b, "\r\n"...)
	cp := make([]byte, len(b))
	copy(cp, b)
	cachedDateValue.Store(cp)
}

func appendCachedDate(buf []byte) []byte {
	if v := cachedDateValue.Load(); v != nil {
		return append(buf, v.([]byte)...)
	}
	buf = append(buf, "date: "...)
	buf = time.Now().UTC().AppendFormat(buf, time.RFC1123)
	return append(buf, crlf...)
}

var (
	statusLine200 = []byte("HTTP/1.1 200 OK\r\n")
	statusLine201 = []byte("HTTP/1.1 201 Created\r\n")
	statusLine204 = []byte("HTTP/1.1 204 No Content\r\n")
	statusLine301 = []byte("HTTP/1.1 301 Moved Permanently\r\n")
	statusLine302 = []byte("HTTP/1.1 302 Found\r\n")
	statusLine304 = []byte("HTTP/1.1 304 Not Modified\r\n")
	statusLine400 = []byte("HTTP/1.1 400 Bad Request\r\n")
	statusLine401 = []byte("HTTP/1.1 401 Unauthorized\r\n")
	statusLine403 = []byte("HTTP/1.1 403 Forbidden\r\n")
	statusLine404 = []byte("HTTP/1.1 404 Not Found\r\n")
	statusLine500 = []byte("HTTP/1.1 500 Internal Server Error\r\n")
	statusLine502 = []byte("HTTP/1.1 502 Bad Gateway\r\n")
	statusLine503 = []byte("HTTP/1.1 503 Service Unavailable\r\n")

	crlf = []byte("\r\n")
)

var responseBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 32768)
		return &b
	},
}

func getResponseBuffer() *[]byte {
	return responseBufferPool.Get().(*[]byte)
}

func putResponseBuffer(p *[]byte) {
	const maxPoolBufferCap = 128 << 10 // 128 KB
	if cap(*p) > maxPoolBufferCap {
		return // let GC reclaim; pool will allocate fresh 32KB
	}
	*p = (*p)[:0]
	responseBufferPool.Put(p)
}

type h1ResponseAdapter struct {
	write     func([]byte)
	keepAlive bool
	isHEAD    bool
	hijackFn  func() (net.Conn, error)
	hijacked  bool
}

func (a *h1ResponseAdapter) Hijack(_ *stream.Stream) (net.Conn, error) {
	if a.hijackFn == nil {
		return nil, stream.ErrHijackNotSupported
	}
	conn, err := a.hijackFn()
	if err == nil {
		a.hijacked = true
	}
	return conn, err
}

var _ stream.Hijacker = (*h1ResponseAdapter)(nil)

func (a *h1ResponseAdapter) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
	pooled := getResponseBuffer()
	buf := (*pooled)[:0]

	buf = appendStatusLine(buf, status)

	buf = appendCachedDate(buf)

	hasContentLength := false
	for _, h := range headers {
		if h[0] == "content-length" {
			hasContentLength = true
			break
		}
	}
	if !hasContentLength && len(body) > 0 {
		buf = append(buf, "content-length: "...)
		buf = strconv.AppendInt(buf, int64(len(body)), 10)
		buf = append(buf, crlf...)
	}

	for _, h := range headers {
		buf = appendSanitizedHeaderField(buf, h[0])
		buf = append(buf, ": "...)
		buf = appendSanitizedHeaderField(buf, h[1])
		buf = append(buf, crlf...)
	}

	if !a.keepAlive {
		buf = append(buf, "connection: close\r\n"...)
	}

	buf = append(buf, crlf...)

	// RFC 9110 §9.3.2: HEAD responses MUST NOT contain a message body.
	// Content-Length is still included above to indicate the size that
	// would be returned for a GET, but no bytes are sent.
	if len(body) > 0 && !a.isHEAD {
		buf = append(buf, body...)
	}

	a.write(buf)
	*pooled = buf
	putResponseBuffer(pooled)
	return nil
}

func (a *h1ResponseAdapter) SendGoAway(_ uint32, _ http2.ErrCode, _ []byte) error {
	return nil
}

func (a *h1ResponseAdapter) MarkStreamClosed(_ uint32) {}

func (a *h1ResponseAdapter) IsStreamClosed(_ uint32) bool {
	return false
}

func (a *h1ResponseAdapter) WriteRSTStreamPriority(_ uint32, _ http2.ErrCode) error {
	return nil
}

func (a *h1ResponseAdapter) CloseConn() error {
	return nil
}

type h2ResponseAdapter struct {
	write   func([]byte)
	outBuf  *bytes.Buffer
	writer  *frame.Writer
	encoder *frame.HeaderEncoder
	mu      sync.Mutex
}

func (a *h2ResponseAdapter) WriteResponse(s *stream.Stream, status int, headers [][2]string, body []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// RFC 9110 §9.3.2: HEAD responses MUST NOT contain a message body.
	if len(body) > 0 && s.IsHEAD {
		body = nil
	}

	// Stack-allocated header buffer for common case (≤7 response headers).
	var buf [8][2]string
	responseHeaders := buf[:0:8]
	if len(headers)+1 > 8 {
		responseHeaders = make([][2]string, 0, len(headers)+1)
	}
	responseHeaders = append(responseHeaders, [2]string{":status", statusCodeString(status)})
	responseHeaders = append(responseHeaders, headers...)

	headerBlock, err := a.encoder.Encode(responseHeaders)
	if err != nil {
		return fmt.Errorf("HPACK encode error: %w", err)
	}

	endStream := len(body) == 0
	if err := a.writer.WriteHeaders(s.ID, endStream, headerBlock, 16384); err != nil {
		return err
	}

	if len(body) > 0 {
		window := s.GetWindowSize()

		if window <= 0 {
			// No window available — buffer everything.
			s.BufferOutbound(body, true)
		} else {
			sendLen := len(body)
			if int32(sendLen) > window {
				sendLen = int(window)
			}
			isEnd := sendLen == len(body)
			if err := a.writer.WriteData(s.ID, isEnd, body[:sendLen]); err != nil {
				return err
			}
			s.DeductWindow(int32(sendLen))

			if !isEnd {
				// Buffer remaining data.
				s.BufferOutbound(body[sendLen:], true)
			}
		}
	}

	if err := a.writer.Flush(); err != nil {
		return err
	}

	s.HeadersSent = true
	return nil
}

func (a *h2ResponseAdapter) SendGoAway(lastStreamID uint32, code http2.ErrCode, debug []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writer.WriteGoAway(lastStreamID, code, debug); err != nil {
		return err
	}
	return a.writer.Flush()
}

func (a *h2ResponseAdapter) MarkStreamClosed(_ uint32) {}

func (a *h2ResponseAdapter) IsStreamClosed(_ uint32) bool {
	return false
}

func (a *h2ResponseAdapter) WriteRSTStreamPriority(streamID uint32, code http2.ErrCode) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writer.WriteRSTStream(streamID, code); err != nil {
		return err
	}
	return a.writer.Flush()
}

func (a *h2ResponseAdapter) CloseConn() error {
	return nil
}

func appendStatusLine(buf []byte, status int) []byte {
	switch status {
	case 200:
		return append(buf, statusLine200...)
	case 201:
		return append(buf, statusLine201...)
	case 204:
		return append(buf, statusLine204...)
	case 301:
		return append(buf, statusLine301...)
	case 302:
		return append(buf, statusLine302...)
	case 304:
		return append(buf, statusLine304...)
	case 400:
		return append(buf, statusLine400...)
	case 401:
		return append(buf, statusLine401...)
	case 403:
		return append(buf, statusLine403...)
	case 404:
		return append(buf, statusLine404...)
	case 500:
		return append(buf, statusLine500...)
	case 502:
		return append(buf, statusLine502...)
	case 503:
		return append(buf, statusLine503...)
	default:
		buf = append(buf, "HTTP/1.1 "...)
		buf = strconv.AppendInt(buf, int64(status), 10)
		buf = append(buf, ' ')
		buf = append(buf, statusText(status)...)
		return append(buf, crlf...)
	}
}

func statusCodeString(code int) string {
	switch code {
	case 200:
		return "200"
	case 201:
		return "201"
	case 204:
		return "204"
	case 301:
		return "301"
	case 302:
		return "302"
	case 304:
		return "304"
	case 400:
		return "400"
	case 401:
		return "401"
	case 403:
		return "403"
	case 404:
		return "404"
	case 500:
		return "500"
	case 502:
		return "502"
	case 503:
		return "503"
	default:
		return strconv.Itoa(code)
	}
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 301:
		return "Moved Permanently"
	case 302:
		return "Found"
	case 304:
		return "Not Modified"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 500:
		return "Internal Server Error"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	default:
		return "Unknown"
	}
}

// appendSanitizedHeaderField appends s to buf, stripping any \r or \n bytes
// to prevent HTTP response splitting (CWE-113). This is a defense-in-depth
// measure; the public API (Context.SetHeader) also strips CRLF.
// h1 streaming support — h1ResponseAdapter implements stream.Streamer.

func (a *h1ResponseAdapter) WriteHeader(_ *stream.Stream, status int, headers [][2]string) error {
	pooled := getResponseBuffer()
	buf := (*pooled)[:0]
	buf = appendStatusLine(buf, status)
	buf = append(buf, "date: "...)
	buf = time.Now().UTC().AppendFormat(buf, time.RFC1123)
	buf = append(buf, crlf...)
	buf = append(buf, "transfer-encoding: chunked\r\n"...)
	for _, h := range headers {
		buf = appendSanitizedHeaderField(buf, h[0])
		buf = append(buf, ": "...)
		buf = appendSanitizedHeaderField(buf, h[1])
		buf = append(buf, crlf...)
	}
	if !a.keepAlive {
		buf = append(buf, "connection: close\r\n"...)
	}
	buf = append(buf, crlf...)
	a.write(buf)
	*pooled = buf
	putResponseBuffer(pooled)
	return nil
}

func (a *h1ResponseAdapter) Write(_ *stream.Stream, data []byte) error {
	// Chunked transfer encoding: hex(len)\r\n data \r\n
	chunk := fmt.Appendf(nil, "%x\r\n", len(data))
	chunk = append(chunk, data...)
	chunk = append(chunk, crlf...)
	a.write(chunk)
	return nil
}

func (a *h1ResponseAdapter) Flush(_ *stream.Stream) error {
	return nil // write() is synchronous
}

func (a *h1ResponseAdapter) Close(_ *stream.Stream) error {
	a.write([]byte("0\r\n\r\n"))
	return nil
}

// h2 streaming support — h2ResponseAdapter implements stream.Streamer.

func (a *h2ResponseAdapter) WriteHeader(s *stream.Stream, status int, headers [][2]string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	responseHeaders := make([][2]string, 0, len(headers)+1)
	responseHeaders = append(responseHeaders, [2]string{":status", strconv.Itoa(status)})
	responseHeaders = append(responseHeaders, headers...)

	headerBlock, err := a.encoder.Encode(responseHeaders)
	if err != nil {
		return fmt.Errorf("HPACK encode error: %w", err)
	}

	// endStream=false — body follows
	if err := a.writer.WriteHeaders(s.ID, false, headerBlock, 16384); err != nil {
		return err
	}
	s.HeadersSent = true
	return a.writer.Flush()
}

func (a *h2ResponseAdapter) Write(s *stream.Stream, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writer.WriteData(s.ID, false, data)
}

func (a *h2ResponseAdapter) Flush(_ *stream.Stream) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writer.Flush()
}

func (a *h2ResponseAdapter) Close(s *stream.Stream) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writer.WriteData(s.ID, true, nil); err != nil {
		return err
	}
	return a.writer.Flush()
}

var _ stream.Streamer = (*h1ResponseAdapter)(nil)
var _ stream.Streamer = (*h2ResponseAdapter)(nil)

func appendSanitizedHeaderField(buf []byte, s string) []byte {
	for i := range len(s) {
		b := s[i]
		if b != '\r' && b != '\n' {
			buf = append(buf, b)
		}
	}
	return buf
}

func buildErrorResponse(status int, message string) []byte {
	body := []byte(message)
	pooled := getResponseBuffer()
	buf := (*pooled)[:0]
	buf = appendStatusLine(buf, status)
	buf = append(buf, "content-type: text/plain; charset=utf-8\r\n"...)
	buf = append(buf, "content-length: "...)
	buf = strconv.AppendInt(buf, int64(len(body)), 10)
	buf = append(buf, crlf...)
	buf = append(buf, "connection: close\r\n"...)
	buf = append(buf, crlf...)
	buf = append(buf, body...)
	result := make([]byte, len(buf))
	copy(result, buf)
	*pooled = buf
	putResponseBuffer(pooled)
	return result
}
