//line middleware/websocket/websocket.go:1:1
package websocket; import _cover_atomic_ "sync/atomic"

import (
	"context"
	"io"
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
func New(config ...Config) celeris.HandlerFunc {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[0], 1);
	cfg := defaultConfig
	if len(config) > 0 {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[5], 1);
		cfg = config[0]
	}
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[1], 1);cfg = applyDefaults(cfg)
	cfg.validate()

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[2], 1);var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[3], 1);handler := cfg.Handler
	onConnect := cfg.OnConnect
	onDisconnect := cfg.OnDisconnect
	checkOrigin := cfg.CheckOrigin
	subprotocols := cfg.Subprotocols
	readBufSize := cfg.ReadBufferSize
	writeBufSize := cfg.WriteBufferSize
	readLimit := cfg.ReadLimit
	handshakeTimeout := cfg.HandshakeTimeout
	enableCompression := cfg.EnableCompression

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[4], 1);return func(c *celeris.Context) error {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[6], 1);
		if skip.ShouldSkip(c) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[21], 1);
			return c.Next()
		}

		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[7], 1);if !c.IsWebSocket() {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[22], 1);
			return c.Next()
		}

		// HTTP/2 cannot be hijacked.
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[8], 1);if c.Protocol() == "2" {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[23], 1);
			return celeris.NewHTTPError(426, "websocket: upgrade not supported over HTTP/2")
		}

		// Validate the upgrade request.
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[9], 1);wsKey, err := validateUpgrade(c)
		if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[24], 1);
			return celeris.NewHTTPError(400, err.Error())
		}

		// Origin check (default: same-origin).
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[10], 1);if checkOrigin != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[25], 1);
			if !checkOrigin(c) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[26], 1);
				return celeris.NewHTTPError(403, "websocket: origin not allowed")
			}
		} else{ _cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[27], 1);{
			// Default same-origin check: Origin must match Host. Browser
			// clients always send Origin on cross-origin upgrade attempts;
			// missing Origin on https:// is treated as a CSRF-class
			// signal and rejected. Non-browser clients that legitimately
			// omit Origin should set an explicit [Config.CheckOrigin].
			origin := c.Header("origin")
			if origin == "" {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[28], 1);
				if c.Scheme() == "https" {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[29], 1);
					return celeris.NewHTTPError(403, "websocket: missing Origin on https — set Config.CheckOrigin to allow")
				}
				// Plain http: keep the legacy permissive behavior; loopback
				// dev tools and CLI clients commonly omit Origin and would
				// break otherwise.
			} else{ _cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[30], 1);{
				host := c.Host()
				if !checkSameOrigin(origin, host) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[31], 1);
					return celeris.NewHTTPError(403, "websocket: origin not allowed")
				}
			}}
		}}

		// Negotiate subprotocol.
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[11], 1);subproto := negotiateSubprotocol(
			c.Header("sec-websocket-protocol"),
			subprotocols,
		)

		// Negotiate compression.
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[12], 1);compress := negotiateCompression(
			c.Header("sec-websocket-extensions"),
			enableCompression,
		)

		// Capture request metadata before upgrade.
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[13], 1);reqHeaders := c.RequestHeaders()
		queryParams := captureQuery(c)
		acceptKey := computeAcceptKey(wsKey)

		// Try engine-integrated path first (native engines: epoll/io_uring).
		// On native engines, the handler runs on the event loop thread.
		// We must spawn a goroutine for the blocking handler and return
		// immediately to free the event loop.
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[14], 1);ws, done := tryEngineUpgrade(c, acceptKey, subproto, readBufSize, readLimit, compress,
			cfg.MaxBackpressureBuffer, cfg.BackpressureHighPct, cfg.BackpressureLowPct)

		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[15], 1);if ws != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[32], 1);
			// Engine path: populate conn and run handler in goroutine.
			setupConn(ws, &cfg, compress,
				reqHeaders, queryParams)
			go func() {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[34], 1);
				defer func() {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[37], 1);
					ws.fragWriting.Store(false) // clear in case handler panicked mid-NextWriter
					if !ws.closeSent.Load() {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[40], 1);
						_ = ws.writeCloseFrame(CloseNormalClosure, "")
					}
					_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[38], 1);_ = ws.Close()
					if onDisconnect != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[41], 1);
						onDisconnect(ws)
					}
					_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[39], 1);if done != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[42], 1);
						done()
					}
				}()
				_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[35], 1);if onConnect != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[43], 1);
					if err := onConnect(ws); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[44], 1);
						return
					}
				}
				_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[36], 1);handler(ws)
			}()
			_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[33], 1);return nil // free the event loop thread
		}

		// Hijack path (std engine): handler blocks in this goroutine.
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[16], 1);ws, err = hijackUpgrade(c, acceptKey, subproto, readBufSize, writeBufSize, readLimit, handshakeTimeout, compress)
		if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[45], 1);
			return err
		}

		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[17], 1);setupConn(ws, &cfg, compress,
			reqHeaders, queryParams)

		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[18], 1);defer func() {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[46], 1);
			ws.fragWriting.Store(false) // clear in case handler panicked mid-NextWriter
			if !ws.closeSent.Load() {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[48], 1);
				_ = ws.writeCloseFrame(CloseNormalClosure, "")
			}
			_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[47], 1);_ = ws.Close()
			if onDisconnect != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[49], 1);
				onDisconnect(ws)
			}
		}()

		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[19], 1);if onConnect != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[50], 1);
			if err := onConnect(ws); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[51], 1);
				return nil
			}
		}

		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[20], 1);handler(ws)
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
	backpressure, highPct, lowPct int) (*Conn, func()) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[52], 1);

	reader := newChanReader(backpressure, highPct, lowPct)

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[53], 1);if !c.UpgradeWebSocket(func(data []byte) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[64], 1);
		// Called on the event loop thread — must NOT block.
		// COPY first because the engine reuses its read buffer after the
		// callback returns; chanReader stores the slice as-is.
		cp := make([]byte, len(data))
		copy(cp, data)
		reader.Append(cp)
	}) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[65], 1);
		return nil, nil
	}

	// Pre-construct the Conn shell so the error handler installed
	// BEFORE Detach has a stable place to record write errors. After
	// Detach, the engine may immediately call OnError (e.g. a pre-
	// existing peer RST race window between UpgradeWebSocket and the
	// caller-visible Conn); installing the handler ahead of Detach
	// avoids losing that error.
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[54], 1);ctx, cancel := context.WithCancel(c.Context())
	ws := newEngineConn(ctx, cancel, reader, nil, readBufSize) // rawWrite filled after Detach
	ws.readLimit = readLimit
	ws.subprotocol = subproto

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[55], 1);c.SetWSErrorHandler(func(err error) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[66], 1);
		if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[68], 1);
			ws.writeErr.Store(storedWriteErr{err})
		}
		_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[67], 1);reader.closeWith(err)
	})

	// Detach installs the guarded writeFn and engine pause/resume hooks.
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[56], 1);done := c.Detach()

	// Get the raw write function (bypasses chunked encoding) and finish
	// wiring it into the Conn.
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[57], 1);rawWrite := c.WSRawWriteFn()
	if rawWrite == nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[69], 1);
		reader.closeWith(io.EOF)
		done()
		return nil, nil
	}
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[58], 1);ws.setRawWrite(rawWrite)

	// Write the 101 response using raw write (no chunked encoding).
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[59], 1);resp := buildUpgradeResponse(acceptKey, subproto, compress)
	rawWrite(resp)

	// Wire engine pause/resume into the chanReader for TCP-level
	// backpressure. The engine applies pause/resume asynchronously via
	// its detach queue.
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[60], 1);if pause, resume := c.WSReadPauser(); pause != nil && resume != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[70], 1);
		reader.SetPauser(pause, resume)
	}

	// Idle timeout on the engine path: store a closure the WS conn calls
	// after each successful frame read to extend the deadline.
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[61], 1);ws.idleDeadlineFn = func(ns int64) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[71], 1);
		c.SetWSIdleDeadline(ns)
	}

	// Tell the engine to close the chanReader / wake the handler goroutine
	// when it tears down this detached connection.
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[62], 1);c.SetWSDetachClose(func() {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[72], 1);
		_ = ws.Close()
	})

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[63], 1);return ws, done
}

