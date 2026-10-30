package conn

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/internal/protocol/h2/frame"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"

	"golang.org/x/net/http2"
)

// cachedDatePtr holds the pre-formatted HTTP Date header line, updated every second.
// Using atomic.Pointer avoids the type assertion cost of atomic.Value (P6).
var cachedDatePtr atomic.Pointer[[]byte]

// cachedStatus200DatePtr holds "HTTP/1.1 200 OK\r\ndate: <RFC1123>\r\n" fused into
// a single cached block, updated every second alongside cachedDatePtr. For 200 OK
// responses (95%+ of API traffic), this replaces two separate appends with one.
var cachedStatus200DatePtr atomic.Pointer[[]byte]

// cachedStatus200DateCT* hold the further-fused
// "HTTP/1.1 200 OK\r\ndate: ...\r\ncontent-type: <ct>\r\n" block for the
// most common content types served by API benchmarks. Lets WriteResponse's
// fast path collapse three appends (status+date, content-type, then
// content-length+body) into two — the cached fused block + content-length.
var (
	cachedStatus200DateCTJSONPtr  atomic.Pointer[[]byte]
	cachedStatus200DateCTPlainPtr atomic.Pointer[[]byte]
	cachedStatus200DateCTHTMLPtr  atomic.Pointer[[]byte]
	cachedStatus200DateCTXMLPtr   atomic.Pointer[[]byte]
	cachedStatus200DateCTOctetPtr atomic.Pointer[[]byte]
)

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
	cachedDatePtr.Store(&cp)

	// Fused "HTTP/1.1 200 OK\r\ndate: ...\r\n" block.
	var buf2 [96]byte
	b2 := buf2[:0]
	b2 = append(b2, statusLine200...)
	b2 = append(b2, cp...)
	cp2 := make([]byte, len(b2))
	copy(cp2, b2)
	cachedStatus200DatePtr.Store(&cp2)

	// Further-fused 200+date+content-type blocks for the common 5
	// content types. One allocation each, swapped atomically every second.
	storeFusedCT := func(target *atomic.Pointer[[]byte], ctLine []byte) {
		var bb [192]byte
		b := bb[:0]
		b = append(b, cp2...)
		b = append(b, ctLine...)
		out := make([]byte, len(b))
		copy(out, b)
		target.Store(&out)
	}
	storeFusedCT(&cachedStatus200DateCTJSONPtr, ctJSON)
	storeFusedCT(&cachedStatus200DateCTPlainPtr, ctPlain)
	storeFusedCT(&cachedStatus200DateCTHTMLPtr, ctHTML)
	storeFusedCT(&cachedStatus200DateCTXMLPtr, ctXML)
	storeFusedCT(&cachedStatus200DateCTOctetPtr, ctOctet)
}

func appendCachedDate(buf []byte) []byte {
	if p := cachedDatePtr.Load(); p != nil {
		return append(buf, (*p)...)
	}
	buf = append(buf, "date: "...)
	buf = time.Now().UTC().AppendFormat(buf, time.RFC1123)
	return append(buf, crlf...)
}

// appendCachedStatus200Date appends the fused "HTTP/1.1 200 OK\r\ndate: ...\r\n"
// block. Falls back to two separate appends if the cache isn't ready.
func appendCachedStatus200Date(buf []byte) []byte {
	if p := cachedStatus200DatePtr.Load(); p != nil {
		return append(buf, (*p)...)
	}
	buf = appendStatusLine(buf, 200)
	return appendCachedDate(buf)
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

	// Pre-built content-type header lines for common MIME types.
	// Avoids per-byte append and sanitization on the hot path.
	ctJSON  = []byte("content-type: application/json\r\n")
	ctPlain = []byte("content-type: text/plain\r\n")
	ctHTML  = []byte("content-type: text/html; charset=utf-8\r\n")
	ctXML   = []byte("content-type: application/xml\r\n")
	ctOctet = []byte("content-type: application/octet-stream\r\n")

	// Pre-built header name prefix for content-length.
	clPrefix = []byte("content-length: ")
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
	writeBody func([]byte) // optional scatter-gather body writer (zero-copy body)
	// sendFile, when non-nil, drives a zero-copy sendfile(2) response on
	// the owning engine. It receives the pre-built response header block
	// (status line + date + headers + framing, ending in the blank CRLF)
	// and the file slice to transfer. The engine takes responsibility for
	// the file slice from the call onward (it dups the descriptor), so the
	// caller may close its own *os.File once WriteFileResponse returns.
	// Installed by sendfile-capable engines (epoll); nil elsewhere, which
	// makes WriteFileResponse decline (handled=false) so the caller falls
	// back to the buffered path. Only set in non-async mode — the async
	// dispatch goroutine would race the worker on the sendfile state.
	sendFile func(header []byte, file *os.File, offset, length int64) error
	// writeErr, when non-nil, is the engine's write-error query
	// (H1State.SetWriteErrQuery): a streaming Write asks it with the size of
	// the chunk before it stages it, and Write, Flush and Close ask it after
	// the write hook, which cannot report an error itself. werr latches what it
	// returned for the rest of the response (the engine closes a connection it
	// reports on, so it never outlives one); WriteHeader starts a response and
	// clears it. Only the streaming methods touch either: WriteResponse and
	// the other whole-response paths are not affected. Like the rest of the
	// adapter they are not safe for concurrent use: calls on one StreamWriter
	// must be serialised.
	writeErr  WriteErrQuery
	werr      error
	keepAlive bool
	isHEAD    bool
	// shutdown is H1State.Shutdown, wired once with the write hook: a final
	// response (status 200 and up) written while it is up says
	// Connection: close and clears keepAlive, which handleH1Request turns into
	// the end of the connection (see closing).
	shutdown *atomic.Bool
	// A streamed response (WriteHeader, Write, Close) keeps its own state, set
	// by WriteHeader and cleared for each request (handleH1Request), so that
	// what a later chunk does depends on the request that began the response
	// and not on whatever request the adapter holds by then (celeris#889).
	// Like the rest of the adapter, not safe for concurrent use; the handler
	// that detached owns it, and handleH1Request reads it only for a response
	// the handler did not detach.
	//
	// streamBegun: WriteHeader has begun a response, and streamNoBody says it has
	// none: a HEAD's, or one with a status that has no content (headers only, no
	// chunk, no last chunk). streamOpen: a chunked
	// response is under way and its last chunk has not been sent (a HEAD's, and
	// one with no content, 1xx, 204 and 304, never is). streamBroken: the
	// handler failed after its headers went out.
	streamBegun  bool
	streamNoBody bool
	streamOpen   bool
	streamBroken bool
	// streamInterim: the last WriteHeader was a 1xx, so no final response has
	// been sent (handleH1Request answers one when the handler returns).
	streamInterim bool
	// streamDeclared is the content-length WriteHeader was given (-1: none,
	// declaredInvalid: one that cannot be held to) and streamWritten the body
	// bytes Write has staged: a response its handler leaves unclosed is ended
	// only if they agree.
	streamDeclared int64
	streamWritten  int64
	// chunkBuf is the scratch a streamed chunk is framed in. Every write hook
	// copies its argument before it returns (the engines append it to their
	// send buffer), so it is reused for the next chunk; it never grows past
	// maxChunkScratch, a larger chunk is framed in a buffer of its own. Once
	// the stream is detached (UpdateWriteFn), a long-lived writer keeps at most
	// chunkScratchKeep (scratchDetached).
	chunkBuf        []byte
	scratchDetached bool
	hijackFn        func() (net.Conn, error)
	hijacked        bool
	respBuf         []byte // per-connection reusable response buffer, avoids sync.Pool per request
}

