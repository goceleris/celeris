//line websocket.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/goceleris/celeris"
)

// New creates a WebSocket middleware that upgrades matching requests.
//
// Non-WebSocket requests are passed through to the next handler. HTTP/2
// requests receive a 426 Upgrade Required response because connection
// hijacking is not possible over multiplexed streams.
//
// On native engines (epoll, io_uring), the connection remains in the
// event loop after upgrade — reads are delivered by the engine, writes
// go through the engine's write buffer with backpressure. On the std
// engine, the connection is hijacked for direct I/O.
//
// This is a zero-dependency native WebSocket implementation (RFC 6455).
//
// Usage:
//
//	server.GET("/ws", websocket.New(websocket.Config{
//	    Handler: func(c *websocket.Conn) {
//	        for {
//	            mt, msg, err := c.ReadMessage()
//	            if err != nil {
//	                break
//	            }
//	            c.WriteMessage(mt, msg)
//	        }
//	    },
//	}))
func New(config ...Config) celeris.HandlerFunc {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[0], 1);
	cfg := defaultConfig
	if len(config) > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[5], 1);
		cfg = config[0]
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[1], 1);cfg = applyDefaults(cfg)
	cfg.validate()

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[2], 1);var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[3], 1);handler := cfg.Handler
	onConnect := cfg.OnConnect
	onDisconnect := cfg.OnDisconnect
	checkOrigin := cfg.CheckOrigin
	subprotocols := cfg.Subprotocols
	readBufSize := cfg.ReadBufferSize
	writeBufSize := cfg.WriteBufferSize
	readLimit := cfg.ReadLimit
	handshakeTimeout := cfg.HandshakeTimeout
	enableCompression := cfg.EnableCompression

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[4], 1);return func(c *celeris.Context) error {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[6], 1);
		if skip.ShouldSkip(c) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[22], 1);
			return c.Next()
		}

		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[7], 1);if !c.IsWebSocket() {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[23], 1);
			return c.Next()
		}

		// HTTP/2 cannot be hijacked.
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[8], 1);if c.Protocol() == "2" {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[24], 1);
			return celeris.NewHTTPError(426, "websocket: upgrade not supported over HTTP/2")
		}

		// Validate the upgrade request.
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[9], 1);wsKey, err := validateUpgrade(c)
		if err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[25], 1);
			return celeris.NewHTTPError(400, err.Error())
		}

		// Origin check (default: same-origin).
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[10], 1);if checkOrigin != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[26], 1);
			if !checkOrigin(c) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[27], 1);
				return celeris.NewHTTPError(403, "websocket: origin not allowed")
			}
		} else{ _cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[28], 1);{
			// Default same-origin check: Origin must match Host. Browser
			// clients always send Origin on cross-origin upgrade attempts;
			// missing Origin on https:// is treated as a CSRF-class
			// signal and rejected. Non-browser clients that legitimately
			// omit Origin should set an explicit [Config.CheckOrigin].
			origin := c.Header("origin")
			if origin == "" {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[29], 1);
				if c.Scheme() == "https" {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[30], 1);
					return celeris.NewHTTPError(403, "websocket: missing Origin on https — set Config.CheckOrigin to allow")
				}
				// Plain http: keep the legacy permissive behavior; loopback
				// dev tools and CLI clients commonly omit Origin and would
				// break otherwise.
			} else{ _cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[31], 1);{
				host := c.Host()
				if !checkSameOrigin(origin, host) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[32], 1);
					return celeris.NewHTTPError(403, "websocket: origin not allowed")
				}
			}}
		}}

		// Negotiate subprotocol.
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[11], 1);subproto := negotiateSubprotocol(
			c.Header("sec-websocket-protocol"),
			subprotocols,
		)

		// Negotiate compression.
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[12], 1);compress := negotiateCompression(
			c.Header("sec-websocket-extensions"),
			enableCompression,
		)

		// Capture request metadata before upgrade. On the native engines
		// reqHeaders is the stream's own header slice, and Context.Detach
		// (in tryEngineUpgrade) replaces its entries with clones in place,
		// so the Conn never keeps a header view.
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[13], 1);reqHeaders := c.RequestHeaders()
		acceptKey := computeAcceptKey(wsKey)

		// Try engine-integrated path first (native engines: epoll/io_uring).
		// On native engines, the handler runs on the event loop thread.
		// We must spawn a goroutine for the blocking handler and return
		// immediately to free the event loop.
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[14], 1);ws, done := tryEngineUpgrade(c, acceptKey, subproto, readBufSize, readLimit, compress,
			cfg.MaxBackpressureBuffer, cfg.BackpressureHighPct, cfg.BackpressureLowPct)

		// The query is captured only now (celeris#714). On the engine path
		// tryEngineUpgrade has detached, and Detach copied the raw query and
		// any query already parsed from it, so the pairs are copies without
		// cloning them again. On the hijack path nothing has been hijacked
		// yet, and captureQuery clones them itself.
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[15], 1);queryParams := captureQuery(c, ws == nil)

		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[16], 1);if ws != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[33], 1);
			// Engine path: populate conn and run handler in goroutine.
			setupConn(ws, &cfg, compress,
				reqHeaders, queryParams)
			go func() {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[35], 1);
				defer func() {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[38], 1);
					ws.fragWriting.Store(false) // clear in case handler panicked mid-NextWriter
					if !ws.closeSent.Load() {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[41], 1);
						_ = ws.writeCloseFrame(CloseNormalClosure, "")
					}
					_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[39], 1);_ = ws.Close()
					if onDisconnect != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[42], 1);
						onDisconnect(ws)
					}
					_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[40], 1);if done != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[43], 1);
						done()
					}
				}()
				_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[36], 1);if onConnect != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[44], 1);
					if err := onConnect(ws); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[45], 1);
						return
					}
				}
				_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[37], 1);handler(ws)
			}()
			_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[34], 1);return nil // free the event loop thread
		}

		// Hijack path (std engine): handler blocks in this goroutine.
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[17], 1);ws, err = hijackUpgrade(c, acceptKey, subproto, readBufSize, writeBufSize, readLimit, handshakeTimeout, compress)
		if err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[46], 1);
			return err
		}

		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[18], 1);setupConn(ws, &cfg, compress,
			reqHeaders, queryParams)

		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[19], 1);defer func() {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[47], 1);
			ws.fragWriting.Store(false) // clear in case handler panicked mid-NextWriter
			if !ws.closeSent.Load() {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[49], 1);
				_ = ws.writeCloseFrame(CloseNormalClosure, "")
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[48], 1);_ = ws.Close()
			if onDisconnect != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[50], 1);
				onDisconnect(ws)
			}
		}()

		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[20], 1);if onConnect != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[51], 1);
			if err := onConnect(ws); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[52], 1);
				return nil
			}
		}

		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[21], 1);handler(ws)
		return nil
	}
}

