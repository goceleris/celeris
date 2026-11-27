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
// Values returned by Client methods, [Client.Do] and the typed Cmds of a [Tx]
// are copies, safe to retain. Values read from a [Pipeline] (its typed Cmds)
// are valid until [Pipeline.Release] or [Pipeline.Discard]: copy what you
// keep. Server error replies surface as
// [*RedisError] (with sentinels such as [ErrNil], [ErrWrongType],
// [ErrCrossSlot]); see [errors.Is]. TLS (rediss://) is not yet supported.
//
// # Platforms
//
// On Linux (and other unix hosts) the driver runs on the celeris event loop:
// that is the supported production path, and the one every claim about
// throughput and latency refers to. On Windows the package compiles and works
// through a portable fallback for development and testing: each connection
// is a net.Conn read by its own goroutine, with no epoll or io_uring and no
// WithEngine colocation. It is tested in CI on windows-latest against
// in-process fake servers, not benchmarked and not tuned; do not deploy it.
//
// # Context cancellation
//
// A context with a deadline ends a command, a [Pipeline.Exec] or a [Tx.Exec] that
// waits for the server when the deadline passes, in every mode of the driver (a
// conn on the event loop, and a direct-mode conn: a pool with AsyncHandlers, a
// started epoll or io_uring server, and off Linux a server that has not started),
// and the error matches context.DeadlineExceeded (errors.Is); a direct-mode conn
// used to report the socket's raw "i/o timeout" instead.
//
// A cancel() ends a command that blocks on the server (BLPOP, BRPOP, BRPOPLPUSH,
// BLMOVE, BLMPOP, BZPOPMIN, BZPOPMAX, BZMPOP, XREAD and XREADGROUP with BLOCK, WAIT,
// WAITAOF, run through [Client.Do]) in every mode, with an error that matches
// context.Canceled, when the context has no deadline. On a direct-mode conn nothing
// else is watched for a cancel: an ordinary command, a pipeline or a Tx waits for the
// server's answer, as before, and a blocking command under a context that has a
// deadline ends at the deadline, not at an earlier cancel() (the call then returns the
// context's error, context.Canceled if it was canceled by then, a conn on the event
// loop returns at the cancel in all these cases). A context that cannot end
// (context.Background) costs a command nothing, and so does a context with a
// deadline (the socket's deadline is set for the command and cleared after it, as
// before); a blocking command under a context that can only be canceled costs a
// direct-mode conn the registration of a context.AfterFunc.
//
// The server may still be running a command that its context ended, and its reply
// is on the way, so the connection is closed and the pool dials another for the
// next command. A pop (BLPOP, BRPOP, BZPOPMIN, BZPOPMAX, BLMPOP, BZMPOP) that its
// context ends just as the server answers may already have removed the element, and
// it is lost: use BLMOVE or BRPOPLPUSH to a processing list when an element must not
// be lost (a deadline expiry did this before as well; a cancel now reaches it too).
//
// # Waiting for pipeline and Tx replies
//
// On Linux, a pipeline or Tx Exec on a direct-mode connection (the pool of a
// server with AsyncHandlers) can wait for its reply in poll(2) on the calling
// goroutine's own thread instead of parking in Go's netpoll, which saves the
// netpoll wake-up hop when the round trip is long (celeris#854). The
// environment variable CELERIS_REDIS_DIRECT_WAIT, read on the first such Exec,
// sets the mode:
//
//   - unset or empty: auto on Linux, where each connection switches to poll(2)
//     while its first-reply wait stays above 300 us and back to netpoll below
//     150 us. Elsewhere, and for a pool with a negative PoolSize or the
//     WithEngine fallback pool, netpoll;
//   - "netpoll": always Go's netpoll, the behaviour before #854 (the opt-out);
//   - "auto": as the default, on every direct-mode pool;
//   - "block": always poll(2), on every direct-mode pool;
//   - any other value: netpoll.
//
// A slow Exec that waits in poll(2) holds one OS thread until the reply
// arrives (at most 20 ms per wait, then it parks in netpoll), so the threads
// held are bounded by the pool's MaxOpen ([Config.PoolSize], default 4 times
// GOMAXPROCS) and unbounded with a negative PoolSize. Pub/sub and the other pools
// are not affected, and no other platform has the poll(2) wait.
//
// # Errors from writes
//
// An error from a write does not prove that the write was not applied: a
// connection that closes right after the server answered can be reported as a
// failure (celeris#1460); do not blindly retry Incr, Decr or other
// non-idempotent commands on a connection error.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/data-stores
//
// # Status
//
// Status: beta. The exported API of the driver packages stays under the
// compatibility check, but behaviour fixes (what a value scans to, which
// cases return an error) may land in minor releases and are listed in the
// release notes.
//
// The supported types and the known gaps are listed on the supported-types
// page: https://goceleris.dev/docs/redis-replies
package redis