// sendfileThreshold is the minimum body size at which the H1 response
// path prefers sendfile(2) over a buffered write. Below ~16 KiB a
// buffered write coalesces with the header block into a single send and
// avoids the extra sendfile syscall (and the loss of header+body fusion);
// the zero-copy win only materialises for larger bodies. Static-file
// benchmarks and the kernel's own small-send copy threshold both put the
// crossover in this neighbourhood.
const sendfileThreshold = 16 << 10

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
var _ stream.FileResponder = (*h1ResponseAdapter)(nil)

// mustFrameEmptyBody reports whether an HTTP/1.1 response with no body
// requires explicit framing (Content-Length: 0 or Transfer-Encoding) on
// a keep-alive connection.
//
// RFC 9110 §15 + RFC 9112 §6.2:
//
//   - 1xx (Informational) and 204 (No Content) MUST NOT include
//     Content-Length, and have no body — keep-alive parsers know they
//     finish at the trailing CRLF, no framing required.
//   - 304 (Not Modified) — Content-Length is optional, and parsers
//     never expect a body. We omit it for symmetry with 1xx/204.
//   - Every other status (2xx ≠ 204, 3xx, 4xx, 5xx) with an empty body
//     MUST advertise the absence to the keep-alive client; otherwise
//     the client waits for body bytes forever — exactly the hang
//     observed by the probatorium walker against iouring/epoll when
//     it received a 301 redirect from c.Redirect / c.NoContent.
func mustFrameEmptyBody(status int) bool {
	switch {
	case status >= 100 && status < 200:
		return false
	case status == 204, status == 304:
		return false
	default:
		return true
	}
}

// closing reports whether the response being written with status ends the
// connection: the request said so (keepAlive already false), or the engine's
// shutdown has begun (celeris#884, celeris#885): every final response written
// from then on says Connection: close and the connection ends after it, as the
// engines' gate does for a request it meets (H1State.CloseAfter). It is read
// when the head is built, so a handler that was already running when Shutdown
// began (a sync handler holding its loop, an Async-route handler on its
// goroutine) is covered, not just a request read after it. An interim response
// (1xx, which includes 101 Switching Protocols of an upgrade) is not final: it
// neither says close nor ends the connection. One nil test and, with the flag
// wired, one atomic load of a word that is written once, per response.
func (a *h1ResponseAdapter) closing(status int) bool {
	if !a.keepAlive {
		return true
	}
	if sd := a.shutdown; sd != nil && status >= 200 && sd.Load() {
		a.keepAlive = false
		return true
	}
	return false
}

// withoutConnectionHeaders returns headers with every Connection entry removed,
// for a response that ends the connection: it carries the one
// "connection: close" the head builders append, as net/http's response written
// after Shutdown began carries one (a handler's "Connection: keep-alive" is
// replaced, its "Connection: close" is not repeated). The common case, no such
// header, returns headers itself and allocates nothing. Cold: only a closing
// response asks.
func withoutConnectionHeaders(headers [][2]string) [][2]string {
	n := 0
	for _, h := range headers {
		if len(h[0]) == len("connection") && strings.EqualFold(h[0], "connection") {
			n++
		}
	}
	if n == 0 {
		return headers
	}
	out := make([][2]string, 0, len(headers)-n)
	for _, h := range headers {
		if len(h[0]) == len("connection") && strings.EqualFold(h[0], "connection") {
			continue
		}
		out = append(out, h)
	}
	return out
}

func (a *h1ResponseAdapter) WriteResponse(_ *stream.Stream, status int, headers [][2]string, body []byte) error {
	// Reuse per-connection buffer. This eliminates sync.Pool Get/Put
	// per response (~40ns). The buffer grows as needed and persists
	// across requests on the same keep-alive connection.
	buf := a.respBuf[:0]
	closing := a.closing(status)
	if closing && status >= 200 { // an interim head's Connection (101: Upgrade) is the handler's
		headers = withoutConnectionHeaders(headers)
	}

	// Ultra-fast path: 200 + 2 standard headers (content-type, content-length)
	// where the content-type is one of the cached common types — collapse
	// status line + date + content-type into a single fused append. Falls
	// through to the per-step path on cache miss or non-200 / unusual
	// shapes.
	if status == 200 && len(headers) == 2 && headers[1][0] == "content-length" {
		var fused *[]byte
		switch headers[0][1] {
		case "application/json":
			fused = cachedStatus200DateCTJSONPtr.Load()
		case "text/plain":
			fused = cachedStatus200DateCTPlainPtr.Load()
		case "text/html; charset=utf-8":
			fused = cachedStatus200DateCTHTMLPtr.Load()
		case "application/xml":
			fused = cachedStatus200DateCTXMLPtr.Load()
		case "application/octet-stream":
			fused = cachedStatus200DateCTOctetPtr.Load()
		}
		if fused != nil {
			buf = append(buf, (*fused)...)
			buf = append(buf, clPrefix...)
			buf = append(buf, headers[1][1]...)
			buf = append(buf, crlf...)
			if closing {
				buf = append(buf, "connection: close\r\n"...)
			}
			buf = append(buf, crlf...)
			if len(body) > 0 && !a.isHEAD {
				if len(body) >= 8192 && a.writeBody != nil {
					a.write(buf)
					a.respBuf = buf
					a.writeBody(body)
					return nil
				}
				a.write(buf)
				a.respBuf = buf
				a.write(body)
				return nil
			}
			a.write(buf)
			a.respBuf = buf
			return nil
		}
	}

	// Fast path: status 200 uses fused status+date block (one append).
	if status == 200 {
		buf = appendCachedStatus200Date(buf)
	} else {
		buf = appendStatusLine(buf, status)
		buf = appendCachedDate(buf)
	}

	// Fast path: exactly 2 headers from Blob() (content-type, content-length).
	// This is the dominant case for API responses. Skip per-header string
	// comparisons and the hasContentLength flag entirely.
	// Guard: verify the second header is actually content-length to avoid
	// misframing responses from callers that pass non-CL 2-header slices.
	if len(headers) == 2 && headers[1][0] == "content-length" {
		// First header: content-type (from Blob).
		switch headers[0][1] {
		case "application/json":
			buf = append(buf, ctJSON...)
		case "text/plain":
			buf = append(buf, ctPlain...)
		case "text/html; charset=utf-8":
			buf = append(buf, ctHTML...)
		case "application/xml":
			buf = append(buf, ctXML...)
		case "application/octet-stream":
			buf = append(buf, ctOctet...)
		default:
			buf = append(buf, headers[0][0]...)
			buf = append(buf, ": "...)
			buf = append(buf, headers[0][1]...)
			buf = append(buf, crlf...)
		}
		// Second header: content-length (from Blob).
		buf = append(buf, clPrefix...)
		buf = append(buf, headers[1][1]...)
		buf = append(buf, crlf...)
	} else {
		// General path: merged loop checks for content-length while appending.
		hasContentLength := false
		for _, h := range headers {
			if h[0] == "content-length" {
				hasContentLength = true
			}
			if h[0] == "content-type" {
				switch h[1] {
				case "application/json":
					buf = append(buf, ctJSON...)
					continue
				case "text/plain":
					buf = append(buf, ctPlain...)
					continue
				case "text/html; charset=utf-8":
					buf = append(buf, ctHTML...)
					continue
				case "application/xml":
					buf = append(buf, ctXML...)
					continue
				case "application/octet-stream":
					buf = append(buf, ctOctet...)
					continue
				}
			}
			// Headers here are pre-validated CRLF-free: SetHeader/AddHeader
			// sanitize at set time; SetHeaderTrust/AppendRespHeader push
			// that invariant onto the caller. Skipping the per-header
			// scan is worth ~2.24% CPU on the realistic-API hot path.
			buf = append(buf, h[0]...)
			buf = append(buf, ": "...)
			buf = append(buf, h[1]...)
			buf = append(buf, crlf...)
		}
		if !hasContentLength && (len(body) > 0 || mustFrameEmptyBody(status)) {
			buf = append(buf, clPrefix...)
			buf = strconv.AppendInt(buf, int64(len(body)), 10)
			buf = append(buf, crlf...)
		}
	}

	if closing {
		buf = append(buf, "connection: close\r\n"...)
	}

	buf = append(buf, crlf...)

	// RFC 9110 §9.3.2: HEAD responses MUST NOT contain a message body.
	// Content-Length is still included above to indicate the size that
	// would be returned for a GET, but no bytes are sent.
	if len(body) > 0 && !a.isHEAD {
		// Scatter-gather for large bodies (epoll's writev(2)): ships headers +
		// body in one syscall without copying body bytes.
		if len(body) >= 8192 && a.writeBody != nil {
			a.write(buf)
			a.respBuf = buf
			a.writeBody(body)
			return nil
		}
		// Two-write path for small bodies: writeFn just appends to
		// cs.writeBuf, so two calls land headers and body back-to-back
		// without the second body memcpy a single-write would force.
		a.write(buf)
		a.respBuf = buf
		a.write(body)
		return nil
	}

	a.write(buf)
	a.respBuf = buf
	return nil
}

