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
// Server errors surface as [*PGError] (alias of [protocol.PGError]) carrying
// the SQLSTATE code; match with errors.As. Package sentinels include
// [ErrPoolClosed], [ErrClosed], [ErrBadConn], [ErrSSLNotSupported],
// [ErrUnsupportedAuth], and [ErrResultTooBig]. Custom type codecs can be
// registered with [protocol.RegisterType].
//
// Current limitations: TLS is not yet supported (use sslmode=disable behind a
// VPC/loopback/sidecar); authentication is limited to trust, cleartext, MD5,
// and SCRAM-SHA-256 without channel binding; there is no LISTEN/NOTIFY,
// pipeline mode, or replica routing.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/data-stores
package postgres
