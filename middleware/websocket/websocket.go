package websocket

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
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	handler := cfg.Handler
	onConnect := cfg.OnConnect
	onDisconnect := cfg.OnDisconnect
	checkOrigin := cfg.CheckOrigin
	subprotocols := cfg.Subprotocols
	readBufSize := cfg.ReadBufferSize
	writeBufSize := cfg.WriteBufferSize
	readLimit := cfg.ReadLimit
	handshakeTimeout := cfg.HandshakeTimeout
	enableCompression := cfg.EnableCompression

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		if !c.IsWebSocket() {
			return c.Next()
		}

		// HTTP/2 cannot be hijacked.
		if c.Protocol() == "2" {
			return celeris.NewHTTPError(426, "websocket: upgrade not supported over HTTP/2")
		}

		// Validate the upgrade request.
		wsKey, err := validateUpgrade(c)
		if err != nil {
			return celeris.NewHTTPError(400, err.Error())
		}

		// Origin check (default: same-origin).
		if checkOrigin != nil {
			if !checkOrigin(c) {
				return celeris.NewHTTPError(403, "websocket: origin not allowed")
			}
		} else {
			// Default same-origin check: Origin must match Host. Browser
			// clients always send Origin on cross-origin upgrade attempts;
			// missing Origin on https:// is treated as a CSRF-class
			// signal and rejected. Non-browser clients that legitimately
			// omit Origin should set an explicit [Config.CheckOrigin].
			origin := c.Header("origin")
			if origin == "" {
				if c.Scheme() == "https" {
					return celeris.NewHTTPError(403, "websocket: missing Origin on https — set Config.CheckOrigin to allow")
				}
				// Plain http: keep the legacy permissive behavior; loopback
				// dev tools and CLI clients commonly omit Origin and would
				// break otherwise.
			} else {
				host := c.Host()
				if !checkSameOrigin(origin, host) {
					return celeris.NewHTTPError(403, "websocket: origin not allowed")
				}
			}
		}

		// Negotiate subprotocol.
		subproto := negotiateSubprotocol(
			c.Header("sec-websocket-protocol"),
			subprotocols,
		)

		// Negotiate compression.
		compress := negotiateCompression(
			c.Header("sec-websocket-extensions"),
			enableCompression,
		)

		// Capture request metadata before upgrade.
		reqHeaders := c.RequestHeaders()
		queryParams := captureQuery(c)
		acceptKey := computeAcceptKey(wsKey)

		// Try engine-integrated path first (native engines: epoll/io_uring).
		// On native engines, the handler runs on the event loop thread.
		// We must spawn a goroutine for the blocking handler and return
		// immediately to free the event loop.
		ws, done := tryEngineUpgrade(c, acceptKey, subproto, readBufSize, readLimit, compress,
			cfg.MaxBackpressureBuffer, cfg.BackpressureHighPct, cfg.BackpressureLowPct)

		if ws != nil {
			// Engine path: populate conn and run handler in goroutine.
			setupConn(ws, &cfg, compress,
				reqHeaders, queryParams)
			go func() {
				defer func() {
					ws.fragWriting.Store(false) // clear in case handler panicked mid-NextWriter
					if !ws.closeSent.Load() {
						_ = ws.writeCloseFrame(CloseNormalClosure, "")
					}
					_ = ws.Close()
					if onDisconnect != nil {
						onDisconnect(ws)
					}
					if done != nil {
						done()
					}
				}()
				if onConnect != nil {
					if err := onConnect(ws); err != nil {
						return
					}
				}
				handler(ws)
			}()
			return nil // free the event loop thread
		}

		// Hijack path (std engine): handler blocks in this goroutine.
		ws, err = hijackUpgrade(c, acceptKey, subproto, readBufSize, writeBufSize, readLimit, handshakeTimeout, compress)
		if err != nil {
			return err
		}

		setupConn(ws, &cfg, compress,
			reqHeaders, queryParams)

		defer func() {
			ws.fragWriting.Store(false) // clear in case handler panicked mid-NextWriter
			if !ws.closeSent.Load() {
				_ = ws.writeCloseFrame(CloseNormalClosure, "")
			}
			_ = ws.Close()
			if onDisconnect != nil {
				onDisconnect(ws)
			}
		}()

		if onConnect != nil {
			if err := onConnect(ws); err != nil {
				return nil
			}
		}

		handler(ws)
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
	backpressure, highPct, lowPct int) (*Conn, func()) {

	reader := newChanReader(backpressure, highPct, lowPct)

	if !c.UpgradeWebSocket(func(data []byte) {
		// Called on the event loop thread — must NOT block.
		// COPY first because the engine reuses its read buffer after the
		// callback returns; chanReader stores the slice as-is.
		cp := make([]byte, len(data))
		copy(cp, data)
		reader.Append(cp)
	}) {
		return nil, nil
	}

	// Pre-construct the Conn shell so the error handler installed
	// BEFORE Detach has a stable place to record write errors. After
	// Detach, the engine may immediately call OnError (e.g. a pre-
	// existing peer RST race window between UpgradeWebSocket and the
	// caller-visible Conn); installing the handler ahead of Detach
	// avoids losing that error.
	ctx, cancel := context.WithCancel(c.Context())
	ws := newEngineConn(ctx, cancel, reader, nil, readBufSize) // rawWrite filled after Detach
	ws.readLimit = readLimit
	ws.subprotocol = subproto

	c.SetWSErrorHandler(func(err error) {
		ws.writeErr.Store(err)
		reader.closeWith(err)
	})

	// Detach installs the guarded writeFn and engine pause/resume hooks.
	done := c.Detach()

	// Get the raw write function (bypasses chunked encoding) and finish
	// wiring it into the Conn.
	rawWrite := c.WSRawWriteFn()
	if rawWrite == nil {
		reader.closeWith(io.EOF)
		done()
		return nil, nil
	}
	ws.setRawWrite(rawWrite)

	// Write the 101 response using raw write (no chunked encoding).
	resp := buildUpgradeResponse(acceptKey, subproto, compress)
	rawWrite(resp)

	// Wire engine pause/resume into the chanReader for TCP-level
	// backpressure. The engine applies pause/resume asynchronously via
	// its detach queue.
	if pause, resume := c.WSReadPauser(); pause != nil && resume != nil {
		reader.SetPauser(pause, resume)
	}

	// Idle timeout on the engine path: store a closure the WS conn calls
	// after each successful frame read to extend the deadline.
	ws.idleDeadlineFn = func(ns int64) {
		c.SetWSIdleDeadline(ns)
	}

	// Tell the engine to close the chanReader / wake the handler goroutine
	// when it tears down this detached connection.
	c.SetWSDetachClose(func() {
		_ = ws.Close()
	})

	return ws, done
}