// appendFileResponseHead builds the H1 response header block for a
// sendfile body: status line + date, the caller's headers (with cached
// content-type lines reused), an injected content-length of bodyLen
// unless the caller already supplied one, the connection-close line when
// not keep-alive, and the terminating blank CRLF. It mirrors the general
// (non-fast) path of WriteResponse so a sendfile response is byte-for-byte
// equivalent to the buffered response that would otherwise be emitted.
func (a *h1ResponseAdapter) appendFileResponseHead(buf []byte, status int, headers [][2]string, bodyLen int64) []byte {
	closing := a.closing(status)
	if closing && status >= 200 { // an interim head's Connection (101: Upgrade) is the handler's
		headers = withoutConnectionHeaders(headers)
	}
	if status == 200 {
		buf = appendCachedStatus200Date(buf)
	} else {
		buf = appendStatusLine(buf, status)
		buf = appendCachedDate(buf)
	}
	hasContentLength := false
	for _, h := range headers {
		if h[0] == "content-length" {
			hasContentLength = true
		}
		if h[0] == "content-type" {
			switch h[1] {
			case "application/json":
				buf = append(buf, ctJSON...)
				continue
			case "text/plain":
				buf = append(buf, ctPlain...)
				continue
			case "text/html; charset=utf-8":
				buf = append(buf, ctHTML...)
				continue
			case "application/xml":
				buf = append(buf, ctXML...)
				continue
			case "application/octet-stream":
				buf = append(buf, ctOctet...)
				continue
			}
		}
		// Headers reach this path pre-validated CRLF-free (same invariant
		// as WriteResponse — Context.Set* sanitize at set time).
		buf = append(buf, h[0]...)
		buf = append(buf, ": "...)
		buf = append(buf, h[1]...)
		buf = append(buf, crlf...)
	}
	if !hasContentLength {
		buf = append(buf, clPrefix...)
		buf = strconv.AppendInt(buf, bodyLen, 10)
		buf = append(buf, crlf...)
	}
	if closing {
		buf = append(buf, "connection: close\r\n"...)
	}
	return append(buf, crlf...)
}

// WriteFileResponse implements stream.FileResponder. It serves [offset,
// offset+length) of file as the response body via the engine's zero-copy
// sendfile(2) path. Returns handled=false (emitting nothing) when the
// zero-copy path is unavailable or unwanted — no sendfile hook installed
// (engine not sendfile-capable, or async mode), or the body is below
// sendfileThreshold — so the caller falls back to the buffered path.
//
// The HEAD-request invariant is enforced here: for HEAD we emit the
// header block (with the correct content-length) and NO body bytes, and
// we never invoke the sendfile hook.
func (a *h1ResponseAdapter) WriteFileResponse(_ *stream.Stream, status int, headers [][2]string, file *os.File, offset, length int64) (bool, error) {
	if a.sendFile == nil {
		return false, nil
	}
	// HEAD: headers only, no body, no sendfile. Build + write the header
	// block (content-length reflects the would-be GET body size) and return
	// handled — the caller must not also write a body.
	if a.isHEAD {
		buf := a.respBuf[:0]
		buf = a.appendFileResponseHead(buf, status, headers, length)
		a.write(buf)
		a.respBuf = buf
		return true, nil
	}
	if length < sendfileThreshold {
		// Small body: a buffered write coalesces header+body better than a
		// separate sendfile syscall. Decline so the caller uses read+write.
		return false, nil
	}
	// Build the header block into a fresh buffer (NOT a.respBuf — the
	// engine keeps the header slice referenced until the sendfile drains,
	// and a.respBuf is reused by the next response on this conn).
	header := a.appendFileResponseHead(nil, status, headers, length)
	if err := a.sendFile(header, file, offset, length); err != nil {
		return true, err
	}
	return true, nil
}

// h2ReserveSend atomically reserves AND debits up to n bytes of send credit for
// stream s across BOTH the per-stream and the connection-level send windows
// (RFC 7540 §6.9), returning how many bytes may be sent now (the caller MUST
// send exactly that many and buffer the rest). A return ≤ 0 means the stream
// must stall because at least one window is exhausted. Reserving+debiting in
// one atomic step (vs clamp-then-debit against a stale window) is what prevents
// the event loop and a worker goroutine from over-subscribing a shared window.
// When mgr is nil (unit-test adapters constructed without a manager) only the
// per-stream window applies, preserving the single-window behaviour.
func h2ReserveSend(mgr *stream.Manager, s *stream.Stream, n int) int {
	if n <= 0 {
		return 0
	}
	// A request over MaxInt32 is asked for as MaxInt32: int32(n) wraps to
	// zero or a negative number, which the windows refuse, and a loop that
	// waits for them to open would spin on a Write of 2 GiB or more.
	n = min(n, math.MaxInt32)
	if mgr != nil {
		return int(mgr.ReserveSendWindow(s, int32(n)))
	}
	win := s.GetWindowSize()
	if win <= 0 {
		return 0
	}
	if int32(n) > win {
		n = int(win)
	}
	s.DeductWindow(int32(n))
	return n
}

// h2InlineResponseAdapter writes H2 response frames directly to outBuf.
// Used by inline handlers that execute on the event loop thread. Bypasses
// the sharded write queue entirely (no mutex, no eventfd, no goroutine).
// Safe only when called from the event loop under H2State.mu.
type h2InlineResponseAdapter struct {
	outBuf       *bytes.Buffer
	maxFrameSize uint32          // peer's advertised MAX_FRAME_SIZE (updated via Refresh)
	manager      *stream.Manager // peer-SETTINGS-aware source of max frame size
	enc          h2StreamEncoder // per-connection encoder (no sync.Pool needed for inline path)
	// queue is this connection's sharded-queue adapter. Incremental streaming
	// (the Streamer methods: WriteHeader/Write/Flush/Close) MUST delegate to it
	// — a streaming response (SSE, chunked) may be produced by a detached/async
	// goroutine that cannot write outBuf directly (outBuf is event-loop-thread +
	// H2State.mu only). Only WriteResponse — the buffered inline GET/HEAD fast
	// path, always on the event loop — writes straight to outBuf. See celeris#408.
	queue *h2ResponseAdapter
}

// peerMaxFrame returns the peer's currently-advertised SETTINGS_MAX_FRAME_SIZE.
// Falls back to the adapter's default (cfg.MaxFrameSize, capped to 16 KiB when
// the manager is nil, e.g. in unit tests).
func (a *h2InlineResponseAdapter) peerMaxFrame() uint32 {
	if a.manager != nil {
		return a.manager.GetMaxFrameSize()
	}
	if a.maxFrameSize == 0 {
		return 16384
	}
	return a.maxFrameSize
}

