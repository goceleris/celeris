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
// An SSE stream is a detached stream, so the server's ReadTimeout and
// WriteTimeout do not end it on any engine (celeris#855): it stays open
// without setting WriteTimeout to -1. The std engine clears the deadlines
// net/http armed for the request when the middleware detaches, and bounds each
// write of the stream by WriteTimeout instead: a client that stays connected
// and reads nothing ends its stream that way. Until then the handler (or the
// queue's drain goroutine) is parked in the write, so returning or
// [ClientPolicyClose] cannot end the stream sooner. The
// native engines do not park the handler in a write. When Shutdown's budget
// runs out, std fails the writes of the streams still running. A stream that
// should not run for ever is ended by its handler, which Send's error or the
// client's context tells when the client has gone.
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
