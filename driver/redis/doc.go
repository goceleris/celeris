// Package redis is celeris's native RESP2/RESP3 Redis driver, designed to run
// its socket I/O on the celeris event loop alongside your HTTP handlers.
//
// [NewClient] returns a [Client] backed by a per-worker connection pool;
// connections are dialed lazily on first command and negotiate RESP3 (HELLO 3)
// with automatic RESP2 fallback. Pass [WithEngine] to colocate the driver's
// file descriptors on the same workers as a running celeris server (lower
// latency via better data locality); omit it to use a standalone, internally
// reference-counted event loop. Other options include [WithPassword],
// [WithUsername], [WithDB], [WithForceRESP2], [WithProto], and [WithOnPush];
// see [Config] for the full set.
//
//	client, err := redis.NewClient("localhost:6379", redis.WithEngine(srv))
//	defer client.Close()
//	v, err := client.Get(ctx, "key")
//
// Key exported symbols and when to reach for them:
//
//   - [Client] — typed commands for strings, hashes, lists, sets, sorted sets,
//     keys, scripting (Eval, EvalSHA, ScriptLoad) and pub/sub. [Client.Do]
//     (plus DoString/DoInt/DoBool/DoSlice) is the escape hatch for any command
//     the typed surface omits.
//   - [Client.Pipeline] returns a [Pipeline] that batches commands into one
//     write; queued calls return typed handles (*StringCmd, *IntCmd, etc.)
//     resolved after [Pipeline.Exec].
//   - [Client.TxPipeline] and [Client.Watch] provide MULTI/EXEC transactions
//     and optimistic locking via [Tx]; aborts surface as [ErrTxAborted].
//   - [Client.Subscribe] / [Client.PSubscribe] return a [PubSub] whose
//     [PubSub.Channel] delivers [Message]s, with automatic reconnect.
//   - [Client.Scan] returns a [ScanIterator] for cursor-based key iteration.
//   - [NewClusterClient] ([ClusterClient]) and [NewSentinelClient]
//     ([SentinelClient]) cover Redis Cluster and Sentinel deployments.
//
// Typed accessors and pipeline/Do results are detached from the reader buffer,
// so returned values are safe to retain. Server error replies surface as
// [*RedisError] (with sentinels such as [ErrNil], [ErrWrongType],
// [ErrCrossSlot]); see [errors.Is]. TLS (rediss://) is not yet supported.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/data-stores
package redis