func (a *h2InlineResponseAdapter) WriteResponse(s *stream.Stream, status int, headers [][2]string, body []byte) error {
	if len(body) > 0 && s.IsHEAD {
		body = nil
	}

	enc := &a.enc

	// Fast path for common responses: status 200 + known content-type +
	// content-length. Uses pre-encoded HPACK bytes for status and
	// content-type, and manual HPACK encoding for content-length (avoids
	// the full encoder's static table lookup + Huffman decision overhead).
	// Skips the responseHeaders construction below entirely — the fast
	// path never feeds the standard encoder.
	var headerBlock []byte
	if status == 200 && len(headers) == 2 && headers[0][0] == "content-type" && headers[1][0] == "content-length" {
		var ctBlock []byte
		switch headers[0][1] {
		case "text/plain":
			ctBlock = hpackCTTextPlain
		case "application/json":
			ctBlock = hpackCTAppJSON
		}
		if ctBlock != nil {
			enc.hpackBuf.Reset()
			enc.hpackBuf.Write(hpackStatus200)
			enc.hpackBuf.Write(ctBlock)
			appendHPACKContentLength(&enc.hpackBuf, headers[1][1])
			headerBlock = enc.hpackBuf.Bytes()
		}
	}
	if headerBlock == nil {
		// Slow path: construct the response-header slice (status pseudo-
		// header prepended) only when the standard encoder is actually
		// needed.
		var hdrBuf [16][2]string
		responseHeaders := hdrBuf[:0:16]
		if len(headers)+1 > 16 {
			responseHeaders = make([][2]string, 0, len(headers)+1)
		}
		responseHeaders = append(responseHeaders, [2]string{":status", statusCodeString(status)})
		responseHeaders = append(responseHeaders, headers...)
		var err error
		headerBlock, err = enc.encodeHeaders(responseHeaders)
		if err != nil {
			return fmt.Errorf("HPACK encode error: %w", err)
		}
	}

	endStream := len(body) == 0
	maxFrame := a.peerMaxFrame()

	// Write frames directly to outBuf — no intermediate allocation.
	// Fast path: single HEADERS frame (fits in one frame, the common case).
	if uint32(len(headerBlock)) <= maxFrame {
		writeH2FrameHeader(a.outBuf, h2FrameHeaders, endStream, s.ID, len(headerBlock))
		a.outBuf.Write(headerBlock)
	} else {
		a.outBuf.Write(appendH2Headers(nil, s.ID, endStream, headerBlock, maxFrame))
	}

	// Flow control: respect BOTH the per-stream and the connection-level
	// send windows for DATA frames (RFC 7540 §6.9). HEADERS don't consume
	// either window, so they are always sent inline above.
	if len(body) > 0 {
		// Reserve+debit both windows atomically for exactly what we send.
		sendLen := h2ReserveSend(a.manager, s, len(body))
		if sendLen <= 0 {
			s.BufferOutbound(body, true)
		} else {
			isEnd := sendLen == len(body)
			// RFC 7540 §4.2: fragment by peer's MAX_FRAME_SIZE (not our own).
			writeH2DataFragmented(a.outBuf, s.ID, isEnd, body[:sendLen], maxFrame)
			if !isEnd {
				s.BufferOutbound(body[sendLen:], true)
			}
		}
	}

	s.SetHeadersSent()
	return nil
}

func (a *h2InlineResponseAdapter) SendGoAway(_ uint32, _ http2.ErrCode, _ []byte) error {
	return nil // handled by connWriter
}

func (a *h2InlineResponseAdapter) MarkStreamClosed(_ uint32) {}

func (a *h2InlineResponseAdapter) IsStreamClosed(_ uint32) bool { return false }

func (a *h2InlineResponseAdapter) WriteRSTStreamPriority(_ uint32, _ http2.ErrCode) error {
	return nil
}

func (a *h2InlineResponseAdapter) CloseConn() error { return nil }

// h2InlineResponseAdapter implements stream.Streamer by delegating incremental
// streaming to the sharded write queue (a.queue). This is what keeps
// StreamWriter()-based responses (SSE, chunked) working when an inline-eligible
// route streams: Context.StreamWriter() type-asserts ResponseWriter.(Streamer),
// so the inline writer MUST be a Streamer or streaming 500s. The queue path is
// safe from a detached/async streaming goroutine; direct-outBuf (WriteResponse)
// is not. See celeris#408.
func (a *h2InlineResponseAdapter) WriteHeader(s *stream.Stream, status int, headers [][2]string) error {
	return a.queue.WriteHeader(s, status, headers)
}

func (a *h2InlineResponseAdapter) Write(s *stream.Stream, data []byte) error {
	return a.queue.Write(s, data)
}

func (a *h2InlineResponseAdapter) Flush(s *stream.Stream) error {
	return a.queue.Flush(s)
}

func (a *h2InlineResponseAdapter) Close(s *stream.Stream) error {
	return a.queue.Close(s)
}

func (a *h2InlineResponseAdapter) WriteHeaderUse(s *stream.Stream, gen uint64, status int, headers [][2]string) error {
	return a.queue.WriteHeaderUse(s, gen, status, headers)
}

func (a *h2InlineResponseAdapter) WriteUse(s *stream.Stream, gen uint64, data []byte) error {
	return a.queue.WriteUse(s, gen, data)
}

func (a *h2InlineResponseAdapter) CloseUse(s *stream.Stream, gen uint64) error {
	return a.queue.CloseUse(s, gen)
}

// ResetStream queues the RST_STREAM behind the stream's own frames, which an
// inline handler's streamed response sends through the queue (stream.Resetter).
func (a *h2InlineResponseAdapter) ResetStream(s *stream.Stream, code http2.ErrCode) {
	a.queue.ResetStream(s, code)
}

var _ stream.Streamer = (*h2InlineResponseAdapter)(nil)
var _ stream.UseStreamer = (*h2InlineResponseAdapter)(nil)
var _ stream.Resetter = (*h2InlineResponseAdapter)(nil)
var _ stream.Resetter = (*h2ResponseAdapter)(nil)

type h2ResponseAdapter struct {
	write        func([]byte)
	outBuf       *bytes.Buffer
	writer       *frame.Writer
	writeQueue   *h2ShardedQueue // async response queue (sharded)
	maxFrameSize uint32          // our own advertised MAX_FRAME_SIZE (fallback)
	manager      *stream.Manager // peer-SETTINGS-aware source of max frame size
	// writeTimeout bounds how long sendRest waits for the peer's window
	// (H2Config.WriteTimeout; 0 = no bound).
	writeTimeout time.Duration
}

// peerMaxFrame returns the peer's currently-advertised SETTINGS_MAX_FRAME_SIZE,
// falling back to the adapter's default when the manager hasn't been wired yet.
func (a *h2ResponseAdapter) peerMaxFrame() uint32 {
	if a.manager != nil {
		return a.manager.GetMaxFrameSize()
	}
	if a.maxFrameSize == 0 {
		return 16384
	}
	return a.maxFrameSize
}

// h2BeforeEnqueueHook is a test seam: WriteResponse calls it, when it is set,
// after it has reserved the windows and built its frames and just before it
// queues them, so a test can put a WINDOW_UPDATE at the one point where a pool
// handler is exposed to one (celeris#903). Production code never sets it.
var h2BeforeEnqueueHook func(a *h2ResponseAdapter, s *stream.Stream)

