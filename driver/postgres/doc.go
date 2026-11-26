// Package postgres is celeris's native PostgreSQL driver. It speaks the
// PostgreSQL v3 wire protocol directly on top of the celeris event loop,
// exposes a database/sql-compatible driver, and also provides a lower-level
// worker-affinity [Pool] for callers that want to skip database/sql overhead.
//
// There are two entry points. Use database/sql for portability (works with
// any ORM); use the direct [Pool] for peak throughput.
//
//	// database/sql — registers itself under DriverName in init.
//	import _ "github.com/goceleris/celeris/driver/postgres"
//	db, err := sql.Open(postgres.DriverName, "postgres://app:pass@localhost/mydb?sslmode=disable")
//
//	// Direct Pool — optionally bound to a celeris.Server event loop.
//	pool, err := postgres.Open(dsn, postgres.WithEngine(srv))
//
// [Open] accepts both the libpq URL form and the key=value DSN form. Pool
// behavior is tuned with functional [Option] values: [WithEngine],
// [WithMaxOpen], [WithMaxIdlePerWorker], [WithMaxLifetime], [WithMaxIdleTime],
// [WithHealthCheck], [WithStatementCacheSize], and [WithApplication].
// [WithEngine] is optional. When set on a server that has not started yet, the
// pool shares the HTTP server's event loop instead of resolving a standalone
// one; a bare [Open] and [WithEngine] on a server that has started or runs
// async handlers give a direct-mode pool, whose results are capped at 64 MiB
// (see [ErrResultTooBig]: always check [Rows.Err]). The same holds for the
// connections of a [Connector] bound to a started epoll, io_uring or adaptive
// server ([Connector.WithEngine] says which mode it uses when).
// [WithWorker] adds a per-call worker-affinity hint to a context.
//
// Query with [Pool.QueryContext] (returns [*Rows]), [Pool.QueryRow]
// (returns [*Row]), or [Pool.ExecContext] (returns [Result]). Transactions
// come from [Pool.BeginTx], which returns a [*Tx] with [Tx.Commit],
// [Tx.Rollback], and savepoint helpers ([Tx.Savepoint],
// [Tx.ReleaseSavepoint], [Tx.RollbackToSavepoint]). Bulk loads use
// [Pool.CopyFrom] (feed rows via [CopyFromSource] or [CopyFromSlice]) and
// [Pool.CopyTo].
//
// Server errors surface as [*PGError] carrying the SQLSTATE code; match
// with errors.As. Package sentinels include [ErrPoolClosed], [ErrClosed],
// [ErrBadConn], [ErrSSLNotSupported], [ErrUnsupportedAuth], and
// [ErrResultTooBig]. Custom type codecs ([TypeCodec]) can be registered
// with [RegisterType].
//
// Current limitations: TLS is not yet supported (use sslmode=disable behind a
// VPC/loopback/sidecar); authentication is limited to trust, cleartext, MD5,
// and SCRAM-SHA-256 without channel binding; there is no LISTEN/NOTIFY,
// pipeline mode, or replica routing.
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
// # Transactions on a pool
//
// A transaction needs one connection: use [Pool.BeginTx] or db.BeginTx, or
// db.Conn for statements that must share a connection. A connection returned to
// a pool inside a transaction is discarded and the server rolls the transaction
// back. On the Pool, the call that left it open returns [ErrTxLeftOpen]. On a
// *sql.DB there is no error (a db.Exec("BEGIN") on a pooled connection cannot be
// told from the same statement on a dedicated db.Conn): db.Exec("BEGIN"),
// db.Exec("INSERT ..."), db.Exec("COMMIT") as three calls is not a transaction,
// the INSERT runs on another connection and autocommits, also with
// SetMaxOpenConns(1). That sequence used to work only while one goroutine kept
// reusing the same connection. A connection that comes back inside a transaction
// (open, or failed by a statement) is logged at Warn on slog.Default, the first
// time and then at most once a minute. See the Session state section of
// [Pool].
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
// page: https://goceleris.dev/docs/postgres-types
package postgres
