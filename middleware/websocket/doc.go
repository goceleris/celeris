// Package websocket provides a zero-dependency native WebSocket middleware
// for celeris, implementing RFC 6455.
//
// # Basic Echo Server
//
//	server.GET("/ws", websocket.New(websocket.Config{
//	    Handler: func(c *websocket.Conn) {
//	        for {
//	            mt, msg, err := c.ReadMessage()
//	            if err != nil {
//	                break
//	            }
//	            if err := c.WriteMessage(mt, msg); err != nil {
//	                break
//	            }
//	        }
//	    },
//	}))
//
// # Origin Checking
//
// By default, a same-origin check is enforced (the Origin header must
// match the Host header). To allow all origins:
//
//	websocket.New(websocket.Config{
//	    CheckOrigin: func(c *celeris.Context) bool { return true },
//	    Handler:     myHandler,
//	})
//
// To restrict to specific origins:
//
//	websocket.New(websocket.Config{
//	    CheckOrigin: func(c *celeris.Context) bool {
//	        return c.Header("origin") == "https://example.com"
//	    },
//	    Handler: myHandler,
//	})
//
// # JSON Messaging
//
//	websocket.New(websocket.Config{
//	    Handler: func(c *websocket.Conn) {
//	        var msg MyType
//	        if err := c.ReadJSON(&msg); err != nil {
//	            return
//	        }
//	        c.WriteJSON(msg)
//	    },
//	})
//
// # Subprotocol Negotiation
//
//	websocket.New(websocket.Config{
//	    Subprotocols: []string{"graphql-transport-ws"},
//	    Handler: func(c *websocket.Conn) {
//	        proto := c.Subprotocol()
//	        // handle based on negotiated protocol
//	    },
//	})
//
// # HTTP/2 Limitation
//
// WebSocket requires HTTP/1.1 connection hijacking. HTTP/2 multiplexes
// streams over a single TCP connection, making hijack impossible. This
// middleware returns 426 Upgrade Required for HTTP/2 requests.
//
// # Concurrency
//
// All write methods ([Conn.WriteMessage], [Conn.WriteText], [Conn.WriteBinary],
// [Conn.WriteJSON], [Conn.WritePing]) are internally serialized and safe for
// concurrent use from multiple goroutines. A single goroutine may call
// [Conn.ReadMessage] while others write concurrently.
//
// [Conn.SetPingHandler], [Conn.SetPongHandler], and [Conn.SetCloseHandler]
// must be called before starting the read loop.
//
// # Keepalive (Ping/Pong)
//
// Detect dead connections with periodic pings:
//
//	Handler: func(c *websocket.Conn) {
//	    c.SetPongHandler(func(data []byte) error {
//	        c.SetReadDeadline(time.Now().Add(60 * time.Second))
//	        return nil
//	    })
//	    c.SetReadDeadline(time.Now().Add(60 * time.Second))
//
//	    go func() {
//	        ticker := time.NewTicker(30 * time.Second)
//	        defer ticker.Stop()
//	        for range ticker.C {
//	            if err := c.WritePing(nil); err != nil {
//	                return
//	            }
//	        }
//	    }()
//
//	    for {
//	        mt, msg, err := c.ReadMessage()
//	        if err != nil { break }
//	        c.WriteMessage(mt, msg)
//	    }
//	},
//
// # Compression (permessage-deflate)
//
// Enable RFC 7692 permessage-deflate compression:
//
//	websocket.New(websocket.Config{
//	    EnableCompression: true,
//	    Handler:           myHandler,
//	})
//
// Compression is negotiated during the upgrade handshake. Messages above
// the compression threshold (default 128 bytes) are compressed transparently.
//
// # Streaming Large Messages
//
// For large messages, use [Conn.NextReader] and [Conn.NextWriter] to avoid
// buffering the entire message in memory:
//
//	mt, reader, _ := c.NextReader()
//	io.Copy(dst, reader)
//
//	writer, _ := c.NextWriter(websocket.TextMessage)
//	writer.Write(chunk1)
//	writer.Write(chunk2)
//	writer.Close()
//
// # ReadMessage vs ReadMessageReuse
//
// [Conn.ReadMessage] returns an owned copy (safe to retain).
// [Conn.ReadMessageReuse] returns a reused buffer (zero-alloc, only valid
// until the next read call). Use ReadMessageReuse for echo servers and
// message-forwarding proxies where the data is processed immediately.
//
// # Access Request Data
//
// Route params, query params, and headers are captured at upgrade time:
//
//	// Route: /ws/:room
//	c.Param("room")      // route parameter
//	c.Query("token")     // query parameter
//	c.Header("origin")   // request header
//
// # Engine-integrated mode vs hijack mode
//
// On the std engine, WebSocket connections are upgraded via Go's standard
// connection hijacking — the handler runs on a goroutine and reads/writes
// directly on a *net.TCPConn. On the native engines (epoll, io_uring), the
// connection stays in the event loop after upgrade: inbound chunks are
// delivered to the handler goroutine via an internal chanReader (which
// applies TCP-level backpressure on overflow), and outbound writes go
// through the engine's per-connection write buffer (cs.writeBuf).
//
// The same Handler signature works for both modes — the middleware picks
// the engine-integrated path automatically when the engine supports it
// and falls back to hijack on the std engine.
//
// # Backpressure semantics
//
// On the engine path, the WebSocket conn maintains a bounded chanReader
// between the event loop and the handler goroutine. When the buffer fills
// past the high-water mark (75% of [Config.MaxBackpressureBuffer]), the
// engine pauses inbound delivery for that connection (epoll: drops
// EPOLLIN; io_uring: cancels the in-flight RECV). The kernel then closes
// the TCP receive window, slowing the peer at the network level. When the
// buffer drains below 25%, the engine resumes inbound delivery.
//
// On the std (hijack) path, backpressure is handled directly by the
// kernel's TCP stack via [net.Conn.Read] returning when the kernel buffer
// has data — no middleware-level buffering happens.
//
// In healthy operation [Conn.BackpressureDropped] returns 0. A non-zero
// value indicates the engine pause/resume mechanism is malfunctioning.
//
// # IdleTimeout semantics
//
// On the std (hijack) path, [Config.IdleTimeout] is enforced via
// [net.Conn.SetReadDeadline], which is reset before each blocking read.
// On the engine path, the WS middleware extends an absolute deadline via
// [Context.SetWSIdleDeadline] after each successful frame read, and the
// engine's idle sweep closes connections whose deadline has expired.
// Both paths converge to the same observable behavior.
//
// # WriteControl deadline semantics
//
// [Conn.WriteControl] applies the supplied deadline to the channel-based
// write semaphore (so a stalled large NextWriter cannot indefinitely block
// pings/pongs). On the std path, the deadline is also pinned to the
// underlying [net.Conn] via [net.Conn.SetWriteDeadline] so a peer that
// has stopped reading cannot stall the actual flush. On the engine path,
// writes go into the engine's write buffer and never block at the
// syscall level — only the lock-acquisition deadline applies.
//
// # Fan-out (Hub)
//
// For broadcasting a single message to N connections, use [Hub] with
// [PreparedMessage]. The frame is encoded once per uncompressed /
// compressed variant and reused across every [Conn.WritePreparedMessage]
// dispatch — so per-message wire-encoding cost is O(1) regardless of
// subscriber count, while per-Conn write throughput remains the
// engine's normal write path.
//
//	hub := websocket.NewHub(websocket.HubConfig{
//	    OnSlowConn: func(c *websocket.Conn, err error) websocket.HubPolicy {
//	        return websocket.HubPolicyClose // boot misbehaving peers
//	    },
//	})
//	server.GET("/ws", websocket.New(websocket.Config{
//	    Handler: func(c *websocket.Conn) {
//	        unregister := hub.Register(c)
//	        defer unregister()
//	        // your read loop here
//	    },
//	}))
//	// Publishers anywhere in the app:
//	hub.Broadcast(websocket.TextMessage, []byte(`{"type":"tick"}`))
//
// Hub.Close drains every in-flight Broadcast (via an internal
// inflight WaitGroup) before tearing down conns, so a shutdown that
// synchronises on Close cannot race a still-fanning-out message.
//
// Authorization MUST happen before [Hub.Register]. Hub broadcasts go
// to every registered connection unfiltered; if a per-conn ACL is
// required, use [Hub.BroadcastFilter] with a pure predicate.
//
// PreparedMessage rejects control opcodes (Ping/Pong/Close) at
// construction time — control frames have RFC 6455 §5.5 size and
// fragmentation constraints that the cache-and-reuse model can't
// satisfy. Use [Conn.WriteControl] per-conn for those.
//
// See also: middleware/sse for the equivalent on Server-Sent Events,
// where the same broker pattern lives as [middleware/sse.Broker].
package websocket
