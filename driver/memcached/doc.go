// Package memcached is celeris's native Memcached driver. It speaks both the
// text and binary wire protocols directly on top of the celeris event loop,
// using the same async-bridge architecture as the Redis and PostgreSQL
// drivers: one in-flight request enqueued per conn, responses demuxed from
// the loop's recv callback, and per-worker idle pools so handlers stay on
// the same CPU that dispatched them.
//
// # Usage
//
//	client, err := memcached.NewClient("localhost:11211",
//	    memcached.WithEngine(srv),           // optional; omit for a standalone loop
//	    memcached.WithProtocol(memcached.ProtocolText),
//	)
//	defer client.Close()
//
//	if err := client.Set(ctx, "key", "value", time.Minute); err != nil {
//	    log.Fatal(err)
//	}
//	v, err := client.Get(ctx, "key")
//
// [WithEngine] routes the driver's FDs through the same epoll/io_uring
// instance as the HTTP workers. When it is omitted, a package-level
// standalone loop is resolved and reference-counted
// (driver/internal/eventloop).
//
// # DSNs
//
// Addresses may optionally include a "memcache://" or "memcached://" scheme
// prefix; the prefix is stripped. The resulting "host:port" is passed to
// net.Dialer verbatim. TLS is not supported in v1.4.0.
//
// # Options
//
//   - [WithProtocol] selects the wire dialect (text or binary); default text.
//   - [WithMaxOpen] caps the total number of connections across workers.
//   - [WithMaxIdlePerWorker] bounds per-worker idle pool size.
//   - [WithTimeout] sets an advisory per-op deadline when ctx carries none.
//   - [WithDialTimeout] sets the TCP dial timeout.
//   - [WithMaxLifetime] / [WithMaxIdleTime] bound pooled-conn age and idleness.
//   - [WithHealthCheckInterval] configures the background health sweep.
//   - [WithEngine] wires the driver into a celeris.Server's event loop.
//
// # Typed Commands
//
// The typed API on [Client] covers:
//
//   - Storage:   Set, Add, Replace, Append, Prepend, CAS.
//   - Retrieval: Get, GetBytes, GetMulti, GetMultiBytes, Gets (returns CAS).
//   - Arithmetic: Incr, Decr.
//   - Keys:      Delete, Touch.
//   - Server:    Flush, FlushAfter, Stats, Version, Ping.
//
// Values passed to Set / Add / Replace / CAS may be string, []byte, integer,
// float, bool, nil, or fmt.Stringer. Anything else returns an error.
//
// # Errors
//
//   - [ErrCacheMiss]    — key not found.
//   - [ErrNotStored]    — Add/Replace/Append/Prepend precondition failed.
//   - [ErrCASConflict]  — CAS token mismatch (or concurrent modification).
//   - [ErrClosed]       — command issued against a closed client.
//   - [ErrProtocol]     — reply did not parse.
//   - [ErrMalformedKey] — key violates text-protocol constraints (whitespace,
//     control bytes, > 250 bytes).
//   - [ErrPoolExhausted] — all idle conns are stale (rare; pool blocks when full).
//
// Non-sentinel server errors surface as [*MemcachedError] carrying the reply
// kind ("ERROR", "CLIENT_ERROR", "SERVER_ERROR") or, in binary mode, the
// raw status code.
//
// # Pool
//
// Like the Redis and Postgres drivers, memcached uses [async.Pool] for
// worker-affinity connection pooling. Each worker owns an idle list guarded
// by its own lock; Acquire prefers the caller's worker before scanning
// neighbors. Acquire blocks when MaxOpen is reached (wait-queue semantics)
// rather than returning ErrPoolExhausted.
//
// # WithEngine and standalone operation
//
// [WithEngine] is optional. When omitted, [NewClient] resolves a standalone
// event loop backed by the platform's best mechanism (epoll on Linux,
// goroutine-per-conn on Darwin). The standalone loop is reference-counted
// inside driver/internal/eventloop and shared across all drivers that omit
// WithEngine. Correctness is identical with or without WithEngine; sharing
// the HTTP server's event loop reduces per-IO syscalls and improves data
// locality because driver FDs land on the same worker goroutine as HTTP
// handlers.
//
// # Text vs binary protocol
//
// The text protocol is the default. Every memcached deployment supports it,
// responses are self-describing, and the parser is trivially debugged by
// reading a PCAP. The binary protocol is opt-in via [WithProtocol]; it adds
// an opaque correlation ID, fixed-size framing, and explicit status codes.
// The binary dialect is useful where malformed user input or embedded NULs
// in values must round-trip cleanly, but its lack of server-side multi-key
// pipeline semantics in our current state machine means GetMulti on binary
// falls back to sequential GETs (one round trip per key). Callers needing
// true multi-get pipelining should pick the text protocol.
//
// # Expiration times
//
// Callers pass a Go time.Duration relative TTL. The driver rewrites it on
// the wire as either a relative second-count (when <= 30 days) or an
// absolute Unix timestamp (when > 30 days), matching the memcached server
// convention. Pass 0 for no expiration.
//
// # Key validation
//
// Keys are validated against text-protocol rules (1..250 bytes, no
// whitespace or control bytes) uniformly in both dialects, so that callers
// who switch from text to binary do not accidentally ship previously
// valid-looking keys that the text server would have rejected.
//
// # Known limitations (v1.4.0)
//
//   - No TLS — deploy over VPC, loopback, or a sidecar.
//   - No SASL authentication; memcached servers configured with -Y will
//     reject the driver's handshake. Deploy with network-level auth.
//   - Binary GetMulti is implemented as sequential GETs (one round trip per
//     key). Text GetMulti is truly pipelined in a single round trip.
//   - No server-side compression. Values are sent as-is; large payloads
//     should be compressed on the client side.
//   - No consistent-hash ring across multiple servers — the driver pins to
//     a single addr. Use a client-side hash ring or a proxy (mcrouter) for
//     sharding.
package memcached