// tryEngineUpgrade attempts engine-integrated WebSocket. Returns (nil, nil)
// if the engine doesn't support it (e.g. std engine).
//
// On native engines (epoll, io_uring):
//  1. Registers a WSDataDelivery callback that pushes inbound chunks into
//     a chanReader (bufio.Reader's source).
//  2. Detaches (the engine installs a guarded writeFn that's safe to call
//     from any goroutine, plus PauseRecv/ResumeRecv for backpressure).
//  3. Sends the 101 response via the engine's raw writeFn (bypasses chunked
//     encoding).
//  4. Wires error propagation, idle deadline, and pause/resume callbacks
//     between the WS Conn and the engine.
//  5. Returns the new Conn plus a `done` callback the caller invokes when
//     the handler goroutine finishes.
func tryEngineUpgrade(c *celeris.Context, acceptKey, subproto string,
	readBufSize int, readLimit int64, compress bool,
	backpressure, highPct, lowPct int) (*Conn, func()) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[53], 1);

	reader := newChanReader(backpressure, highPct, lowPct)

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[54], 1);if !c.UpgradeWebSocket(func(data []byte) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[65], 1);
		// Called on the event loop thread — must NOT block.
		// COPY first because the engine reuses its read buffer after the
		// callback returns; chanReader stores the slice as-is.
		cp := make([]byte, len(data))
		copy(cp, data)
		reader.Append(cp)
	}) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[66], 1);
		return nil, nil
	}

	// Pre-construct the Conn shell so the error handler installed
	// BEFORE Detach has a stable place to record write errors. After
	// Detach, the engine may immediately call OnError (e.g. a pre-
	// existing peer RST race window between UpgradeWebSocket and the
	// caller-visible Conn); installing the handler ahead of Detach
	// avoids losing that error.
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[55], 1);ctx, cancel := context.WithCancel(c.Context())
	ws := newEngineConn(ctx, cancel, reader, nil, readBufSize) // rawWrite filled after Detach
	ws.readLimit = readLimit
	ws.subprotocol = subproto

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[56], 1);c.SetWSErrorHandler(func(err error) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[67], 1);
		if err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[69], 1);
			ws.writeErr.Store(storedWriteErr{err})
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[68], 1);reader.closeWith(err)
	})

	// Detach installs the guarded writeFn and engine pause/resume hooks.
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[57], 1);done := c.Detach()

	// Get the raw write function (bypasses chunked encoding) and finish
	// wiring it into the Conn.
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[58], 1);rawWrite := c.WSRawWriteFn()
	if rawWrite == nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[70], 1);
		reader.closeWith(io.EOF)
		done()
		return nil, nil
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[59], 1);ws.setRawWrite(rawWrite)

	// Write the 101 response using raw write (no chunked encoding).
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[60], 1);resp := buildUpgradeResponse(acceptKey, subproto, compress)
	rawWrite(resp)

	// Wire engine pause/resume into the chanReader for TCP-level
	// backpressure. The engine applies pause/resume asynchronously via
	// its detach queue.
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[61], 1);if pause, resume := c.WSReadPauser(); pause != nil && resume != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[71], 1);
		reader.SetPauser(pause, resume)
	}

	// Idle timeout on the engine path: store a closure the WS conn calls
	// after each successful frame read to extend the deadline.
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[62], 1);ws.idleDeadlineFn = func(ns int64) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[72], 1);
		c.SetWSIdleDeadline(ns)
	}

	// Tell the engine to close the chanReader / wake the handler goroutine
	// when it tears down this detached connection.
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[63], 1);c.SetWSDetachClose(func() {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[73], 1);
		_ = ws.Close()
	})

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[64], 1);return ws, done
}

