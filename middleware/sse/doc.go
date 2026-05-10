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
// For automatic replay see [Config.ReplayStore] + [NewRingBuffer] /
// [NewKVReplayStore]: the middleware will Append every Send and replay
// missed events on the next reconnect with no work from the handler.
//
// # Backpressure: two layers, two purposes
//
// celeris exposes per-subscriber backpressure at TWO distinct layers:
//
//   - [Config.MaxQueueDepth] / [Config.OnSlowClient] is the per-Client
//     queue. When set, [Client.Send] enqueues onto a bounded channel
//     that a per-Client drain goroutine writes to the wire. The user's
//     [Config.OnSlowClient] policy fires when that queue overflows.
//     Use this layer when a single subscriber drives the load — e.g.
//     a long-lived per-user feed where Send is the only producer.
//
//   - [Broker] / [BrokerConfig.SubscriberBuffer] / [OnSlowSubscriber]
//     is the per-subscriber queue INSIDE the broker. [Broker.Publish]
//     fans out via these queues; per-subscriber drain goroutines call
//     [Client.WritePreparedEvent] (which writes synchronously to the
//     wire, bypassing Client.MaxQueueDepth). Use this layer for
//     fan-out from a single publisher to N subscribers — the broker
//     formats each event ONCE and never blocks on a single slow
//     subscriber.
//
// The two layers are orthogonal. A Broker.Subscribe'd client can also
// have a non-zero MaxQueueDepth — the broker uses WritePreparedEvent
// (which is always synchronous) so the Client.queue lies dormant
// unless something else calls [Client.Send] directly. In that case
// the Client.queue absorbs Send-bursts independently of the broker
// fan-out. There is no scenario where both queues fight each other.
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