// WriteResponse builds complete response frame bytes using a goroutine-local
// HPACK encoder (dynamic table = 0) and enqueues them to the write queue.
// No shared locks are held during encoding — concurrent streams encode independently.
func (a *h2ResponseAdapter) WriteResponse(s *stream.Stream, status int, headers [][2]string, body []byte) error {
	// RFC 9110 §9.3.2: HEAD responses MUST NOT contain a message body.
	if len(body) > 0 && s.IsHEAD {
		body = nil
	}

	// Fast path: status 200 + common content-type + content-length.
	// Uses pre-encoded HPACK bytes (dynamic table = 0, deterministic).
	if status == 200 && len(headers) == 2 && headers[0][0] == "content-type" && headers[1][0] == "content-length" {
		var ctBlock []byte
		switch headers[0][1] {
		case "text/plain":
			ctBlock = hpackCTTextPlain
		case "application/json":
			ctBlock = hpackCTAppJSON
		}
		if ctBlock != nil {
			enc := getH2StreamEncoder()
			enc.hpackBuf.Reset()
			enc.hpackBuf.Write(hpackStatus200)
			enc.hpackBuf.Write(ctBlock)
			appendHPACKContentLength(&enc.hpackBuf, headers[1][1])
			headerBlock := enc.hpackBuf.Bytes()

			endStream := len(body) == 0 || s.IsHEAD
			if s.IsHEAD {
				body = nil
			}
			maxFrame := a.peerMaxFrame()
			// Reserve+debit both windows atomically for exactly what we send.
			sendLen := h2ReserveSend(a.manager, s, len(body))

			pooled := getH2FrameBuf()
			frameBuf := (*pooled)[:0]
			// Room for what goes out now, the HEADERS and the DATA the
			// windows took; not for the whole body (celeris#893).
			estimatedSize := h2FrameBufSize(len(headerBlock), sendLen, maxFrame)
			if cap(frameBuf) < estimatedSize {
				frameBuf = make([]byte, 0, estimatedSize)
			}
			frameBuf = appendH2Headers(frameBuf, s.ID, endStream, headerBlock, maxFrame)
			var rest []byte
			if len(body) > 0 {
				frameBuf, rest = a.stageBody(s, frameBuf, body, sendLen, maxFrame)
			}
			putH2StreamEncoder(enc)
			*pooled = frameBuf
			if h := h2BeforeEnqueueHook; h != nil {
				h(a, s)
			}
			a.writeQueue.Enqueue(s.ID, pooled)
			s.SetHeadersSent()
			if len(rest) > 0 {
				return a.finishBody(s, rest, maxFrame)
			}
			return nil
		}
	}

	enc := getH2StreamEncoder()

	// Stack-allocated header buffer for common case (≤7 response headers).
	var hdrBuf [16][2]string
	responseHeaders := hdrBuf[:0:16]
	if len(headers)+1 > 16 {
		responseHeaders = make([][2]string, 0, len(headers)+1)
	}
	responseHeaders = append(responseHeaders, [2]string{":status", statusCodeString(status)})
	responseHeaders = append(responseHeaders, headers...)

	headerBlock, err := enc.encodeHeaders(responseHeaders)
	if err != nil {
		putH2StreamEncoder(enc)
		return fmt.Errorf("HPACK encode error: %w", err)
	}

	endStream := len(body) == 0
	maxFrame := a.peerMaxFrame()
	// Reserve+debit both windows atomically for exactly what we send.
	sendLen := h2ReserveSend(a.manager, s, len(body))

	// Use pooled frame buffer to eliminate per-response allocation.
	pooled := getH2FrameBuf()
	frameBuf := (*pooled)[:0]

	// Ensure capacity for what goes out now: the HEADERS and the DATA the
	// windows took, not the whole body (celeris#893).
	estimatedSize := h2FrameBufSize(len(headerBlock), sendLen, maxFrame)
	if cap(frameBuf) < estimatedSize {
		frameBuf = make([]byte, 0, estimatedSize)
	}

	frameBuf = appendH2Headers(frameBuf, s.ID, endStream, headerBlock, maxFrame)

	var rest []byte
	if len(body) > 0 {
		frameBuf, rest = a.stageBody(s, frameBuf, body, sendLen, maxFrame)
	}

	putH2StreamEncoder(enc)

	*pooled = frameBuf
	if h := h2BeforeEnqueueHook; h != nil {
		h(a, s)
	}
	a.writeQueue.Enqueue(s.ID, pooled)

	s.SetHeadersSent()
	if len(rest) > 0 {
		return a.finishBody(s, rest, maxFrame)
	}
	return nil
}

// maxH2FrameBufHint bounds the frame-buffer sizes computed up front: a
// buffer past it is left to grow by append, so a size computation cannot
// overflow whatever lengths it is given.
const maxH2FrameBufHint = 1 << 30

// h2DataFramesLen is the size of n bytes of DATA framed by maxFrame, or 0
// (no up-front size) when n or maxFrame is out of range.
func h2DataFramesLen(n int, maxFrame uint32) int {
	if n <= 0 || n > maxH2FrameBufHint || maxFrame == 0 || maxFrame > maxH2FrameBufHint {
		return 0
	}
	return n + 9*((n-1)/int(maxFrame)+1)
}

// h2FrameBufSize is the capacity for a HEADERS block of headerLen bytes and
// dataLen bytes of DATA framed by maxFrame: what goes out now, not the whole
// body (celeris#893). 0 (no up-front size) when a length is out of range.
func h2FrameBufSize(headerLen, dataLen int, maxFrame uint32) int {
	if headerLen < 0 || headerLen > maxH2FrameBufHint {
		return 0
	}
	d := h2DataFramesLen(dataLen, maxFrame)
	if dataLen > 0 && d == 0 {
		return 0
	}
	return 9 + headerLen + d
}

// stageBody appends to frameBuf the sendLen bytes of DATA the flow-control
// windows let body send with its HEADERS (reserved by the caller). It returns
// the rest of body for finishBody, which runs after the frames are queued.
func (a *h2ResponseAdapter) stageBody(s *stream.Stream, frameBuf, body []byte, sendLen int, maxFrame uint32) ([]byte, []byte) {
	if sendLen > 0 {
		isEnd := sendLen == len(body)
		// RFC 7540 §4.2: fragment by peer's MAX_FRAME_SIZE.
		frameBuf = appendH2DataFragmented(frameBuf, s.ID, isEnd, body[:sendLen], maxFrame)
		if isEnd {
			return frameBuf, nil
		}
	} else {
		sendLen = 0
	}
	return frameBuf, body[sendLen:]
}

// finishBody deals with the part of a response body that the windows did not
// take, once the response's HEADERS and first DATA are in the write queue:
// it buffers it on the stream when the connection's outbound budget has room
// for it (celeris#893) and the stream may be buffered at all (not one that
// runs on the pool only because of the budget: TryBufferOutbound), and
// otherwise sends it itself (sendRest).
//
// The rest is buffered only after the HEADERS are queued, never before
// (celeris#903): the event loop can flush a stream's buffer at any moment, and
// through the queue its DATA follows whatever of the stream is already in it,
// but a buffer filled ahead of the HEADERS would let a WINDOW_UPDATE queue the
// DATA first. And the window may have opened while the handler buffered, a
// WINDOW_UPDATE having found nothing to flush: the handler then flushes its own
// buffer, so the credit is not left unused until the next WINDOW_UPDATE.
func (a *h2ResponseAdapter) finishBody(s *stream.Stream, rest []byte, maxFrame uint32) error {
	if s.TryBufferOutbound(rest, true) {
		if m := a.manager; m != nil && s.RunsOnPool() && m.WindowsOpen(s) {
			m.FlushOutbound(s, a.sendData)
		}
		return nil
	}
	return a.sendRest(s, rest, maxFrame)
}

