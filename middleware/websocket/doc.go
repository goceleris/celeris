// Package websocket provides a zero-dependency native WebSocket middleware
// for celeris, implementing RFC 6455.
//
// Register the middleware on a route with [New] and a [Config] whose
// [Config.Handler] runs once per upgraded connection. The handler receives
// a [*Conn] and should block until the connection is done; it is closed
// automatically when the handler returns. Non-WebSocket requests pass
// through to the next handler, and HTTP/2 requests get 426 Upgrade Required
// (hijacking is impossible over multiplexed streams).
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
//
// On [Conn], [Conn.ReadMessage] / [Conn.WriteMessage] (plus the
// [Conn.WriteText], [Conn.WriteBinary], [Conn.ReadJSON], and [Conn.WriteJSON]
// helpers) cover the common case. [Conn.ReadMessageReuse] returns a buffer
// valid only until the next read for zero-alloc echo/proxy loops, while
// [Conn.NextReader] and [Conn.NextWriter] stream large messages without
// buffering them whole. [Conn.WriteControl], [Conn.WritePing], and the
// [Conn.SetPingHandler] / [Conn.SetPongHandler] / [Conn.SetCloseHandler]
// hooks handle keepalive and shutdown. All write methods are internally
// serialized and safe for concurrent use; a single goroutine may read while
// others write.
//
// [Config] options control origin checking ([Config.CheckOrigin], default
// same-origin), subprotocol negotiation ([Config.Subprotocols]),
// permessage-deflate compression ([Config.EnableCompression], RFC 7692),
// idle timeouts, and backpressure tuning for the native-engine path. Route
// params, query values, and headers captured at upgrade time are available
// via [Conn.Param], [Conn.Query], and [Conn.Header].
//
// The same Handler works on every engine. On the std engine the connection
// is hijacked for direct I/O; on native engines (epoll, io_uring) it stays
// in the event loop with TCP-level backpressure. [Conn.BackpressureDropped]
// returns 0 in healthy operation.
//
// For broadcasting one message to many connections, register each [*Conn]
// with a [Hub] and publish via [Hub.Broadcast], [Hub.BroadcastFilter], or
// [Hub.BroadcastPrepared] with a [PreparedMessage] (encoded once, reused
// across subscribers). Slow connections are handled by [HubConfig.OnSlowConn]
// returning a [HubPolicy]. Authorization must happen before [Hub.Register].
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/websocket
package websocket