// hijackUpgrade performs the traditional hijack-based WebSocket upgrade.
func hijackUpgrade(c *celeris.Context, acceptKey, subproto string,
	readBufSize, writeBufSize int, readLimit int64, timeout time.Duration, compress bool) (*Conn, error) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[74], 1);

	rawConn, err := c.Hijack()
	if err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[80], 1);
		return nil, celeris.NewHTTPError(500, "websocket: hijack failed: "+err.Error())
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[75], 1);if timeout > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[81], 1);
		_ = rawConn.SetDeadline(time.Now().Add(timeout))
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[76], 1);resp := buildUpgradeResponse(acceptKey, subproto, compress)
	if _, err := rawConn.Write(resp); err != nil {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[82], 1);
		_ = rawConn.Close()
		return nil, err
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[77], 1);if timeout > 0 {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[83], 1);
		_ = rawConn.SetDeadline(time.Time{})
	}

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[78], 1);ctx, cancel := context.WithCancel(c.Context())
	ws := newConn(ctx, cancel, rawConn, readBufSize, writeBufSize)
	ws.readLimit = readLimit
	ws.subprotocol = subproto

	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[79], 1);return ws, nil
}

func setupConn(ws *Conn, cfg *Config, compress bool,
	headers [][2]string, query [][2]string) {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[84], 1);
	if compress {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[87], 1);
		ws.compressEnabled = true
		ws.compressLevel = cfg.CompressionLevel
		ws.compressThreshold = cfg.CompressionThreshold
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[85], 1);ws.headers = headers
	ws.query = query
	ws.idleTimeout = cfg.IdleTimeout
	// WriteBufferPool: only consulted on the hijack (std) path. The native
	// engine path uses the engine's internal write buffer pool (cs.writeBuf)
	// and ignores this setting — see Conn.getWriter().
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[86], 1);ws.writePool = cfg.WriteBufferPool
}

