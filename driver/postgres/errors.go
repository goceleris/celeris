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

// ErrRowsOpen is returned by a statement started on a connection that still has
// a Rows open: a result that is streaming (the server has not finished sending
// it) and whose consumer has not read it to the end or closed it. A result under
// 64 rows is buffered whole when the call returns and does not count, nor does a
// larger one the connection has already received in full. The wire carries one
// statement at a time, and the first statement's rows are read by the code that
// has not got to them yet: answering the second one would wait for the first,
// which only that caller can finish, so the call fails at once instead, before it
// takes any lock or writes anything, and nothing is sent to the server.
//
// It is reported by every call that would start a request on the connection:
// Exec, Query, QueryRow, Prepare, Ping, BeginTx, Commit, Rollback and
// Savepoint on a [Tx], a [database/sql.Tx], a [database/sql.Conn] or a [Pool]
// transaction, and CopyFrom and CopyTo on a connection that is held. It matches
// with errors.Is, also through database/sql, which returns the driver's error as
// it is. It does not wrap [driver.ErrBadConn], so database/sql does not discard the
// connection nor retry the statement. The open Rows is not disturbed: it goes on
// delivering its rows, and Close, or reading it to the end, frees the connection
// for the next statement.
//
// A *sql.DB (a pool, not a Tx or Conn) does not meet it: a second statement takes
// another connection. [Pool] methods outside a Tx do not either. On a [Pool] Tx,
// Commit returns it with the Tx still open (close the Rows and Commit again), and
// Rollback does not: it discards the connection (the server rolls the transaction
// back when the connection ends) and returns nil; the open Rows then ends with an
// error. Direct mode (a [Pool] opened without [WithEngine], or on a server that
// has started) buffers the whole result before it returns, so a second statement
// there works while the first Rows is open and this error does not occur.
var ErrRowsOpen = errors.New("celeris-postgres: a Rows is still open on this connection; close it, or read it to the end, before the next statement")

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
// exceeds the result buffer cap, 64 MiB (maxDirectResultBytes), on a path that
// buffers the reply whole. Those paths are a direct-mode [Pool]: a bare
// [Open](dsn), or [WithEngine] on a server that has started (epoll, io_uring,
// adaptive) or that runs async handlers; and, rarely, the sync fast path of an
// event-loop Pool while one poll reads past the cap. A Pool opened with
// [WithEngine] on a server that has not started yet (or that runs the std
// engine without async handlers) and a *sql.DB opened with this driver stream a
// large result and have no cap.
//
// The caller reads the rows that fit under the cap and then gets this error
// from [Rows.Err]. [Rows.Next] only returns false and [Rows.Close] returns nil,
// so a caller that does not check Rows.Err sees a truncated result with no other
// sign: always check it. A single row larger than the cap fails the query
// itself: the call returns this error and no rows. The rest of the reply is
// read and discarded, so the connection stays usable. The cap is fixed: there
// is no option and no DSN parameter for it.
//
// To read a larger result use [Pool.CopyTo], a cursor (DECLARE and FETCH n)
// inside [Pool.BeginTx], a Pool on the event loop (WithEngine on a server that
// has not started yet), or database/sql, all of which stream; or paginate the
// query with LIMIT/OFFSET or keyset paging. Before v1.6.0 a direct-mode Pool
// buffered the whole reply (several times its size in memory) before it
// returned this error.
var ErrResultTooBig = errors.New("celeris-postgres: query result exceeds direct-mode buffer cap (64 MiB); paginate or use streaming mode")

// ErrDirectModeUnsupported is returned by operations that rely on
// unsolicited server messages between queries — LISTEN/UNLISTEN/NOTIFY
// (NotificationResponse) most notably. Direct-mode conns dial a plain
// net.TCPConn and drive reads from the caller goroutine only during
// an active query; between queries no reader is active, so any
// async-delivered message is silently dropped. COPY FROM/TO is
// supported via a short-lived per-call reader goroutine; only the
// persistent-listener class of operations returns this error.
//
// Workarounds: open the pool against an engine with AsyncHandlers=false
// (drivers will pick the mini-loop path which has an always-on reader),
// or use a separate non-pooled listener conn dedicated to
// LISTEN/NOTIFY in a non-async configuration.
var ErrDirectModeUnsupported = errors.New("celeris-postgres: LISTEN/UNLISTEN/NOTIFY are not supported in direct mode; use a non-async engine pool or a dedicated listener conn")

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
