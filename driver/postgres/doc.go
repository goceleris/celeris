// Package postgres is celeris's native PostgreSQL driver. It speaks the
// PostgreSQL v3 wire protocol directly on top of the celeris event loop,
// exposes a database/sql compatible driver, and also provides a lower-level
// worker-affinity Pool for callers that want to skip database/sql overhead.
//
// # Usage
//
// Two entry points are supported. Which one to pick depends on whether the
// caller wants portability (database/sql + any ORM) or peak throughput
// (direct Pool bound to a celeris.Server).
//
// (a) database/sql
//
//	import (
//	    "database/sql"
//	    _ "github.com/goceleris/celeris/driver/postgres"
//	)
//
//	db, err := sql.Open("celeris-postgres", "postgres://app:pass@localhost/mydb?sslmode=disable")
//
// database/sql owns the pool in this mode. The driver registers itself under
// [DriverName] in its init. Every *sql.Conn handed out by db is a pgConn
// running on a standalone event loop (one is resolved and reference-counted
// inside driver/internal/eventloop).
//
// (b) Direct Pool
//
//	pool, err := postgres.Open(dsn, postgres.WithEngine(srv))
//	defer pool.Close()
//
//	rows, err := pool.QueryContext(ctx, "SELECT id, name FROM users WHERE tenant = $1", tenantID)
//
// The direct Pool pins each connection to a worker (see [WithWorker] for the
// context-based hint API), so a handler running on worker N preferentially
// re-uses conns whose event-loop callbacks also land on worker N. When
// [WithEngine] is passed, the Pool shares the same epoll/io_uring instance
// as the HTTP workers; otherwise a standalone driver loop is resolved.
//
// # DSNs
//
// Both the libpq URL form and the key=value form are accepted.
//
// URL form:
//
//	postgres://user:pass@host:5432/dbname?sslmode=disable&application_name=svc&connect_timeout=5
//
// Key=value form:
//
//	host=localhost port=5432 user=app password=secret dbname=mydb sslmode=disable
//
// Recognized DSN keys: host, port, user, password, dbname / database, sslmode,
// connect_timeout (seconds), statement_cache_size, auto_cache_statements,
// application_name. Any other key is forwarded to the server as a
// StartupMessage parameter (so search_path, timezone, statement_timeout, and
// similar GUCs can be set at connect time).
//
// auto_cache_statements (default true at the [Pool.Open] / [NewConnector]
// layer): when true, cacheable SELECT-style QueryContext calls with
// arguments transparently auto-prepare on first use and reuse the
// prepared statement (Bind+Execute+Sync) on subsequent invocations.
// Mirrors pgx's QueryExecModeCacheStatement default. Set
// `auto_cache_statements=false` in the DSN to opt out and stay on the
// extended-protocol-without-cache path. Arg-less queries always take the
// simple-query path regardless.
//
// # Options
//
// Pool knobs are supplied as functional options to [Open]:
//
//   - [WithEngine]      bind the pool to a celeris.Server event loop.
//   - [WithMaxOpen]     total conn cap (default NumWorkers*4).
//   - [WithMaxIdlePerWorker]  per-worker idle list cap (default 2).
//   - [WithMaxLifetime]       max conn age (default 30m).
//   - [WithMaxIdleTime]       max idle duration (default 5m).
//   - [WithHealthCheck]       background sweep interval (default 30s; 0 disables).
//   - [WithStatementCacheSize] per-conn prepared-statement LRU (default 256).
//   - [WithApplication]       application_name startup parameter.
//
// # Transactions
//
// [Pool.BeginTx] opens a [Tx] that pins its connection until [Tx.Commit] or
// [Tx.Rollback] is called. Passing *sql.TxOptions selects the isolation level
// (Read Uncommitted / Read Committed / Repeatable Read / Serializable) and
// the read-only flag, all folded into a single BEGIN round trip.
//
// Savepoints are reachable three ways:
//
//  1. (*postgres.Tx).Savepoint / ReleaseSavepoint / RollbackToSavepoint on
//     the direct Pool transaction.
//
//  2. database/sql via sql.Conn.Raw + the exported [Conn] alias:
//
//     conn, _ := db.Conn(ctx)
//     defer conn.Close()
//     _ = conn.Raw(func(raw any) error {
//     pc := raw.(*postgres.Conn)
//     return pc.Savepoint(ctx, "sp1")
//     })
//
//  3. Raw simple queries ("SAVEPOINT sp1") issued through Exec on a
//     transaction; the first two forms are preferred because they
//     validate the name.
//
// Savepoint names must match [A-Za-z0-9_]+; other identifiers are
// rejected before the wire write to avoid SQL-injection.
//
// # Bulk COPY
//
// [Pool.CopyFrom] streams rows into a table via COPY FROM STDIN. Callers
// implement [CopyFromSource] (or use [CopyFromSlice] for in-memory fixtures)
// to feed rows. [Pool.CopyTo] runs COPY ... TO STDOUT and invokes a callback
// for each raw row. Both use PG's text format with tab separators and
// backslash escaping.
//
// # Pool configurations
//
// database/sql and postgres.Pool each maintain their own set of connections.
// Opening a sql.DB with sql.Open("celeris-postgres", dsn) and separately
// calling postgres.Open(dsn) does not share pool state — configure
// sql.DB.SetMaxOpenConns on the former and [WithMaxOpen] on the latter
// independently.
//
// # Type support
//
// The encoder/decoder understands the following server OIDs out of the box
// (see postgres/protocol for the full codec table):
//
//	bool, int2, int4, int8, float4, float8, text, varchar, bytea, uuid,
//	date, timestamp, timestamptz, numeric, json, jsonb, and the one-
//	dimensional array variants of those types (_bool, _int4, _text, ...).
//
// Go type mappings:
//
//	bool                   ↔ bool
//	int2/int4/int8         ↔ int64 (accepts int, int32 at encode)
//	float4/float8          ↔ float64
//	text/varchar           ↔ string
//	bytea                  ↔ []byte
//	uuid                   ↔ []byte (16 bytes)
//	date/timestamp/tstz    ↔ time.Time
//	numeric                ↔ string (use strconv or math/big to convert)
//	json/jsonb             ↔ []byte or string
//	arrays                 ↔ []T of the element's Go type
//
// Any argument that implements [database/sql/driver.Valuer] is resolved to
// its underlying value before encoding. Any destination that implements
// [database/sql.Scanner] receives the raw bytes. Custom types can be plugged
// in via [protocol.RegisterType].
//
// # Errors
//
// Server-side errors surface as [*PGError] (a re-export of
// [protocol.PGError]). The struct carries the five-character SQLSTATE code,
// the short message, and any optional fields the server attached (detail,
// hint, constraint name, etc.). Sentinels are wrapped so errors.Is and
// errors.As work as expected:
//
//	var pgErr *postgres.PGError
//	if errors.As(err, &pgErr) && pgErr.Code == "23505" { ... }
//
// Other package-level sentinels: [ErrPoolClosed], [ErrClosed], [ErrBadConn],
// [ErrSSLNotSupported], [ErrUnsupportedAuth].
//
// # Query cancellation
//
// Canceling a context aborts the in-flight query by dialing a short-lived
// side connection and sending the PostgreSQL v3 CancelRequest packet
// (protocol code 80877102). The client blocks until the server drains the
// rest of the original query's response — cancellation is cooperative, not
// instantaneous.
//
// # WithEngine and standalone operation
//
// [WithEngine] is optional. When omitted, [Open] resolves a standalone event
// loop backed by the platform's best mechanism (epoll on Linux, goroutine-
// per-conn on Darwin). The standalone loop is reference-counted inside
// driver/internal/eventloop and shared across all drivers that omit WithEngine.
// Correctness is identical with or without WithEngine; the difference is
// performance: sharing the HTTP server's event loop improves data locality
// and saves one epoll/uring syscall per I/O. Expect ~5-20% lower latency for
// serial queries when WithEngine is used.
//
// database/sql mode (sql.Open) always uses the standalone loop. For optimal
// performance, prefer the direct Pool with WithEngine(srv).
//
// # Rows iteration
//
// [Pool.QueryContext] returns a *Rows value. Iterate with Next + Scan:
//
//	rows, err := pool.QueryContext(ctx, "SELECT id, name FROM users")
//	if err != nil { return err }
//	defer rows.Close()
//	for rows.Next() {
//	    var id int64
//	    var name string
//	    if err := rows.Scan(&id, &name); err != nil { return err }
//	    // process id, name
//	}
//	if err := rows.Err(); err != nil {
//	    return err // check Err() after the loop
//	}
//
// Always check [Rows.Err] after the loop — it surfaces errors from the
// underlying query that were deferred until iteration completed (e.g.
// cancellation, network errors, or server-side errors on large result sets).
//
// # Known limitations
//
//   - TLS is not yet supported. sslmode=require, verify-ca, and verify-full
//     are rejected at [Open] time with [ErrSSLNotSupported]. Deploy over
//     VPC, loopback, or a sidecar TLS terminator.
//   - Result sets are fully buffered before Rows.Next returns — there is no
//     true row-by-row streaming. Callers with large result sets should page
//     via LIMIT/OFFSET or a server-side DECLARE CURSOR inside a transaction.
//   - Authentication supports trust, cleartext, MD5, and SCRAM-SHA-256
//     (without channel binding). GSS, SSPI, Kerberos, and SCRAM-SHA-256-PLUS
//     are not implemented.
//   - No LISTEN / NOTIFY. Asynchronous NotificationResponse frames received
//     from the server are silently dropped by the dispatcher.
//   - COPY IN / COPY OUT is exposed via [Pool.CopyFrom] / [Pool.CopyTo] on
//     the direct Pool API. database/sql has no analogous surface; callers
//     who need COPY must use the direct Pool.
//   - No PG 14+ pipeline mode (Execute chaining).
//   - No cluster- or replica-routing logic; callers with a read replica
//     should open a second Pool pointed at it.
package postgres