// captureQuery returns the upgrade request's query parameters for
// [Conn.Query]. On epoll and io_uring the raw query is a view of the engine's
// receive buffer, and url.ParseQuery returns substrings of it for anything it
// did not have to unescape. A view is safe to compare while the request is
// being handled and unsafe to keep after it, and the Conn keeps these for the
// connection's lifetime while the engine receives the WebSocket frames into
// that same buffer: kept as views, they read back as frame bytes, or a lookup
// no longer finds its key (celeris#714).
//
// So the pairs must be copies. After Context.Detach they are (Detach copies
// the raw query and the parsed query cache), and clone is false. Before it,
// clone is true and every key and value is cloned: one copy per upgrade,
// nothing per message.
func captureQuery(c *celeris.Context, clone bool) [][2]string {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[88], 1);
	qp := c.QueryParams()
	if len(qp) == 0 {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[91], 1);
		return nil
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[89], 1);result := make([][2]string, 0, len(qp))
	for k, vs := range qp {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[92], 1);
		if clone {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[94], 1);
			k = strings.Clone(k)
		}
		_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[93], 1);for _, v := range vs {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[95], 1);
			if clone {_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[97], 1);
				v = strings.Clone(v)
			}
			_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[96], 1);result = append(result, [2]string{k, v})
		}
	}
	_cover_atomic_.AddUint32(&GoCover_b2b_websocket.Count[90], 1);return result
}