// hijackUpgrade performs the traditional hijack-based WebSocket upgrade.
func hijackUpgrade(c *celeris.Context, acceptKey, subproto string,
	readBufSize, writeBufSize int, readLimit int64, timeout time.Duration, compress bool) (*Conn, error) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[73], 1);

	rawConn, err := c.Hijack()
	if err != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[79], 1);
		return nil, celeris.NewHTTPError(500, "websocket: hijack failed: "+err.Error())
	}

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[74], 1);if timeout > 0 {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[80], 1);
		_ = rawConn.SetDeadline(time.Now().Add(timeout))
	}

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[75], 1);resp := buildUpgradeResponse(acceptKey, subproto, compress)
	if _, err := rawConn.Write(resp); err != nil {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[81], 1);
		_ = rawConn.Close()
		return nil, err
	}

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[76], 1);if timeout > 0 {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[82], 1);
		_ = rawConn.SetDeadline(time.Time{})
	}

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[77], 1);ctx, cancel := context.WithCancel(c.Context())
	ws := newConn(ctx, cancel, rawConn, readBufSize, writeBufSize)
	ws.readLimit = readLimit
	ws.subprotocol = subproto

	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[78], 1);return ws, nil
}

func setupConn(ws *Conn, cfg *Config, compress bool,
	headers [][2]string, query [][2]string) {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[83], 1);
	if compress {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[86], 1);
		ws.compressEnabled = true
		ws.compressLevel = cfg.CompressionLevel
		ws.compressThreshold = cfg.CompressionThreshold
	}
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[84], 1);ws.headers = headers
	ws.query = query
	ws.idleTimeout = cfg.IdleTimeout
	// WriteBufferPool: only consulted on the hijack (std) path. The native
	// engine path uses the engine's internal write buffer pool (cs.writeBuf)
	// and ignores this setting — see Conn.getWriter().
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[85], 1);ws.writePool = cfg.WriteBufferPool
}