// hijackUpgrade performs the traditional hijack-based WebSocket upgrade.
func hijackUpgrade(c *celeris.Context, acceptKey, subproto string,
	readBufSize, writeBufSize int, readLimit int64, timeout time.Duration, compress bool) (*Conn, error) {

	rawConn, err := c.Hijack()
	if err != nil {
		return nil, celeris.NewHTTPError(500, "websocket: hijack failed: "+err.Error())
	}

	if timeout > 0 {
		_ = rawConn.SetDeadline(time.Now().Add(timeout))
	}

	resp := buildUpgradeResponse(acceptKey, subproto, compress)
	if _, err := rawConn.Write(resp); err != nil {
		_ = rawConn.Close()
		return nil, err
	}

	if timeout > 0 {
		_ = rawConn.SetDeadline(time.Time{})
	}

	ctx, cancel := context.WithCancel(c.Context())
	ws := newConn(ctx, cancel, rawConn, readBufSize, writeBufSize)
	ws.readLimit = readLimit
	ws.subprotocol = subproto

	return ws, nil
}

func setupConn(ws *Conn, cfg *Config, compress bool,
	headers [][2]string, query [][2]string) {
	if compress {
		ws.compressEnabled = true
		ws.compressLevel = cfg.CompressionLevel
		ws.compressThreshold = cfg.CompressionThreshold
	}
	ws.headers = headers
	ws.query = query
	ws.idleTimeout = cfg.IdleTimeout
	// WriteBufferPool: only consulted on the hijack (std) path. The native
	// engine path uses the engine's internal write buffer pool (cs.writeBuf)
	// and ignores this setting — see Conn.getWriter().
	ws.writePool = cfg.WriteBufferPool
}

func captureQuery(c *celeris.Context) [][2]string {
	qp := c.QueryParams()
	if len(qp) == 0 {
		return nil
	}
	result := make([][2]string, 0, len(qp))
	for k, vs := range qp {
		for _, v := range vs {
			result = append(result, [2]string{k, v})
		}
	}
	return result
}
