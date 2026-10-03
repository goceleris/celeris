package websocket

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strconv"
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

		// Capture request metadata before upgrade. On the native engines
		// reqHeaders is the stream's own header slice, and Context.Detach
		// (in tryEngineUpgrade) replaces its entries with clones in place,
		// so the Conn never keeps a header view.
		reqHeaders := c.RequestHeaders()
		acceptKey := computeAcceptKey(wsKey)

		// Try engine-integrated path first (native engines: epoll/io_uring).
		// On native engines, the handler runs on the event loop thread.
		// We must spawn a goroutine for the blocking handler and return
		// immediately to free the event loop.
		ws, done := tryEngineUpgrade(c, acceptKey, subproto, readBufSize, readLimit, compress,
			cfg.MaxBackpressureBuffer, cfg.BackpressureHighPct, cfg.BackpressureLowPct)

		// The query is captured only now (celeris#714). On the engine path
		// tryEngineUpgrade has detached, and Detach copied the raw query and
		// any query already parsed from it, so the pairs are copies without
		// cloning them again. On the hijack path nothing has been hijacked
		// yet, and captureQuery clones them itself.
		queryParams := captureQuery(c, ws == nil)
		// The route params likewise (celeris#721): Detach copied them on the
		// engine path; captureParams clones them on the hijack path.
		routeParams := captureParams(c, ws == nil)

		if ws != nil {
			// Engine path: populate conn and run handler in goroutine.
			// No net.Conn on the engine path to ask for the peer: keep the
			// address the engine reported for the connection (celeris#721).
			// Set before setupConn, which derives the Conn's IP from it.
			ws.remote = peerAddr(c.RemoteAddr())
			setupConn(ws, &cfg, compress,
				reqHeaders, queryParams)
			ws.params = routeParams
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
		ws.params = routeParams

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
		if err != nil {
			ws.writeErr.Store(storedWriteErr{err})
		}
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
	ws.cachedIP = ipOf(ws.RemoteAddr())
	ws.idleTimeout = cfg.IdleTimeout
	// WriteBufferPool: only consulted on the hijack (std) path. The native
	// engine path uses the engine's internal write buffer pool (cs.writeBuf)
	// and ignores this setting — see Conn.getWriter().
	ws.writePool = cfg.WriteBufferPool
}

// captureParams returns the upgrade request's route params for [Conn.Param]:
// one pair per named parameter (":room") and catch-all ("*path") in the
// matched route pattern, c.FullPath(), whose names the router owns. Before
// celeris#721 nothing captured them and Conn.Param always returned "".
//
// The values are views of the receive buffer on epoll and io_uring, which
// the engine keeps receiving WebSocket frames into (see captureQuery). After
// Context.Detach they are copies (Detach copies the params), and clone is
// false; on the hijack path clone is true and each value is cloned.
//
// The names are parsed from the pattern by the rule the router splits it
// with (router_tree.go, splitPath and findSegmentEnd): ':' up to the next
// '/', '*' to the end. Context has no accessor for its params' names, so a
// change to that syntax must change this loop too.
func captureParams(c *celeris.Context, clone bool) [][2]string {
	pattern := c.FullPath()
	var out [][2]string
	for i := 0; i < len(pattern); i++ {
		var name string
		switch pattern[i] {
		case ':':
			j := i + 1
			for j < len(pattern) && pattern[j] != '/' {
				j++
			}
			name, i = pattern[i+1:j], j
		case '*':
			name, i = pattern[i+1:], len(pattern)
		default:
			continue
		}
		v := c.Param(name)
		if clone {
			v = strings.Clone(v)
		}
		out = append(out, [2]string{name, v})
	}
	return out
}

// peerAddr turns the peer address the engine reported for the connection
// ("ip:port") into the net.Addr [Conn.RemoteAddr] returns: a *net.TCPAddr,
// as the hijack path's net.Conn gives. The engines report an IPv4 peer of a
// dual-stack ("[::]:port") listener as "[a.b.c.d]:port", which
// netip.ParseAddrPort rejects, so the host and the port are then parsed on
// their own. An address that does not parse is kept as it is.
func peerAddr(s string) net.Addr {
	if s == "" {
		return nil
	}
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		host, port, serr := net.SplitHostPort(s)
		ip, ierr := netip.ParseAddr(host)
		p, perr := strconv.ParseUint(port, 10, 16)
		if serr != nil || ierr != nil || perr != nil {
			return rawAddr(s)
		}
		ap = netip.AddrPortFrom(ip, uint16(p))
	}
	return net.TCPAddrFromAddrPort(ap)
}

// rawAddr is a peer address kept as the engine reported it.
type rawAddr string

func (rawAddr) Network() string  { return "tcp" }
func (a rawAddr) String() string { return string(a) }

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
func captureQuery(c *celeris.Context, clone bool) [][2]string {
	qp := c.QueryParams()
	if len(qp) == 0 {
		return nil
	}
	result := make([][2]string, 0, len(qp))
	for k, vs := range qp {
		if clone {
			k = strings.Clone(k)
		}
		for _, v := range vs {
			if clone {
				v = strings.Clone(v)
			}
			result = append(result, [2]string{k, v})
		}
	}
	return result
}