func captureQuery(c *celeris.Context) [][2]string {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[87], 1);
	qp := c.QueryParams()
	if len(qp) == 0 {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[90], 1);
		return nil
	}
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[88], 1);result := make([][2]string, 0, len(qp))
	for k, vs := range qp {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[91], 1);
		for _, v := range vs {_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[92], 1);
			result = append(result, [2]string{k, v})
		}
	}
	_cover_atomic_.AddUint32(&GoCover_c633_websocket.Count[89], 1);return result
}

var GoCover_c633_websocket = struct {
	Count     [93]uint32
	Pos       [3 * 93]uint32
	NumStmt   [93]uint16
} {
	Pos: [3 * 93]uint32{
		38, 39, 0x150002, // [0]
		42, 44, 0x10002, // [1]
		45, 47, 0x10002, // [2]
		48, 58, 0x10002, // [3]
		59, 59, 0x280002, // [4]
		40, 41, 0x10003, // [5]
		60, 60, 0x190003, // [6]
		64, 64, 0x170003, // [7]
		69, 69, 0x1a0003, // [8]
		74, 75, 0x110003, // [9]
		80, 80, 0x190003, // [10]
		107, 111, 0x10003, // [11]
		113, 117, 0x10003, // [12]
		119, 122, 0x10003, // [13]
		127, 129, 0x10003, // [14]
		130, 130, 0x100003, // [15]
		159, 160, 0x110003, // [16]
		164, 166, 0x10003, // [17]
		167, 167, 0x100003, // [18]
		178, 178, 0x170003, // [19]
		184, 185, 0xd0003, // [20]
		61, 62, 0x10004, // [21]
		65, 66, 0x10004, // [22]
		70, 71, 0x10004, // [23]
		76, 77, 0x10004, // [24]
		81, 81, 0x170004, // [25]
		82, 83, 0x10005, // [26]
		90, 91, 0x140004, // [27]
		92, 92, 0x1e0005, // [28]
		93, 94, 0x10006, // [29]
		99, 100, 0x270005, // [30]
		101, 102, 0x10006, // [31]
		132, 134, 0xe0004, // [32]
		155, 155, 0xe0004, // [33]
		135, 135, 0x120005, // [34]
		148, 148, 0x190005, // [35]
		153, 153, 0x100005, // [36]
		136, 137, 0x1e0006, // [37]
		140, 141, 0x1d0006, // [38]
		144, 144, 0x150006, // [39]
		138, 139, 0x10007, // [40]
		142, 143, 0x10007, // [41]
		145, 146, 0x10007, // [42]
		149, 149, 0x2a0006, // [43]
		150, 151, 0x10007, // [44]
		161, 162, 0x10004, // [45]
		168, 169, 0x1c0004, // [46]
		172, 173, 0x1b0004, // [47]
		170, 171, 0x10005, // [48]
		174, 175, 0x10005, // [49]
		179, 179, 0x280004, // [50]
		180, 181, 0x10005, // [51]
		207, 208, 0x10002, // [52]
		209, 209, 0x2b0002, // [53]
		226, 230, 0x10002, // [54]
		231, 231, 0x260002, // [55]
		239, 240, 0x10002, // [56]
		243, 244, 0x150002, // [57]
		249, 250, 0x10002, // [58]
		252, 254, 0x10002, // [59]
		258, 258, 0x460002, // [60]
		264, 264, 0x250002, // [61]
		270, 270, 0x1c0002, // [62]
		274, 274, 0x110002, // [63]
		213, 216, 0x10003, // [64]
		217, 218, 0x10003, // [65]
		232, 232, 0x110003, // [66]
		235, 235, 0x180003, // [67]
		233, 234, 0x10004, // [68]
		245, 248, 0x10003, // [69]
		259, 260, 0x10003, // [70]
		265, 266, 0x10003, // [71]
		271, 272, 0x10003, // [72]
		281, 282, 0x100002, // [73]
		286, 286, 0x110002, // [74]
		290, 291, 0x2f0002, // [75]
		296, 296, 0x110002, // [76]
		300, 304, 0x10002, // [77]
		305, 305, 0x100002, // [78]
		283, 284, 0x10003, // [79]
		287, 288, 0x10003, // [80]
		292, 294, 0x10003, // [81]
		297, 298, 0x10003, // [82]
		310, 310, 0xe0002, // [83]
		315, 318, 0x10002, // [84]
		321, 321, 0x240002, // [85]
		311, 314, 0x10003, // [86]
		325, 326, 0x120002, // [87]
		329, 330, 0x180002, // [88]
		335, 335, 0xf0002, // [89]
		327, 328, 0x10003, // [90]
		331, 331, 0x180003, // [91]
		332, 333, 0x10004, // [92]
	},
	NumStmt: [93]uint16{
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
		2, // 16
		2, // 17
		2, // 18
		1, // 19
		2, // 20
		1, // 21
		1, // 22
		1, // 23
		1, // 24
		1, // 25
		1, // 26
		2, // 27
		1, // 28
		1, // 29
		2, // 30
		1, // 31
		2, // 32
		1, // 33
		1, // 34
		1, // 35
		1, // 36
		2, // 37
		2, // 38
		1, // 39
		1, // 40
		1, // 41
		1, // 42
		1, // 43
		1, // 44
		1, // 45
		2, // 46
		2, // 47
		1, // 48
		1, // 49
		1, // 50
		1, // 51
		2, // 52
		2, // 53
		5, // 54
		5, // 55
		3, // 56
		3, // 57
		4, // 58
		4, // 59
		4, // 60
		1, // 61
		1, // 62
		1, // 63
		3, // 64
		1, // 65
		1, // 66
		1, // 67
		1, // 68
		3, // 69
		1, // 70
		1, // 71
		1, // 72
		2, // 73
		1, // 74
		2, // 75
		1, // 76
		5, // 77
		5, // 78
		1, // 79
		1, // 80
		2, // 81
		1, // 82
		1, // 83
		4, // 84
		4, // 85
		3, // 86
		2, // 87
		2, // 88
		1, // 89
		1, // 90
		1, // 91
		1, // 92
	},
}

var _ = _cover_atomic_.LoadUint32
