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
//     double). This is for HTTP/1.1 streams. An h2c stream is not detached and
//     this bound does not apply to it; what ends one whose client reads
//     nothing is HTTP/2 flow control (see HTTP/2 below).
//
// Over HTTP/2 (h2c), register the SSE route with [celeris.Route.Async]. On
// epoll, io_uring and Adaptive an HTTP/2 handler runs on the event loop unless
// its route is async, and the engine sends the response when the handler
// returns: on a route that is not async the client sees neither the response
// headers nor any event until the stream ends, and the stream holds its event
// loop for as long as it lives, so other connections on that loop wait. The
// server's AsyncHandlers setting does not change this for HTTP/2. On an async
// route the stream runs on the worker pool, the headers and each event go out
// as the handler writes them, and the loop is free. (Measured on epoll and
// Adaptive with a 3 s stream: the first event reached the client after 1 ms on
// an async route and after 3.06 s on an inline one; std streams either way.)
// [celeris.StreamWriter.Flush] is no delivery guarantee on HTTP/2, and an
// async route does not need one: its headers and events are not waiting for a
// flush.
//
// The native engines do not exempt an HTTP/2 stream from ReadTimeout and
// IdleTimeout, as they do a detached HTTP/1.1 one. Their sweep applies both to
// an HTTP/2 connection whatever streams it has open (internal/engine/epoll
// loop.go and the io_uring worker, measured from the last frame the client
// sent), so a connection whose client sends no frame for the smaller of the
// two (60 s and 10 min by default) is closed, an SSE stream's with it. Measured
// on epoll and Adaptive with ReadTimeout 1 s and an async route, the stream was
// closed 1.0 s after the request, which is after the client's last frame; the
// IdleTimeout case is from the code and not measured, and io_uring is not
// measured. std is not affected. This is a defect of the engines, found while
// probing celeris#889 item 34 and tracked there; no release is promised for it.
//
// A client of an async-route stream that stays connected and reads nothing more
// than the headers is disconnected as well. With the default flow-control
// window a write from the worker pool waits for the peer's window for at most
// WriteTimeout and then resets the stream, which cancels [Client.Context] and
// runs [Config.OnDisconnect]. Measured on epoll with WriteTimeout 1 s, the
// disconnect came 1.4 s after connect with the default 64 KiB window, and 1.7 s
// with a window of 1 GiB, which the stream does not fill: what ends that one is
// not confirmed. io_uring is not measured.
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
