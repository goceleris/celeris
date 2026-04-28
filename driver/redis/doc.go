// Package redis is celeris's native Redis driver. It speaks RESP2 and RESP3
// directly on top of the celeris event loop using the same async-bridge
// architecture as the celeris PostgreSQL driver: one in-flight request
// enqueued per conn, responses demuxed from the loop's recv callback, and
// per-worker idle pools so handlers stay on the same CPU that dispatched
// them.
//
// # Usage
//
//	client, err := redis.NewClient("localhost:6379",
//	    redis.WithEngine(srv),          // optional; omit for a standalone loop
//	    redis.WithPassword("secret"),
//	    redis.WithDB(0),
//	)
//	defer client.Close()
//
//	v, err := client.Get(ctx, "key")
//
// [WithEngine] routes the driver's FDs through the same epoll/io_uring
// instance as the HTTP workers. When it is omitted, a package-level
// standalone loop is resolved and reference-counted
// (driver/internal/eventloop).
//
// Connections are lazily dialed on first command. The first dial does the
// RESP3 handshake (HELLO 3 + AUTH + SETNAME), falls back to RESP2 + AUTH +
// SELECT if HELLO is rejected, or speaks RESP2 unconditionally when
// [WithForceRESP2] is set (required for ElastiCache classic clusters and
// for older servers that do not implement HELLO).
//
// # Commands
//
// The typed API on [Client] covers:
//
//   - Strings: Get, GetBytes, Set, SetNX, Del, Exists, Incr, Decr, MGet,
//     Expire, PExpire, ExpireAt, PExpireAt, Persist, TTL.
//   - Hashes:  HGet, HSet, HDel, HGetAll, HExists, HKeys, HVals.
//   - Lists:   LPush, RPush, LPop, RPop, LRange, LLen.
//   - Sets:    SAdd, SRem, SMembers, SIsMember, SCard.
//   - Sorted sets: ZAdd, ZRange, ZRangeByScore, ZRem, ZScore, ZCard.
//   - Keys:    Type, Rename, RandomKey, Scan (cursor iteration).
//   - Pub/Sub: Publish, Subscribe, PSubscribe (see below).
//   - Scripting: Eval, EvalSHA, ScriptLoad.
//   - Watch for optimistic locking (pins a conn for the callback).
//
// Any command the typed surface does not expose can be sent via [Client.Do],
// which accepts ...any args, converts them through [argify], and returns a
// [*protocol.Value] plus a server-side error as [*RedisError].
//
// # Pipeline
//
// [Client.Pipeline] batches commands into a single network write. Each
// queued call returns a typed handle (*StringCmd, *IntCmd, ...); the
// caller then invokes [Pipeline.Exec] to flush and harvest the replies:
//
//	p := client.Pipeline()
//	a := p.Set("k", "v", 0)
//	b := p.Incr("counter")
//	if err := p.Exec(ctx); err != nil { ... }
//	_, _ = a.Result()
//	n, _ := b.Result()
//
// All commands in one Exec ride the same connection, so replies are
// returned in the same order as commands were enqueued. Replies are
// detached from the reader's scratch buffer before the conn is released,
// so each Result() call is safe to retain.
//
// # Transactions
//
// Typed MULTI/EXEC transactions are built via [Client.TxPipeline]. Queued
// commands return deferred *Cmd handles that populate after [Tx.Exec]:
//
//	tx, _ := client.TxPipeline(ctx)
//	defer tx.Discard() // no-op after a successful Exec
//	a := tx.Incr("visits")
//	b := tx.Incr("uniques")
//	if err := tx.Exec(ctx); err != nil { ... }
//	va, _ := a.Result()
//	vb, _ := b.Result()
//
// Exec sends MULTI + the buffered commands + EXEC in a single write. If a
// WATCHed key changed (EXEC returns a null array) every queued *Cmd.Result()
// returns [ErrTxAborted]. WATCH / UNWATCH are available on the Tx itself
// and must be called before any command is queued.
//
// Raw MULTI/EXEC via [Client.Do] also works; the pool tracks the
// MULTI/EXEC/DISCARD state on the pinned conn so a premature release issues
// a DISCARD before the conn returns to the idle list.
//
// # Pub/Sub
//
// [Client.Subscribe] and [Client.PSubscribe] open a dedicated subscriber
// connection. Messages arrive on [PubSub.Channel]:
//
//	ps, _ := client.Subscribe(ctx, "events")
//	defer ps.Close()
//	for m := range ps.Channel() { handle(m) }
//
// On transport error the driver automatically reconnects with exponential
// backoff and replays the tracked SUBSCRIBE/PSUBSCRIBE list. Messages that
// arrive while the connection is down are lost — delivery is at-most-once.
// The channel buffer defaults to 256; when it fills, [PubSub.deliver] drops
// the oldest message to make room and bumps [PubSub.Drops].
//
// # RESP2 vs RESP3
//
// RESP3 is negotiated with HELLO 3 during the handshake and brings richer
// reply types (Map, Set, Double, Bool, Verbatim, BigNumber, Push) that the
// driver surfaces via the [protocol.Value] type-tag enum. When the server
// rejects HELLO the driver transparently downgrades to RESP2 and speaks
// AUTH + SELECT, so almost all callers can leave the default alone.
// [WithForceRESP2] pins the connection to RESP2 for deployments that must
// avoid HELLO entirely. [Client.Proto] reports the negotiated version.
//
// # Zero-copy semantics
//
// The RESP reader returns [protocol.Value] structs whose Str / Array / Map
// fields alias the reader's internal buffer. To keep hot-path reads
// allocation-free, the driver does NOT copy on every reply. Instead:
//
//   - Typed accessors on [Client] (Get, HGet, MGet, ...) return freshly
//     allocated Go strings / slices / maps — the copy happens inside the
//     decode helper, so the caller never sees aliased bytes.
//   - Pipeline results are detached (deep-copied) before the conn returns
//     to the idle pool, so each *Cmd.Result() is independent.
//   - [Client.Do] returns a [*protocol.Value] that has already been detached
//     from the reader buffer; callers can retain it freely.
//
// The only place raw aliasing is observable is inside custom dispatch
// routines that reach into the internal protocol APIs directly — typed
// callers are always safe.
//
// # Errors
//
// Server-side error replies surface as [*RedisError] with Prefix (e.g.
// "WRONGTYPE", "ERR", "MOVED") and full Msg. Sentinels are wired through
// [RedisError.Is]:
//
//   - [ErrNil]          null bulk reply (GET on missing key, LPOP on empty list).
//   - [ErrClosed]       Client or PubSub after Close.
//   - [ErrProtocol]     reply did not parse.
//   - [ErrMoved]        cluster redirect (not followed — cluster is unsupported).
//   - [ErrWrongType]    operation on a key of the wrong kind.
//   - [ErrPoolExhausted] all idle conns are stale (rare; pool blocks when full).
//   - [ErrTxAborted]    WATCH / EXEC aborted the transaction.
//
// # WithEngine and standalone operation
//
// [WithEngine] is optional. When omitted, [NewClient] resolves a standalone
// event loop backed by the platform's best mechanism (epoll on Linux, goroutine-
// per-conn on Darwin). The standalone loop is reference-counted inside
// driver/internal/eventloop and shared across all drivers that omit WithEngine.
// Correctness is identical with or without WithEngine; the difference is
// performance: sharing the HTTP server's event loop saves one epoll/uring
// syscall per I/O because driver FDs land on the same worker goroutine as HTTP
// handlers, improving data locality. Expect ~5-20% lower latency for serial
// queries when WithEngine is used.
//
// # Pipeline.Release lifetime
//
// [Pipeline.Release] returns the Pipeline — and all of its internal slabs —
// to a sync.Pool for reuse. After Release, every typed Cmd handle (*StringCmd,
// *IntCmd, *StatusCmd, *FloatCmd, *BoolCmd) previously returned from the
// Pipeline is invalid. Calling Result() on an invalidated handle returns
// [ErrClosed]. Release is optional; un-Released Pipelines are GC'd normally.
// For tight hot paths, pooling via Release eliminates per-request slab allocs.
//
// # SCAN usage
//
// Cursor-based iteration is exposed via [Client.Scan]:
//
//	it := client.Scan(ctx, "user:*", 100)
//	for {
//	    key, ok := it.Next(ctx)
//	    if !ok {
//	        break
//	    }
//	    // process key
//	}
//	if err := it.Err(); err != nil {
//	    log.Fatal(err)
//	}
//
// The ScanIterator handles cursor paging internally. The match pattern is
// passed as MATCH and the count hint as COUNT. Both are optional — pass ""
// and 0 to iterate all keys. The iterator is NOT safe for concurrent use.
//
// # Watch (optimistic locking)
//
// [Client.Watch] pins a connection, WATCHes the given keys, and invokes fn
// with a [Tx] that queues commands under MULTI/EXEC. If a WATCHed key is
// modified before EXEC, the transaction aborts with [ErrTxAborted] and the
// caller can retry:
//
//	err := client.Watch(ctx, func(tx *redis.Tx) error {
//	    val, err := client.Get(ctx, "counter")
//	    if err != nil { return err }
//	    n, _ := strconv.Atoi(val)
//	    tx.Set("counter", strconv.Itoa(n+1), 0)
//	    return tx.Exec(ctx)
//	}, "counter")
//
// # Push callbacks (client tracking)
//
// RESP3 push frames on command connections (e.g. CLIENT TRACKING invalidation
// messages) can be intercepted via [WithOnPush] or [Client.OnPush]:
//
//	client, _ := redis.NewClient("localhost:6379",
//	    redis.WithOnPush(func(channel string, data []protocol.Value) {
//	        log.Printf("push: %s %v", channel, data)
//	    }),
//	)
//
// When no callback is registered, push frames on command connections are
// silently dropped. Push frames on pub/sub connections are always routed
// through the [PubSub.Channel] mechanism.
//
// # Cluster Transactions
//
// Redis Cluster requires all keys in a MULTI/EXEC transaction to reside in
// the same hash slot. Cross-slot transactions are impossible by design —
// the server returns -CROSSSLOT. The celeris driver validates slot affinity
// client-side and returns [ErrCrossSlot] before contacting the server when
// keys span multiple slots.
//
// Use hash tags to colocate related keys on the same slot:
//
//	user:{123}:name   → hashes only "{123}" → slot X
//	user:{123}:email  → hashes only "{123}" → slot X
//
// [ClusterClient.TxPipeline] returns a [ClusterTx] that queues commands and
// verifies all keys target the same slot:
//
//	tx := cluster.TxPipeline()
//	a := tx.Set("{user:123}:name", "Alice", 0)
//	b := tx.Set("{user:123}:email", "alice@example.com", 0)
//	if err := tx.Exec(ctx); err != nil { ... }
//
// [ClusterClient.Watch] validates slot affinity for the watched keys and
// delegates to the target node's [Client.Watch]:
//
//	err := cluster.Watch(ctx, func(tx *redis.Tx) error {
//	    tx.Incr("{user:123}:visits")
//	    return tx.Exec(ctx)
//	}, "{user:123}:visits")
//
// # Known limitations
//
//   - TLS (rediss://) is not yet supported; the scheme is rejected in
//     [NewClient] with a clear error. Deploy over VPC, loopback, or a
//     sidecar TLS terminator.
//   - Cluster ([ClusterClient]) and Sentinel ([SentinelClient]) are supported.
//     MOVED/ASK redirects are handled transparently by ClusterClient; Sentinel
//     auto-discovers the master and handles failovers via +switch-master.
//     Cluster pipeline splits commands by slot and executes per-node
//     sub-pipelines in parallel. SSUBSCRIBE (shard channels, Redis 7+) is
//     not supported; use regular SUBSCRIBE which is cluster-wide.
//   - No wrappers for RedisJSON, RediSearch, RedisGraph, or RedisTimeSeries —
//     use [Client.Do] with the raw command strings.
//   - Pub/Sub auto-reconnects but messages delivered while the conn is down
//     are lost. Callers needing at-least-once should add server-side
//     durability (Streams + consumer groups).
//   - database/sql is not implemented — Redis is not a SQL database.
//   - Read/write timeouts are advisory today; cancellation is via ctx.
package redis
