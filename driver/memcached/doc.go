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
// in values must round-trip cleanly. Both dialects truly pipeline
// [Client.GetMulti] in a single round trip — binary uses OpGetKQ + OpNoop,
// text uses the multi-key `get` command. Multi-packet server replies such
// as binary Stats are decoded natively on the active conn.
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
// # Multi-server sharding (ClusterClient)
//
// Production memcached deployments fan keys across N independent nodes; the
// memcached protocol has no cluster-aware peer discovery, so sharding is a
// client-side concern. [ClusterClient] owns a static ring of [Client]
// instances (one per configured address) and routes each key via a
// libmemcached-compatible ketama consistent-hash ring.
//
//	cc, err := memcached.NewClusterClient(memcached.ClusterConfig{
//	    Addrs: []string{
//	        "memcache-a:11211",
//	        "memcache-b:11211",
//	        "memcache-c:11211",
//	    },
//	    // Optional: relative weights (nil = equal).
//	    // Weights: []uint32{1, 2, 1},
//	})
//	defer cc.Close()
//	cc.Set(ctx, "user:42", "payload", time.Hour)
//
// The ring assigns 160 virtual nodes per unit weight, placed at
// MD5("<addr>-<vnode>") boundaries (4 ring points per digest). Lookup uses
// CRC32-IEEE on the user key and a binary search with wrap-around. Removing
// one of N nodes only re-homes the ~1/N share of keys it owned; the rest
// keep their owner, matching the usual ketama guarantee.
//
// The [ClusterClient] API mirrors [Client] command-for-command:
//
//   - Single-key ops (Get, Set, Delete, Incr, ...) route to pickNode(key).
//   - GetMulti / GetMultiBytes partition keys by owner, fan out one
//     sub-request per node in parallel, and merge the per-node responses.
//     On error the first error wins; partial results from other nodes are
//     discarded.
//   - Stats, Version return a per-node map keyed by address.
//   - Flush, FlushAfter, Ping fan out to every node; the first error wins.
//   - NodeFor(key) exposes the ring's routing decision for debugging and
//     for out-of-band work that needs to track per-node state.
//
// Topology is static for the lifetime of the client: memcached nodes do
// not gossip and [ClusterClient] does not run a background refresh loop.
// Adding or removing a node requires tearing down the client and building
// a new one. For a dynamically-changing fleet, deploy a proxy such as
// mcrouter or twemproxy in front of the nodes and point the single-node
// [Client] at the proxy.
//
// # Node failure detection and failover (v1.5.0)
//
// [ClusterClient] tracks the health of each node and automatically
// reroutes around failed ones so a single dead node does not translate
// into per-key errors for every key in its slice of the ring.
//
// Detection is passive + probe:
//
//   - Each [Client] operation issued through the cluster updates the
//     node's consecutive-failure counter. Infrastructure errors (dial
//     failures, I/O errors, protocol corruption) count; protocol-level
//     responses ([ErrCacheMiss], [ErrNotStored], [ErrCASConflict],
//     [*MemcachedError]) do NOT — they are valid server replies.
//   - Once the counter reaches [ClusterConfig.FailureThreshold] (default
//     2), the node is marked failing. [ClusterClient.pickNode] then
//     walks the node list clockwise from the failing node's position
//     and returns the first healthy neighbor it finds.
//   - A background goroutine (configurable via
//     [ClusterConfig.HealthProbeInterval], default 5s) pings any
//     failing node that has been down for at least 1 second and clears
//     the failing flag on success.
//   - Any successful operation passively clears the flag — so a node
//     that silently recovers while still receiving traffic from
//     passed-through requests is picked up immediately.
//
// Key-redistribution implications:
//
//   - The successor is the next node in insertion order (the order
//     given in [ClusterConfig.Addrs]), NOT the next ring neighbor.
//     This preserves the consistent-hash invariant that all keys
//     formerly routed to failing node N now route to the same
//     successor — rather than being scattered across the ring.
//   - During the failure transition, keys that were cached on N will
//     MISS on the successor. During recovery, keys that were cached on
//     the successor will MISS on N. Applications must tolerate both
//     transitions — caches are not authoritative.
//
// Observability: [ClusterClient.NodeHealth] returns a snapshot of the
// failing flag per address; [ClusterClient.NodeStatsMap] includes the
// last-fail timestamp and consecutive-fail counter.
//
// Semantics vs. gomemcache/dalli: celeris matches dalli's "mark after
// N failures, passive heal on success" policy. The background probe
// is an addition for deployments where a failing node receives no
// traffic (otherwise the passive path would never clear the flag).
//
// # Known limitations (v1.4.0)
//
//   - No TLS — deploy over VPC, loopback, or a sidecar.
//   - No SASL authentication; memcached servers configured with -Y will
//     reject the driver's handshake. Deploy with network-level auth.
//   - No server-side compression. Values are sent as-is; large payloads
//     should be compressed on the client side.
//   - [ClusterClient] topology is static: no background refresh. Memcached
//     nodes do not gossip. To add/remove nodes at runtime, tear down and
//     rebuild the client, or front the fleet with mcrouter.
package memcached
