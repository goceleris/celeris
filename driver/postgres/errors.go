package postgres

import (
	"errors"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// ErrSSLNotSupported is returned from Connect / Open when the DSN requests
// sslmode=require, verify-ca, or verify-full. Plaintext (sslmode=disable or
// prefer) is the only mode the driver currently supports.
var ErrSSLNotSupported = errors.New("celeris-postgres: TLS/SSL is not yet supported; use sslmode=disable for VPC/loopback deployments, or terminate TLS at a sidecar / VPC boundary")

// ErrUnsupportedAuth is returned when the server demands an authentication
// method this driver cannot fulfill (for example GSS, SSPI, or a SASL
// mechanism other than SCRAM-SHA-256).
var ErrUnsupportedAuth = errors.New("celeris-postgres: unsupported authentication method")

// ErrClosed is returned from operations on a closed pgConn.
var ErrClosed = errors.New("celeris-postgres: connection closed")

// ErrTxCommitRollback is returned from Commit when PostgreSQL answered COMMIT
// with the command tag ROLLBACK: the transaction was in the aborted state (a
// statement of it failed, or was cancelled, and the transaction was not rolled
// back), the server treats such a COMMIT as a ROLLBACK and reports no error, and
// nothing was committed. The transaction is over and the connection is free
// again. It matches with errors.Is, also through database/sql, whose Tx.Commit
// returns the driver's error as it is.
var ErrTxCommitRollback = errors.New("celeris-postgres: commit unexpectedly resulted in rollback")

// ErrBadConn is returned when a connection is in an unusable state; returning
// it causes database/sql to evict the conn and open a fresh one.
var ErrBadConn = errors.New("celeris-postgres: bad connection")

// ErrTxLeftOpen is returned by [Pool.ExecContext], [Rows.Close] and [Row.Scan]
// when the statement left a transaction open on the pooled connection (for
// example Exec "BEGIN"): the connection was discarded, so the server rolled the
// transaction back, and nothing that came after it ran inside it. A
// transaction that spans several statements needs [Pool.BeginTx]. It is not
// returned for a *sql.DB opened with this driver, which cannot tell a pooled
// db.Exec("BEGIN") from the same statement on a dedicated *sql.Conn: there the
// connection is dropped without an error.
var ErrTxLeftOpen = errors.New("celeris-postgres: the statement left a transaction open on a pooled connection; " +
	"the connection was discarded and the transaction rolled back; use BeginTx")

// ErrResultTooBig is the error of a query whose cumulative DataRow payload
// exceeds the result buffer cap, 64 MiB by default, on a path that buffers the
// reply whole. Those paths are the direct connections: a *sql.DB opened with this
// driver (a [Connector] without [Connector.WithStreaming], which includes
// sql.Open), a direct-mode [Pool] (a bare [Open](dsn), or [WithEngine] on a
// server that has started (epoll, io_uring, adaptive) or that runs async
// handlers); and, rarely, the sync fast path of an event-loop Pool while one poll
// reads past the cap. A Pool opened with [WithEngine] on a server that has not
// started yet (or that runs the std engine without async handlers) and a
// Connector with WithStreaming stream a large result and have no cap.
//
// The cap is a setting: [WithMaxResultBytes] for a Pool and
// [Connector.WithMaxResultBytes] for database/sql. It bounds memory, so it
// cannot be turned off (a value below 1 is an error); raise it to a size the
// process can hold. There is no DSN parameter for it.
//
// The caller reads the rows that fit under the cap and then gets this error
// from [Rows.Err]. [Rows.Next] only returns false and [Rows.Close] returns nil,
// so a caller that does not check Rows.Err sees a truncated result with no other
// sign: always check it (database/sql.Rows.Err likewise). A single row larger
// than the cap fails the query itself: the call returns this error and no rows.
// The rest of the reply is read and discarded, so the connection stays usable.
//
// To read a larger result use [Pool.CopyTo], a cursor (DECLARE and FETCH n)
// inside [Pool.BeginTx] or a database/sql transaction, a Pool on the event loop
// (WithEngine on a server that has not started yet), or [Connector.WithStreaming],
// all of which stream; or paginate the query with LIMIT/OFFSET or keyset paging.
// Before v1.6.0 a direct-mode Pool buffered the whole reply (several times its
// size in memory) before it returned this error, and a *sql.DB had no cap.
var ErrResultTooBig = errors.New("celeris-postgres: query result exceeds the result size cap (64 MiB by default; raise it with WithMaxResultBytes, stream it with Connector.WithStreaming, or paginate the query)")

// ErrDirectModeUnsupported is returned by LISTEN and UNLISTEN on a direct
// connection: the default of database/sql (a [Connector] without
// [Connector.WithStreaming]) and of a [Pool] without an event loop. The driver has
// no API to receive a notification on any path: a NotificationResponse is read and
// dropped. On the event loop a LISTEN is accepted and its notifications are
// dropped; a direct connection refuses the statement, so that it does not look as
// if it worked. NOTIFY, which only sends, works on every path. The error text
// names the way back to the old behaviour for database/sql,
// [Connector.WithStreaming] (or streaming=true in the DSN).
//
// Receiving notifications needs a dedicated listener connection of another
// package; this driver does not offer one.
var ErrDirectModeUnsupported = errors.New("celeris-postgres: LISTEN and UNLISTEN are not supported on a direct connection (the default of database/sql and of a Pool without an event loop), and the driver has no way to receive notifications on any connection; Connector.WithStreaming (or streaming=true in the DSN) accepts the statement as before and drops the notifications")

// PGError is a server-sent ErrorResponse. Match it with errors.As.
//
// Its fields are the ErrorResponse fields the driver decodes: Severity,
// Code (the SQLSTATE), Message, Detail and Hint (all string), Position
// (int; the 1-based character position in the query, 0 if absent), and
// Extra (map[byte]string), which holds every other field by its one-byte
// code.
type PGError = protocol.PGError

// isPreparedStatementNotFound reports whether err is a PG error with SQLSTATE
// 26000 (invalid_sql_statement_name), which the server returns when a Bind
// references a prepared statement that has been deallocated (e.g. by DISCARD
// ALL or DEALLOCATE ALL issued on pool return).
func isPreparedStatementNotFound(err error) bool {
	var pgErr *protocol.PGError
	return errors.As(err, &pgErr) && pgErr.Code == "26000"
}