// sendRest sends the part of a worker-pool handler's response body that the
// windows did not take and TryBufferOutbound did not buffer (celeris#893):
// the connection's outbound budget had no room, or the stream is a sync one
// on the pool only because of the budget. Its HEADERS are queued. It waits for
// the windows and sends what they allow, through the write queue, until the
// body is out: the handler blocks, as a net/http handler blocks in Write on
// flow control, instead of the connection copying past its budget. None of it
// is buffered, even once the budget has room again (see
// Stream.TryBufferOutbound).
//
// It gives up, sending nothing more, when the stream is reset or its
// connection closes. And it is bounded by the server's WriteTimeout as a
// minimum rate, the rule of every stream that waits for the peer's window
// (stream.AwaitSendWindowRate, celeris#1170): the client must let at least 64 KiB
// of the rest out per WriteTimeout window, with the carry-over, the connection's
// credit and the grace the rule gives; when it does not, the stream is reset with
// INTERNAL_ERROR, as net/http does at a stream's write deadline, and sendRest
// returns an error that wraps os.ErrDeadlineExceeded. Before celeris#1170 the
// bound was WriteTimeout in all from the first wait (#906), which cut a client
// that read at a healthy rate when the bytes ahead of this stream in its socket
// took longer. The bound is a timer of this handler's, so it holds when the event
// loop cannot run: on the native engines a sync handler waiting for this one (a
// lock it holds across this write, a coalesced call it leads) holds its loop until
// this handler returns; while a loop is stuck like that the wait is bounded by
// WriteTimeout in all, as before.
func (a *h2ResponseAdapter) sendRest(s *stream.Stream, rest []byte, maxFrame uint32) error {
	var w stream.SendWait
	gen := s.Gen()
	defer w.End(s, gen)
	for {
		if err := s.AwaitSendWindowRate(&w, len(rest)); err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				return nil // reset by the peer, or the connection closed
			}
			a.resetStreamOnce(s, http2.ErrCodeInternal)
			return waitRateError(s.ID, len(rest), "the response", a.writeTimeout, &w, err)
		}
		if n := h2ReserveSend(a.manager, s, len(rest)); n > 0 {
			a.manager.NoteWaitSent(s.ID, n)
			isEnd := n == len(rest)
			pooled := getH2FrameBuf()
			frameBuf := (*pooled)[:0]
			if need := h2DataFramesLen(n, maxFrame); cap(frameBuf) < need {
				frameBuf = make([]byte, 0, need)
			}
			frameBuf = appendH2DataFragmented(frameBuf, s.ID, isEnd, rest[:n], maxFrame)
			*pooled = frameBuf
			a.writeQueue.Enqueue(s.ID, pooled)
			if isEnd {
				return nil
			}
			rest = rest[n:]
		}
	}
}

// waitRateError is the error of a handler whose wait for the peer's window was
// judged not to make the minimum rate of WriteTimeout (wraps err, an
// os.ErrDeadlineExceeded).
func waitRateError(id uint32, left int, what string, wt time.Duration, w *stream.SendWait, err error) error {
	if w.BlockedLoop() {
		return fmt.Errorf("h2: stream %d reset: %d bytes of %s still waited for the peer's window after WriteTimeout (%v) while an event loop of the server was blocked: %w",
			id, left, what, wt, err)
	}
	return fmt.Errorf("h2: stream %d reset: %d bytes of %s still waited for the peer's window and the client did not take the minimum rate of WriteTimeout (%v per window): %w",
		id, left, what, wt, err)
}

// resetStreamOnce is resetStream for a wait that was judged: the stream is cancelled
// first, and the RST_STREAM goes only if this call cancelled it. A stream the peer
// reset between the verdict and here gets no second reset (RFC 9113 §5.1).
func (a *h2ResponseAdapter) resetStreamOnce(s *stream.Stream, code http2.ErrCode) {
	if !s.CancelFirst() {
		return
	}
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(code))
	pooled := getH2FrameBuf()
	*pooled = appendH2Frame((*pooled)[:0], h2FrameRSTStream, 0, s.ID, payload[:])
	a.writeQueue.Enqueue(s.ID, pooled)
}

// resetStream queues a RST_STREAM for s behind the frames s has queued (all of
// a stream's frames go to one shard, in order) and cancels s, so its handler's
// context is done and nothing more of it is sent. A pool handler cannot use
// the connection's writer, which belongs to the event loop.
func (a *h2ResponseAdapter) resetStream(s *stream.Stream, code http2.ErrCode) {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(code))
	pooled := getH2FrameBuf()
	*pooled = appendH2Frame((*pooled)[:0], h2FrameRSTStream, 0, s.ID, payload[:])
	a.writeQueue.Enqueue(s.ID, pooled)
	s.Cancel()
}

// ResetStream is the processor's reset of a stream whose handler is over
// without having finished its response (stream.Resetter, celeris#889).
func (a *h2ResponseAdapter) ResetStream(s *stream.Stream, code http2.ErrCode) {
	a.resetStream(s, code)
}

// resetStreamUse is resetStream for a StreamWriter, which may outlive its
// handler (celeris#904): it does nothing once gen is no longer the stream's
// use, so it neither sends a RST_STREAM for the object's next stream nor
// cancels that stream.
func (a *h2ResponseAdapter) resetStreamUse(s *stream.Stream, gen uint64, code http2.ErrCode) {
	if s.UseLock(gen) != nil {
		return
	}
	defer s.UseUnlock()
	a.resetStreamOnce(s, code)
}

// SendGoAway writes a GOAWAY frame via the shared writer.
// Called from the event loop under H2State.mu — no adapter mutex needed.
func (a *h2ResponseAdapter) SendGoAway(lastStreamID uint32, code http2.ErrCode, debug []byte) error {
	if err := a.writer.WriteGoAway(lastStreamID, code, debug); err != nil {
		return err
	}
	return a.writer.Flush()
}

func (a *h2ResponseAdapter) MarkStreamClosed(_ uint32) {}

func (a *h2ResponseAdapter) IsStreamClosed(_ uint32) bool {
	return false
}