var GoCover_b2b_websocket = struct {
	Count     [98]uint32
	Pos       [3 * 98]uint32
	NumStmt   [98]uint16
} {
	Pos: [3 * 98]uint32{
		39, 40, 0x150002, // [0]
		43, 45, 0x10002, // [1]
		46, 48, 0x10002, // [2]
		49, 59, 0x10002, // [3]
		60, 60, 0x280002, // [4]
		41, 42, 0x10003, // [5]
		61, 61, 0x190003, // [6]
		65, 65, 0x170003, // [7]
		70, 70, 0x1a0003, // [8]
		75, 76, 0x110003, // [9]
		81, 81, 0x190003, // [10]
		108, 112, 0x10003, // [11]
		114, 118, 0x10003, // [12]
		123, 125, 0x10003, // [13]
		130, 132, 0x10003, // [14]
		138, 139, 0x10003, // [15]
		140, 140, 0x100003, // [16]
		169, 170, 0x110003, // [17]
		174, 176, 0x10003, // [18]
		177, 177, 0x100003, // [19]
		188, 188, 0x170003, // [20]
		194, 195, 0xd0003, // [21]
		62, 63, 0x10004, // [22]
		66, 67, 0x10004, // [23]
		71, 72, 0x10004, // [24]
		77, 78, 0x10004, // [25]
		82, 82, 0x170004, // [26]
		83, 84, 0x10005, // [27]
		91, 92, 0x140004, // [28]
		93, 93, 0x1e0005, // [29]
		94, 95, 0x10006, // [30]
		100, 101, 0x270005, // [31]
		102, 103, 0x10006, // [32]
		142, 144, 0xe0004, // [33]
		165, 165, 0xe0004, // [34]
		145, 145, 0x120005, // [35]
		158, 158, 0x190005, // [36]
		163, 163, 0x100005, // [37]
		146, 147, 0x1e0006, // [38]
		150, 151, 0x1d0006, // [39]
		154, 154, 0x150006, // [40]
		148, 149, 0x10007, // [41]
		152, 153, 0x10007, // [42]
		155, 156, 0x10007, // [43]
		159, 159, 0x2a0006, // [44]
		160, 161, 0x10007, // [45]
		171, 172, 0x10004, // [46]
		178, 179, 0x1c0004, // [47]
		182, 183, 0x1b0004, // [48]
		180, 181, 0x10005, // [49]
		184, 185, 0x10005, // [50]
		189, 189, 0x280004, // [51]
		190, 191, 0x10005, // [52]
		217, 218, 0x10002, // [53]
		219, 219, 0x2b0002, // [54]
		236, 240, 0x10002, // [55]
		241, 241, 0x260002, // [56]
		249, 250, 0x10002, // [57]
		253, 254, 0x150002, // [58]
		259, 260, 0x10002, // [59]
		262, 264, 0x10002, // [60]
		268, 268, 0x460002, // [61]
		274, 274, 0x250002, // [62]
		280, 280, 0x1c0002, // [63]
		284, 284, 0x110002, // [64]
		223, 226, 0x10003, // [65]
		227, 228, 0x10003, // [66]
		242, 242, 0x110003, // [67]
		245, 245, 0x180003, // [68]
		243, 244, 0x10004, // [69]
		255, 258, 0x10003, // [70]
		269, 270, 0x10003, // [71]
		275, 276, 0x10003, // [72]
		281, 282, 0x10003, // [73]
		291, 292, 0x100002, // [74]
		296, 296, 0x110002, // [75]
		300, 301, 0x2f0002, // [76]
		306, 306, 0x110002, // [77]
		310, 314, 0x10002, // [78]
		315, 315, 0x100002, // [79]
		293, 294, 0x10003, // [80]
		297, 298, 0x10003, // [81]
		302, 304, 0x10003, // [82]
		307, 308, 0x10003, // [83]
		320, 320, 0xe0002, // [84]
		325, 328, 0x10002, // [85]
		331, 331, 0x240002, // [86]
		321, 324, 0x10003, // [87]
		348, 349, 0x120002, // [88]
		352, 353, 0x180002, // [89]
		364, 364, 0xf0002, // [90]
		350, 351, 0x10003, // [91]
		354, 354, 0xc0003, // [92]
		357, 357, 0x180003, // [93]
		355, 356, 0x10004, // [94]
		358, 358, 0xd0004, // [95]
		361, 361, 0x2c0004, // [96]
		359, 360, 0x10005, // [97]
	},
	NumStmt: [98]uint16{
		2, // 0
		15, // 1
		15, // 2
		15, // 3
		15, // 4
		1, // 5
		1, // 6
		1, // 7
		1, // 8
		2, // 9
		1, // 10
		7, // 11
		7, // 12
		7, // 13
		7, // 14
		7, // 15
		7, // 16
		2, // 17
		2, // 18
		2, // 19
		1, // 20
		2, // 21
		1, // 22
		1, // 23
		1, // 24
		1, // 25
		1, // 26
		1, // 27
		2, // 28
		1, // 29
		1, // 30
		2, // 31
		1, // 32
		2, // 33
		1, // 34
		1, // 35
		1, // 36
		1, // 37
		2, // 38
		2, // 39
		1, // 40
		1, // 41
		1, // 42
		1, // 43
		1, // 44
		1, // 45
		1, // 46
		2, // 47
		2, // 48
		1, // 49
		1, // 50
		1, // 51
		1, // 52
		2, // 53
		2, // 54
		5, // 55
		5, // 56
		3, // 57
		3, // 58
		4, // 59
		4, // 60
		4, // 61
		1, // 62
		1, // 63
		1, // 64
		3, // 65
		1, // 66
		1, // 67
		1, // 68
		1, // 69
		3, // 70
		1, // 71
		1, // 72
		1, // 73
		2, // 74
		1, // 75
		2, // 76
		1, // 77
		5, // 78
		5, // 79
		1, // 80
		1, // 81
		2, // 82
		1, // 83
		1, // 84
		4, // 85
		4, // 86
		3, // 87
		2, // 88
		2, // 89
		1, // 90
		1, // 91
		1, // 92
		1, // 93
		1, // 94
		1, // 95
		1, // 96
		1, // 97
	},
}

var _ = _cover_atomic_.LoadUint32
