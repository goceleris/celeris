// Package sse provides Server-Sent Events (SSE) middleware for celeris.
//
// SSE enables unidirectional server-to-client streaming over HTTP. This
// middleware manages event formatting, heartbeat keep-alives, reconnection
// via Last-Event-ID, and client disconnect detection.
//
// # Basic Usage
//
//	server.GET("/events", sse.New(sse.Config{
//	    Handler: func(client *sse.Client) {
//	        ticker := time.NewTicker(time.Second)
//	        defer ticker.Stop()
//	        for {
//	            select {
//	            case <-client.Context().Done():
//	                return
//	            case t := <-ticker.C:
//	                if err := client.Send(sse.Event{
//	                    Event: "time",
//	                    Data:  t.Format(time.RFC3339),
//	                }); err != nil {
//	                    return
//	                }
//	            }
//	        }
//	    },
//	}))
//
// # Reconnection
//
// When a client reconnects, browsers send a Last-Event-ID header. Use
// [Client.LastEventID] to resume from where the client left off:
//
//	Handler: func(client *sse.Client) {
//	    lastID := client.LastEventID()
//	    events := fetchEventsSince(lastID)
//	    for _, e := range events {
//	        client.Send(e)
//	    }
//	},
//
// # Heartbeat
//
// By default, a heartbeat comment is sent every 15 seconds to detect
// disconnected clients. Configure via [Config.HeartbeatInterval] or
// disable with a negative value.
//
// # Engine Compatibility
//
// SSE works with all celeris engines (std, epoll, io_uring). The middleware
// handles [celeris.Context.Detach] internally — callers do not need to manage
// the event loop lifecycle.
//
// # CORS
//
// For cross-origin EventSource connections, configure CORS middleware on
// the SSE endpoint to allow the appropriate origin.
package sse
