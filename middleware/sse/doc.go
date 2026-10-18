// Package sse provides Server-Sent Events (SSE) middleware for celeris.
//
// SSE enables unidirectional server-to-client streaming over HTTP. Use [New]
// with a [Config] to attach an SSE handler to any route. The [Config.Handler]
// field receives a [*Client] per connection; call [Client.Send] or
// [Client.SendData] to push [Event] values, and watch [Client.Context] for
// disconnect.
//
// For fan-out to many subscribers, use [NewBroker] ([BrokerConfig]): call
// [Broker.Subscribe] in each handler and [Broker.Publish] from any goroutine.
// [Broker.Publish] formats each event once via [PreparedEvent] and never
// blocks on a slow subscriber.
//
// Backpressure is controlled per-client via [Config.MaxQueueDepth] and
// [Config.OnSlowClient] (drop/close policy), and per-broker-subscriber via
// [BrokerConfig.SubscriberBuffer].
//
// Reconnection replay is available via [Config.ReplayStore]: supply
// [NewRingBuffer] for in-process replay or [NewKVReplayStore] for
// distributed replay backed by a KV store. The middleware calls
// [Client.LastEventID] and replays missed events automatically.
//
// Heartbeat comments are sent every [DefaultHeartbeatInterval] (15 s) by
// default; configure with [Config.HeartbeatInterval]. The middleware works
// with all celeris engines and handles stream lifecycle internally.
//
// An SSE stream is a detached stream, so the server's ReadTimeout does not end
// it as a whole on any engine and neither does WriteTimeout (celeris#855): it
// stays open for as long as the client keeps reading, without setting
// WriteTimeout to -1. A client that stays connected and stops reading is dealt
// with per engine:
//
//   - std: each write of the stream is bounded by WriteTimeout. A write that
//     has not finished within WriteTimeout fails, so [Client.Send] returns an
//     error and the stream ends (WriteTimeout -1 sets no such bound). For an
//     SSE event, which is small, that means the client has stopped reading.
//     Until then the handler, or the queue's drain goroutine, is parked in
//     that write, so returning or [ClientPolicyClose] cannot end the stream
//     sooner. When Shutdown's budget runs out, the writes of the streams still
//     running fail too.
//   - epoll, io_uring and adaptive: [Client.Send] does not wait for a client
//     that reads nothing (the write only queues the event in the engine), so
//     a write error is not what ends the stream of such a client, and
//     [ClientPolicyClose] ends it only earlier, when the queue overflows (a
//     drain goroutine that lags behind the producer). WriteTimeout is the
//     bound when it does not, as "no progress for WriteTimeout": a client
//     that takes none of the bytes the engine has queued for it for
//     WriteTimeout is disconnected, which cancels [Client.Context], and
//     [Config.OnDisconnect] runs. A client that keeps reading, however slowly
//     (there is no minimum rate), is not disconnected, and neither is one
//     with nothing queued for it (WriteTimeout -1 sets no such bound). A
//     handler that returns (its own bound, or its context) ends the stream
//     sooner, and so does Shutdown. The disconnect comes between about 3/4
//     and 5/4 of WriteTimeout after the client last took a byte (its progress
//     is sampled every quarter of WriteTimeout; a SEND completion restarts the
//     clock at once), plus the engine's sweep interval (up to about 0.1 s on
//     epoll, up to 2 s on io_uring; a WriteTimeout shorter than that can
//     double). This is for HTTP/1.1 streams: an h2c stream is not covered
//     yet (celeris#884).
//
// A HEAD request to an SSE route (answered by its GET route) gets the stream's
// response headers and no body. [Config.OnConnect] runs for it, and a rejection
// is handled as for GET; when it accepts, [Config.OnDisconnect] runs right
// after the headers are sent, and [Config.Handler] does not run.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/sse
package sse