// WriteRSTStreamPriority writes a RST_STREAM frame via the shared writer.
// Called from the event loop under H2State.mu.
func (a *h2ResponseAdapter) WriteRSTStreamPriority(streamID uint32, code http2.ErrCode) error {
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
	case 413:
		return "Request Entity Too Large"
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

// h1 streaming support — h1ResponseAdapter implements stream.Streamer.

func (a *h1ResponseAdapter) WriteHeader(_ *stream.Stream, status int, headers [][2]string) error {
	a.werr = nil
	noContent := status < 200 || status == 204 || status == 304
	a.streamBegun, a.streamNoBody = true, a.isHEAD || noContent
	a.streamBroken = false
	a.streamInterim = status >= 100 && status < 200 && status != 101
	a.streamOpen = !a.streamNoBody
	a.streamDeclared, a.streamWritten = declaredLength(headers), 0
	buf := a.respBuf[:0]
	closing := a.closing(status)
	if closing && status >= 200 { // an interim head's Connection (101: Upgrade) is the handler's
		headers = withoutConnectionHeaders(headers)
	}
	buf = appendStatusLine(buf, status)
	buf = appendCachedDate(buf)
	if !noContent { // RFC 9112 section 6.1: not on a 1xx, 204 or 304
		buf = append(buf, "transfer-encoding: chunked\r\n"...)
	}
	for _, h := range headers {
		// Same invariant as WriteResponse: headers reach this path
		// pre-validated by celeris.Context's Set methods.
		buf = append(buf, h[0]...)
		buf = append(buf, ": "...)
		buf = append(buf, h[1]...)
		buf = append(buf, crlf...)
	}
	if closing {
		buf = append(buf, "connection: close\r\n"...)
	}
	buf = append(buf, crlf...)
	a.respBuf = buf
	// Asked with the size of the head, as Write is with its chunk: a head that
	// would take the response held unsent past the engine's limit is not staged.
	if err := a.askWriteErr(len(buf)); err != nil {
		return err
	}
	a.write(buf)
	return a.askWriteErr(0)
}

// askWriteErr is what a streaming call returns from the engine's query
// (H1State.SetWriteErrQuery) about to stage n bytes (0: after the write hook
// ran, or to ask the state only), kept for the rest of the response. With no
// query installed it is nil, as the hook alone always was.
func (a *h1ResponseAdapter) askWriteErr(n int) error {
	if a.writeErr == nil {
		return nil
	}
	if err := a.writeErr.StreamWriteErr(n); err != nil {
		a.werr = err
		return err
	}
	return nil
}

// maxChunkScratch bounds a connection's chunk scratch buffer: a chunk that
// needs more is framed in a buffer of its own, as a chunk always was.
const maxChunkScratch = 64 << 10

// maxChunkData is the most data one chunk carries.
const maxChunkData = 1 << 30

// chunkScratchKeep is the largest chunk scratch kept when a handler that
// streamed returns without detaching: a connection that goes on to serve other
// requests does not hold a large one.
const chunkScratchKeep = 4 << 10

// headResponse reports whether the response being written has no body: the one
// WriteHeader began (a HEAD's, or a status with no content), if it has begun
// one, else the request the adapter holds.
func (a *h1ResponseAdapter) headResponse() bool {
	if a.streamBegun {
		return a.streamNoBody
	}
	return a.isHEAD
}

func (a *h1ResponseAdapter) Write(_ *stream.Stream, data []byte) error {
	// RFC 9110 §9.3.2: a HEAD response has no content. WriteHeader sent
	// the header block (transfer-encoding: chunked says what a GET would
	// get); a chunk here would be read as the start of the next response
	// on the connection. Nor after the response was ended (Close): a chunk
	// behind the last chunk is no chunk.
	if a.headResponse() || (a.streamBegun && !a.streamOpen) {
		return nil
	}
	// The engine refused an earlier write of this response, or the conn is
	// gone: stage nothing more behind it.
	if a.werr != nil {
		return a.werr
	}
	// A zero-length chunk is the last chunk (RFC 9112 §7.1): an empty Write
	// sends nothing, as net/http's does, and Close ends the response.
	if len(data) == 0 {
		return nil
	}
	// A chunk of more than 1 GiB goes out as several, so that the size of the
	// buffer that frames one cannot overflow.
	for len(data) > maxChunkData {
		if err := a.Write(nil, data[:maxChunkData]); err != nil {
			return err
		}
		data = data[maxChunkData:]
	}
	// Chunked transfer encoding: hex(len)\r\n data \r\n, built in the
	// connection's scratch (see chunkBuf).
	limit := maxChunkScratch
	if a.scratchDetached {
		limit = chunkScratchKeep
	}
	var chunk []byte
	n := len(data)
	if n <= limit-20 {
		if need := n + 20; cap(a.chunkBuf) < need {
			a.chunkBuf = make([]byte, 0, 1<<bits.Len(uint(need-1)))
		}
		chunk = a.chunkBuf[:0]
		chunk = strconv.AppendInt(chunk, int64(n), 16)
		chunk = append(chunk, '\r', '\n')
		if err := a.askWriteErr(len(chunk) + n + len(crlf)); err != nil {
			return err
		}
		chunk = append(chunk, data...)
	} else {
		// Framed in a buffer of its own, grown from the prefix by append, which
		// does not zero the bytes the copy overwrites (make would).
		if cap(a.chunkBuf) < 32 {
			a.chunkBuf = make([]byte, 0, 32)
		}
		prefix := strconv.AppendInt(a.chunkBuf[:0], int64(n), 16)
		prefix = append(prefix, '\r', '\n')
		if err := a.askWriteErr(len(prefix) + n + len(crlf)); err != nil {
			return err
		}
		chunk = append(prefix[:len(prefix):len(prefix)], data...)
	}
	chunk = append(chunk, crlf...)
	a.write(chunk)
	a.streamWritten += int64(n)
	// And again after the hook: a detached conn's hook refuses a write whose
	// backlog is over the limit, and drops it.
	return a.askWriteErr(0)
}

// declaredInvalid is the declared length of a response whose content-length
// cannot be held to: not a plain number, or given more than once.
const declaredInvalid = -2

// declaredLength is the content-length the headers of a streamed response
// declare: -1 for none, declaredInvalid for one that cannot be held to (not
// all digits, too long for an int64, or several fields).
func declaredLength(headers [][2]string) int64 {
	declared := int64(-1)
	for _, h := range headers {
		if len(h[0]) != len("content-length") || !strings.EqualFold(h[0], "content-length") {
			continue
		}
		if declared != -1 {
			return declaredInvalid
		}
		v := strings.TrimSpace(h[1])
		if v == "" || len(v) > 18 {
			return declaredInvalid
		}
		declared = 0
		for i := 0; i < len(v); i++ {
			if v[i] < '0' || v[i] > '9' {
				return declaredInvalid
			}
			declared = declared*10 + int64(v[i]-'0')
		}
	}
	return declared
}

// streamComplete reports whether the streamed response that is under way has
// every byte its headers declared, if they declared any: the condition for
// ending it at the handler's return (a body that is not as long as declared is
// cut, not ended: the last chunk would make it look whole).
func (a *h1ResponseAdapter) streamComplete() bool {
	return a.streamDeclared == -1 || a.streamDeclared == a.streamWritten
}

// Flush has nothing to send: the write hook does not hold bytes back for it
// (on the native engines before Detach they wait for the handler to return,
// which a Flush cannot change, see StreamWriter.Flush). It reports what the
// engine says of the connection, as Write does.
func (a *h1ResponseAdapter) Flush(_ *stream.Stream) error {
	if a.werr != nil {
		return a.werr
	}
	return a.askWriteErr(0)
}

func (a *h1ResponseAdapter) Close(_ *stream.Stream) error {
	// A response with no body (HEAD, 1xx, 204, 304) has no last chunk (see
	// Write), and one that is already ended is not ended twice.
	if a.headResponse() || (a.streamBegun && !a.streamOpen) {
		return nil
	}
	a.streamOpen = false
	if a.werr != nil {
		return a.werr
	}
	// The last chunk is asked for like any other write: one that would take
	// the response past the engine's limit is not staged, so a Close never
	// stages bytes and reports them refused.
	last := []byte("0\r\n\r\n")
	if err := a.askWriteErr(len(last)); err != nil {
		return err
	}
	a.write(last)
	return a.askWriteErr(0)
}

// NeedsEmptyAnswer marks this writer as the native HTTP/1 engines': they send
// nothing for a handler that returns without answering, so the router answers
// for it (an empty 200); net/http's and HTTP/2's own writers do not have it.
func (a *h1ResponseAdapter) NeedsEmptyAnswer() {}

// FailResponse records that the handler failed after its streamed response was
// under way, with the failure swallowed above the engine (the router recovers a
// panic and answers an error with a 500 only when nothing was sent): the
// response is not whole, so it is not ended at the handler's return, and the
// connection is closed instead (handleH1Request), as a client of net/http sees
// a handler that panics after its headers. The HTTP/1.1 form of the reset the
// HTTP/2 processor sends (celeris#889).
func (a *h1ResponseAdapter) FailResponse() {
	a.streamBroken = true
}

// h2 streaming support — h2ResponseAdapter implements stream.Streamer.
// Streaming methods use goroutine-local encoders and the write queue,
// matching the WriteResponse path for async handler safety.

func (a *h2ResponseAdapter) WriteHeader(s *stream.Stream, status int, headers [][2]string) error {
	return a.WriteHeaderUse(s, s.Gen(), status, headers)
}

// WriteHeaderUse is WriteHeader for a StreamWriter: it is refused, queueing
// nothing and marking nothing on the stream, once gen is no longer the
// stream's use (celeris#904).
func (a *h2ResponseAdapter) WriteHeaderUse(s *stream.Stream, gen uint64, status int, headers [][2]string) error {
	if err := s.UseLock(gen); err != nil {
		return err
	}
	defer s.UseUnlock()
	enc := getH2StreamEncoder()

	var hdrBuf [16][2]string
	responseHeaders := hdrBuf[:0:16]
	if len(headers)+1 > 16 {
		responseHeaders = make([][2]string, 0, len(headers)+1)
	}
	responseHeaders = append(responseHeaders, [2]string{":status", statusCodeString(status)})
	responseHeaders = append(responseHeaders, headers...)

	headerBlock, err := enc.encodeHeaders(responseHeaders)
	if err != nil {
		putH2StreamEncoder(enc)
		return fmt.Errorf("HPACK encode error: %w", err)
	}

	maxFrame := a.peerMaxFrame()

	pooled := getH2FrameBuf()
	frameBuf := (*pooled)[:0]
	needed := 9 + len(headerBlock)
	if cap(frameBuf) < needed {
		frameBuf = make([]byte, 0, needed)
	}
	frameBuf = appendH2Headers(frameBuf, s.ID, false, headerBlock, maxFrame)

	putH2StreamEncoder(enc)

	*pooled = frameBuf
	a.writeQueue.Enqueue(s.ID, pooled)
	// A streamed response is whole only once it is closed: the processor
	// settles one whose handler returned without closing it, held to the
	// content-length these headers declare (celeris#889). Marked before
	// headersSent, so a stream with HEADERS out is never taken for a response
	// written whole.
	s.NoteStreamHeader(status, headers)
	s.SetHeadersSent()
	return nil
}

// Write sends a chunk of a streamed response body (celeris#904). It honours
// both of the peer's flow-control windows (RFC 9113 §6.9.1), reserving what it
// sends from each, as WriteResponse does:
//
//   - On a pool goroutine (an async route; a sync one the budget put on the pool), it sends what
//     the windows allow and waits for the rest (Stream.AwaitSendWindow), the
//     way net/http's server blocks a handler's Write on flow control. The wait
//     takes the same WriteTimeout deadline #906 gives WriteResponse's
//     (sendRest), counted from the moment this call first has to wait, so a peer that withholds window cannot
//     hold the goroutine, or a sync handler waiting on a lock it holds, for
//     good: then the stream is reset with INTERNAL_ERROR and Write returns an
//     error wrapping os.ErrDeadlineExceeded. One deadline per Write, not per
//     response: a stream (SSE) is meant to outlast any one write. If the
//     stream ends while Write waits (the peer reset it, the connection
//     closed), Write returns the stream's context error.
//   - Otherwise (a sync handler streaming inline on the event loop, or a goroutine
//     it detached: neither is flagged as a pool handler) it cannot wait: what
//     the windows refuse is buffered on the stream behind the DATA already
//     sent, charged to the connection's outbound budget (which the budget does
//     not limit here: a slow inline stream can push its connection over budget,
//     and its later sync GETs then run on the pool), and the loop sends it as
//     the peer grants window. Write returns at once.
//
// A StreamWriter that outlives its handler is refused (WriteUse): on HTTP/2 the
// stream ends when the handler returns, so a goroutine it started that writes
// later gets stream.ErrStreamEnded, and nothing is sent or buffered.
//
// Without a manager (adapters built by unit tests) there is no flow control.
func (a *h2ResponseAdapter) Write(s *stream.Stream, data []byte) error {
	return a.WriteUse(s, s.Gen(), data)
}

// WriteUse is Write for a StreamWriter (UseStreamer): gen is the use token it
// took when it was made. Once the stream's use is over (its handler returned,
// it was released, the peer reset it) the call is refused with an error and
// sends, buffers and charges nothing (celeris#904); every step that touches
// the stream is checked against gen under the lock its release takes.
func (a *h2ResponseAdapter) WriteUse(s *stream.Stream, gen uint64, data []byte) error {
	m := a.manager
	if m == nil {
		// RFC 9110 §9.3.2 / RFC 9113 §8.1.1: no DATA payload on a HEAD
		// response. Close still ends the stream with an empty DATA frame.
		if s.IsHEAD || len(data) == 0 {
			return nil
		}
		a.sendData(s.ID, false, data)
		return nil
	}
	total := len(data)
	n, id, onPool, err := m.StreamWrite(s, gen, data, a.sendData)
	if err != nil {
		return err
	}
	data = data[n:]
	if !onPool {
		return nil // taken whole: sent, buffered behind the DATA already sent, or a HEAD's, dropped
	}
	// A pool goroutine: wait for the windows and send as they allow. The wait
	// is judged as a minimum rate of WriteTimeout (stream.AwaitSendWindowRateUse),
	// from the moment the call first has to wait: a Write that finds the windows
	// open reads no clock, and takes one lock. One wait per Write, not per
	// response: a stream (SSE) is meant to outlast any one write.
	var w stream.SendWait
	defer w.End(s, gen)
	for len(data) > 0 {
		if err := s.AwaitSendWindowRateUse(gen, &w, len(data)); err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				return err // reset by the peer, the connection closed, or the stream's use is over
			}
			a.resetStreamUse(s, gen, http2.ErrCodeInternal)
			return waitRateError(id, len(data), fmt.Sprintf("a write of %d bytes", total), a.writeTimeout, &w, err)
		}
		n, _, _, err := m.StreamWrite(s, gen, data, a.sendData)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (a *h2ResponseAdapter) Flush(_ *stream.Stream) error {
	return nil // write queue data is drained by event loop
}

// Close ends a streamed response: an empty DATA frame with END_STREAM,
// queued at once, or, when the stream still has DATA buffered for the peer's
// window (an inline handler's), carried by the last of it (EndOutbound).
func (a *h2ResponseAdapter) Close(s *stream.Stream) error {
	return a.CloseUse(s, s.Gen())
}

// CloseUse is Close for a StreamWriter: refused, sending nothing, once gen is
// no longer the stream's use, and on a stream that was reset (the peer's
// RST_STREAM, or the server's at a Write's timeout): RFC 9113 §5.1 allows no
// frame after one.
func (a *h2ResponseAdapter) CloseUse(s *stream.Stream, gen uint64) error {
	if m := a.manager; m != nil {
		return m.EndOutbound(s, gen, a.sendData)
	}
	a.sendData(s.ID, true, nil)
	return nil
}

// sendData frames data as DATA for stream id, fragmented by the peer's
// SETTINGS_MAX_FRAME_SIZE (RFC 9113 §4.2), and queues it behind the stream's
// earlier frames (all of a stream's frames go to one shard). endStream is
// set on the last frame; data may be empty only with endStream. The bytes are
// copied into the frame buffer, so data may be a view of the caller's buffer.
// Safe on any goroutine; it wakes the event loop.
func (a *h2ResponseAdapter) sendData(id uint32, endStream bool, data []byte) {
	a.writeQueue.Enqueue(id, a.dataFrames(id, endStream, data))
}

// sendDataFromLoop is sendData for the event loop's own flushes inside
// ProcessH2 (Processor.SetOutboundSink): it needs no wakeup.
func (a *h2ResponseAdapter) sendDataFromLoop(id uint32, endStream bool, data []byte) {
	a.writeQueue.enqueueFromLoop(id, a.dataFrames(id, endStream, data))
}

func (a *h2ResponseAdapter) dataFrames(id uint32, endStream bool, data []byte) *[]byte {
	maxFrame := a.peerMaxFrame()
	pooled := getH2FrameBuf()
	frameBuf := (*pooled)[:0]
	// Worst case: one 9-byte header per maxFrame-sized chunk plus the data itself.
	if need := h2DataFramesLen(len(data), maxFrame); cap(frameBuf) < need {
		frameBuf = make([]byte, 0, need)
	}
	frameBuf = appendH2DataFragmented(frameBuf, id, endStream, data, maxFrame)
	*pooled = frameBuf
	return pooled
}

var _ stream.Streamer = (*h1ResponseAdapter)(nil)
var _ stream.Streamer = (*h2ResponseAdapter)(nil)
var _ stream.UseStreamer = (*h2ResponseAdapter)(nil)

func writeErrorResponse(write func([]byte), status int, message string) {
	pooled := getResponseBuffer()
	buf := (*pooled)[:0]
	buf = appendStatusLine(buf, status)
	buf = append(buf, "content-type: text/plain; charset=utf-8\r\n"...)
	buf = append(buf, "content-length: "...)
	buf = strconv.AppendInt(buf, int64(len(message)), 10)
	buf = append(buf, crlf...)
	buf = append(buf, "connection: close\r\n"...)
	buf = append(buf, crlf...)
	buf = append(buf, message...)
	write(buf)
	*pooled = buf
	putResponseBuffer(pooled)
}
