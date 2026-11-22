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
// [WithEngine] is optional and affects performance only: when set, the pool
// shares the HTTP server's event loop instead of resolving a standalone one.
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
// # Startup parameters and connection poolers
//
// The text result format the unnamed and simple query paths read depends on
// three session settings, so the driver sends extra_float_digits=3,
// DateStyle=ISO and bytea_output=hex in the StartupMessage (it overrides an
// ALTER ROLE or ALTER DATABASE setting) unless the DSN sets them itself. A
// connection pooler may refuse a startup parameter it does not track:
// PgBouncer 1.26.1 answers "unsupported startup parameter: extra_float_digits"
// (or bytea_output) and closes the connection, and accepts DateStyle. When the
// server answers the startup with an error that names one of them, the driver
// drops that parameter and dials again (once per parameter, remembered per
// address for the life of the process, and each attempt has its own
// connect_timeout); startup_pins=off in the DSN sends none. A bytea read into a
// []byte is decoded from either the hex or the escape form, so a dropped
// bytea_output costs nothing there. extra_float_digits is the one that does:
// behind a pooler it is not applied to the client's session (not in session
// mode, not in transaction mode), so with a role or database that sets it to 0
// or less float8 and float4 columns read back rounded on those paths (0.3 for
// 0.30000000000000004). Leave it at its default (1 since PostgreSQL 12). The
// server does not report it in ParameterStatus, so the driver cannot tell.
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
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/data-stores
package postgres
