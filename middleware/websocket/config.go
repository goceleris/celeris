package websocket

import (
	"bufio"
	"io"
	"time"

	"github.com/goceleris/celeris"
)

// BufferPool is an interface for borrowing and returning [bufio.Writer]
// instances that the WebSocket writer uses on the hijack (std engine)
// path. A typical production implementation wraps a [sync.Pool]:
//
//	type wsPool struct{ p sync.Pool }
//	func (w *wsPool) Get(dst io.Writer) *bufio.Writer {
//	    if v := w.p.Get(); v != nil {
//	        bw := v.(*bufio.Writer)
//	        bw.Reset(dst)
//	        return bw
//	    }
//	    return bufio.NewWriterSize(dst, 4096)
//	}
//	func (w *wsPool) Put(bw *bufio.Writer) { w.p.Put(bw) }
//
// BufferPool is not consulted on the native-engine path (epoll/io_uring),
// which uses the engine's per-connection write buffer internally.
type BufferPool interface {
	// Get returns a bufio.Writer reset to write into dst. The pool
	// should Reset(dst) on borrow so the returned writer has no stale
	// buffered bytes. If the pool is empty, it must allocate a new
	// [bufio.Writer] (typically with [bufio.NewWriterSize]).
	Get(dst io.Writer) *bufio.Writer
	// Put returns a bufio.Writer to the pool. The caller has already
	// called Flush on the writer. Implementations may discard the
	// writer if e.g. its buffer grew beyond an acceptable size.
	Put(bw *bufio.Writer)
}

// Handler is called with the upgraded WebSocket connection.
// The function should block until the connection is done.
// When Handler returns, the connection is closed automatically.
type Handler func(*Conn)

// Config defines the WebSocket middleware configuration.
type Config struct {
	// Handler is called after a successful WebSocket upgrade.
	// Required. Panics if nil.
	Handler Handler

	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match on c.Path()).
	SkipPaths []string

	// CheckOrigin returns true if the request origin is acceptable.
	// If nil, the default same-origin check is used (Origin header must
	// match the Host header). Set to func(*celeris.Context) bool { return true }
	// to allow all origins.
	CheckOrigin func(c *celeris.Context) bool

	// Subprotocols specifies the server's supported protocols in preference order.
	Subprotocols []string

	// ReadBufferSize specifies the I/O read buffer size in bytes.
	// Default: 4096.
	ReadBufferSize int

	// WriteBufferSize specifies the I/O write buffer size in bytes.
	// Default: 4096.
	WriteBufferSize int

	// ReadLimit is the maximum message size in bytes.
	// Default: 64MB.
	ReadLimit int64

	// HandshakeTimeout specifies the duration for the handshake to complete.
	// Default: 0 (no timeout).
	HandshakeTimeout time.Duration

	// WriteBufferPool is an optional pool for write buffers. When set,
	// write buffers are obtained from the pool before each write and
	// returned after flush, reducing memory for idle connections.
	// If nil, each connection allocates its own permanent write buffer.
	WriteBufferPool BufferPool

	// EnableCompression enables permessage-deflate compression (RFC 7692).
	// When enabled, the server negotiates compression during the upgrade
	// handshake. Messages are compressed transparently.
	EnableCompression bool

	// CompressionLevel controls the deflate compression level.
	// Valid range: -2 (Huffman only) to 9 (best compression).
	// Default: 1 (best speed). Use [CompressionLevelDefault] for the
	// flate library default (-1).
	CompressionLevel int

	// CompressionThreshold is the minimum payload size in bytes for
	// compression. Messages smaller than this are sent uncompressed.
	// Default: 128.
	CompressionThreshold int

	// IdleTimeout is the maximum time between messages before the connection
	// is closed. When set, the next read deadline is extended after each
	// successful frame read. On the std (hijack) path this is enforced via
	// net.Conn.SetReadDeadline; on native engines (epoll/io_uring) it is
	// enforced via the engine's idle sweep using SetWSIdleDeadline.
	// Zero means no idle timeout.
	IdleTimeout time.Duration

	// MaxBackpressureBuffer is the maximum number of inbound chunks
	// buffered between the engine event loop and the WebSocket handler
	// goroutine on the engine-integrated path. When the buffer fills past
	// BackpressureHighPct, the engine pauses inbound delivery for this
	// connection (TCP-level backpressure); when it drains below
	// BackpressureLowPct, delivery is resumed.
	// Default: 256. Ignored on the std (hijack) engine path.
	MaxBackpressureBuffer int

	// BackpressureHighPct is the buffer fill percentage (0-100) at which
	// the engine is asked to pause inbound delivery. Default: 75.
	BackpressureHighPct int

	// BackpressureLowPct is the buffer fill percentage (0-100) at which
	// the engine is asked to resume inbound delivery after a pause. Must
	// be lower than BackpressureHighPct or it falls back to the default
	// (25). Default: 25.
	BackpressureLowPct int

	// OnConnect is called after upgrade succeeds, before Handler.
	// If it returns a non-nil error, the connection is closed.
	OnConnect func(*Conn) error

	// OnDisconnect is called after the Handler returns.
	OnDisconnect func(*Conn)
}

var defaultConfig = Config{}

func applyDefaults(cfg Config) Config {
	if cfg.ReadBufferSize <= 0 {
		cfg.ReadBufferSize = defaultReadBufSize
	}
	if cfg.WriteBufferSize <= 0 {
		cfg.WriteBufferSize = defaultWriteBufSize
	}
	if cfg.ReadLimit <= 0 {
		cfg.ReadLimit = defaultReadLimit
	}
	if cfg.EnableCompression && cfg.CompressionLevel == 0 {
		cfg.CompressionLevel = defaultCompressionLevel
	}
	if cfg.EnableCompression && cfg.CompressionThreshold <= 0 {
		cfg.CompressionThreshold = 128
	}
	if cfg.MaxBackpressureBuffer <= 0 {
		cfg.MaxBackpressureBuffer = 256
	}
	if cfg.BackpressureHighPct <= 0 || cfg.BackpressureHighPct > 100 {
		cfg.BackpressureHighPct = 75
	}
	if cfg.BackpressureLowPct <= 0 || cfg.BackpressureLowPct >= cfg.BackpressureHighPct {
		cfg.BackpressureLowPct = 25
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.Handler == nil {
		panic("websocket: Handler must not be nil")
	}
}
