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
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/sse
package sse
