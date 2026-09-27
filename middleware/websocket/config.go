//line middleware/websocket/config.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

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
	//
	// Zero means no idle timeout, and on the native engines that means the
	// connection has NO liveness bound at all (celeris#524). Once a
	// connection detaches, the engine deliberately stops applying its own
	// ReadTimeout / IdleTimeout / WriteTimeout — the middleware owns the I/O
	// lifecycle from that point — and reaps a detached connection only when
	// the middleware has published a deadline. This field is what publishes
	// one. Leave it zero and nothing will ever close an idle connection,
	// which is correct for a long-lived WebSocket and is why zero is the
	// default.
	//
	// Worth setting anyway if you want a backstop. A connection that is
	// WEDGED rather than merely idle is indistinguishable from a healthy
	// quiet one without a deadline, so any engine-side liveness bug becomes
	// permanent for that connection instead of self-limiting. Several such
	// bugs have been found and fixed (celeris#527, #482); a deadline is what
	// would have made them survivable.
	IdleTimeout time.Duration

	// MaxBackpressureBuffer is the maximum number of inbound chunks
	// buffered between the engine event loop and the WebSocket handler
	// goroutine on the engine-integrated path. When the buffer fills past
	// BackpressureHighPct, the engine pauses inbound delivery for this
	// connection (TCP-level backpressure); when it drains below
	// BackpressureLowPct, delivery is resumed.
	//
	// The pause is applied asynchronously on the engine worker, so chunks
	// already in flight keep arriving after it is requested and can exceed
	// the (1 - BackpressureHighPct) headroom. Those chunks are queued
	// rather than discarded (celeris#484), so the worst-case buffered
	// depth is TWICE this value; a peer that outruns even that is cut off
	// with ErrReadLimit. Size it for memory as 2 x this many chunks per
	// connection.
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

func applyDefaults(cfg Config) Config {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[0], 1);
	if cfg.ReadBufferSize <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[9], 1);
		cfg.ReadBufferSize = defaultReadBufSize
	}
	_cover_atomic_.AddUint32(&GoCover_c633_config.Count[1], 1);if cfg.WriteBufferSize <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[10], 1);
		cfg.WriteBufferSize = defaultWriteBufSize
	}
	_cover_atomic_.AddUint32(&GoCover_c633_config.Count[2], 1);if cfg.ReadLimit <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[11], 1);
		cfg.ReadLimit = defaultReadLimit
	}
	_cover_atomic_.AddUint32(&GoCover_c633_config.Count[3], 1);if cfg.EnableCompression && cfg.CompressionLevel == 0 {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[12], 1);
		cfg.CompressionLevel = defaultCompressionLevel
	}
	_cover_atomic_.AddUint32(&GoCover_c633_config.Count[4], 1);if cfg.EnableCompression && cfg.CompressionThreshold <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[13], 1);
		cfg.CompressionThreshold = 128
	}
	_cover_atomic_.AddUint32(&GoCover_c633_config.Count[5], 1);if cfg.MaxBackpressureBuffer <= 0 {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[14], 1);
		cfg.MaxBackpressureBuffer = 256
	}
	_cover_atomic_.AddUint32(&GoCover_c633_config.Count[6], 1);if cfg.BackpressureHighPct <= 0 || cfg.BackpressureHighPct > 100 {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[15], 1);
		cfg.BackpressureHighPct = 75
	}
	_cover_atomic_.AddUint32(&GoCover_c633_config.Count[7], 1);if cfg.BackpressureLowPct <= 0 || cfg.BackpressureLowPct >= cfg.BackpressureHighPct {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[16], 1);
		cfg.BackpressureLowPct = 25
	}
	_cover_atomic_.AddUint32(&GoCover_c633_config.Count[8], 1);return cfg
}

func (cfg Config) validate() {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[17], 1);
	if cfg.Handler == nil {_cover_atomic_.AddUint32(&GoCover_c633_config.Count[18], 1);
		panic("websocket: Handler must not be nil")
	}
}

var GoCover_c633_config = struct {
	Count     [19]uint32
	Pos       [3 * 19]uint32
	NumStmt   [19]uint16
} {
	Pos: [3 * 19]uint32{
		166, 166, 0x1d0002, // [0]
		169, 169, 0x1e0002, // [1]
		172, 172, 0x180002, // [2]
		175, 175, 0x380002, // [3]
		178, 178, 0x3c0002, // [4]
		181, 181, 0x240002, // [5]
		184, 184, 0x430002, // [6]
		187, 187, 0x560002, // [7]
		190, 190, 0xc0002, // [8]
		167, 168, 0x10003, // [9]
		170, 171, 0x10003, // [10]
		173, 174, 0x10003, // [11]
		176, 177, 0x10003, // [12]
		179, 180, 0x10003, // [13]
		182, 183, 0x10003, // [14]
		185, 186, 0x10003, // [15]
		188, 189, 0x10003, // [16]
		194, 194, 0x180002, // [17]
		195, 195, 0x2e0003, // [18]
	},
	NumStmt: [19]uint16{
		1, // 0
		1, // 1
		1, // 2
		1, // 3
		1, // 4
		1, // 5
		1, // 6
		1, // 7
		1, // 8
		1, // 9
		1, // 10
		1, // 11
		1, // 12
		1, // 13
		1, // 14
		1, // 15
		1, // 16
		1, // 17
		1, // 18
	},
}

var _ = _cover_atomic_.LoadUint32
