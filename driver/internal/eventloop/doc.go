// Package eventloop is internal infrastructure used by the celeris
// PostgreSQL and Redis drivers. It is not a supported public API: no
// backwards-compatibility guarantees are made for the types and functions
// exported here, and import from outside the celeris module is not
// supported.
//
// # What it provides
//
// A stripped-down event loop — one worker per CPU (or per request) owning
// an epoll fd on Linux, a net.Conn per FD on other platforms — that
// drivers use when no celeris HTTP server is available. When a Server is
// registered via [Resolve], the Server's own [engine.EventLoopProvider] is
// returned and the standalone loop is never created; drivers and HTTP
// workers then share the same kernel resources.
//
// # ServerProvider indirection
//
// Celeris's root package imports engine, and engine is imported here, so
// we cannot import celeris directly (import cycle). Instead, *celeris.Server
// is expected to satisfy [ServerProvider]:
//
//	type ServerProvider interface {
//	    EventLoopProvider() engine.EventLoopProvider
//	}
//
// [Resolve] accepts a nil provider to request the standalone loop, or a
// non-nil *celeris.Server (typed as ServerProvider) to hook into the HTTP
// event loop. Every Resolve must be paired with a [Release] so the
// standalone loop's refcount can drop to zero and shut down cleanly.
package eventloop
