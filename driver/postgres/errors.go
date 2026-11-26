package postgres

import (
	"database/sql"
	"errors"
	"io"

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

// ErrClosed is returned from operations on a closed pgConn. It is also what a
// statement that was in flight when the connection went away (the server or the
// network closed it, the connection was closed or discarded, a timeout tore it
// down) matches with errors.Is, together with [io.ErrUnexpectedEOF]: the result
// is cut off and the error says so. Such an error never matches [io.EOF] (the
// end of a result), and never [database/sql/driver.ErrBadConn], so database/sql
// does not retry the statement. When the server sent an ErrorResponse before it
// closed (a FATAL 57P01 after pg_terminate_backend), the error is that
// [*PGError] instead.
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
// 64 rows (64 or more stream) is buffered whole when the call returns and does not count, nor does a
// larger one the connection has already received in full. The wire carries one
// statement at a time, and the rows of the first one are read by the code that
// has not got to them yet: the second statement would wait for the first, which
// only that caller can finish. So the call fails at once instead, before it takes
// any lock or sends anything.
//
// It is what a *database/sql.Tx or *database/sql.Conn returns for Exec, Query,
// QueryRow, Prepare (a statement already in the connection's cache is served: it
// sends nothing) and (Conn) Ping while a Rows of it is open, and what a [Tx] of
// a [Pool] returns for ExecContext, QueryContext, QueryRow, Savepoint,
// ReleaseSavepoint, RollbackToSavepoint and Commit. It matches with errors.Is,
// also through database/sql, which returns the driver's error as it is. It does
// not match [driver.ErrBadConn], so database/sql neither discards the connection
// nor retries the statement (the one exception is ResetSession, which wraps every
// failure with it; database/sql only calls it for a connection no Rows is open
// on). The open Rows is not disturbed: it goes on
// delivering its rows, and Close, or reading it to the end, frees the
// connection for the next statement.
//
// After the ctx of a streaming query is cancelled the Rows must still be closed
// (or read until Next returns false, and then closed) before the next statement
// on that connection: the driver does not end the stream on a cancel. Under
// database/sql Rows.Close is synchronous and ends the sequence cancel, Close,
// next statement cleanly; a statement that comes right after the cancel, with no
// Close, can meet this error for the few moments database/sql needs to close the
// Rows on its own goroutine (Rows.Next already returns false by then), and
// succeeds on a retry after Close. Under database/sql, once the ctx of a query
// that has already returned its first rows ends, Rows.Next returns false, but the
// driver's Close still reads the rest of the reply and sends no cancel: for a long
// result under a deadline, expect Close to take as long as the server takes to
// finish it (celeris#1443). The native Pool's Rows does not look at that ctx at
// all: nothing closes it but the caller.
//
// Before v1.6.0 a statement started over an open Rows either hung for good,
// holding the connection, or returned only after its ctx ended and a 30 s cancel
// timeout, having cancelled the first statement's query and failed the connection.
//
// A *sql.DB (not a Tx or Conn) does not meet it: a second statement takes
// another connection. [Pool] methods outside a Tx do not either (each takes a
// connection of its own, CopyFrom and CopyTo included). On a [Pool] Tx, Commit
// returns it and, like every Commit, ends the Tx: nothing is sent, the
// connection is discarded (the server rolls the transaction back when the
// connection ends), the Tx is rolled back, not committed, and every later call on
// it returns [ErrTxDone], Rollback included. Rollback over an open Rows ends the
// Tx the same way and returns nil. The open Rows then ends with an error that
// matches [ErrClosed] and [io.ErrUnexpectedEOF], never with a clean end: close the
// Rows before Commit. A Pool in direct mode (a bare [Open], or [WithEngine] on a
// server that has started) buffers a result whole before it returns, up to the
// 64 MiB cap of [ErrResultTooBig], so a second statement there works while the
// first Rows is open and this error does not occur.
var ErrRowsOpen = errors.New("celeris-postgres: a Rows is still open on this connection; close it, or read it to the end, before the next statement")

// ErrTxDone is returned by every method of a [Tx] after Commit or Rollback has
// been called on it, whatever that call returned (a Commit that failed ends the
// Tx too). Nothing is sent to the server. It keeps the message the driver always
// had and matches [database/sql.ErrTxDone] with errors.Is, as code written against
// database/sql expects.
var ErrTxDone error = txDoneError{}

type txDoneError struct{}

func (txDoneError) Error() string { return "celeris-postgres: tx already finished" }
func (txDoneError) Unwrap() error { return sql.ErrTxDone }

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
// adaptive) or that runs async handlers; a *sql.DB opened with a [Connector]
// bound by [Connector.WithEngine] to a server that has started (epoll, io_uring,
// adaptive), whose connections are dialled direct (see [Connector.WithEngine]);
// and, rarely, the sync fast path of an event-loop Pool while one poll reads
// past the cap. A Pool opened with [WithEngine] on a server that has not started
// yet (or that runs the std engine without async handlers), and a *sql.DB opened
// with this driver with no engine, on the std engine or bound to a server that
// had not started when the Connector was bound with [Connector.WithEngine], run
// on the event loop: they stream a
// large result and have no cap.
//
// The caller reads the rows that fit under the cap and then gets this error
// from [Rows.Err] (from sql.Rows.Err on a *sql.DB). [Rows.Next] only returns
// false, and [Rows.Close] returns the same error (before v1.6.0 it returned nil),
// so a caller that checks neither sees a truncated result with no other sign:
// always check Rows.Err. A single row larger than the cap fails the query itself:
// the call returns this error and no rows. The rest of the reply is read and
// discarded, so the connection stays usable. The cap is fixed: there is no option
// and no DSN parameter for it.
//
// To read a larger result paginate the query with LIMIT/OFFSET or keyset
// paging; a cursor (DECLARE and FETCH n) inside a transaction does not buffer
// the whole result either, and neither does [Pool.CopyTo] on a Pool. A Pool on
// the event loop (WithEngine on a server that has not started yet) streams as
// well. Before v1.6.0 a direct-mode Pool buffered the whole reply (several times
// its size in memory) before it returned this error.
var ErrResultTooBig = errors.New("celeris-postgres: query result exceeds direct-mode buffer cap (64 MiB); paginate the query")

// errMultiResultSets is the error of a row-returning query (Query, QueryRow,
// QueryContext, on a Pool, a Tx or a *sql.DB) whose text holds several
// statements of which more than one returns rows. The driver returns one result
// set; before this error it returned the first set's rows under the LAST set's
// column names (the second set's rows appended when the shapes matched). The
// server has run every statement (a multi-statement simple query is one
// implicit transaction, committed unless a statement failed); the error is
// raised once the whole reply is read, and the connection is ready for the next
// query. A query string whose statements return rows in only one of them (SET
// then SELECT, SELECT then UPDATE, INSERT then SELECT) is not affected, and Exec
// accepts any number of statements. Unexported: the error text is the contract.
var errMultiResultSets = errors.New("celeris-postgres: multiple result sets are not supported; the server has executed the statements")

// ErrDirectModeUnsupported is returned by operations that rely on
// unsolicited server messages between queries — LISTEN/UNLISTEN/NOTIFY
// (NotificationResponse) most notably. Direct-mode conns dial a plain
// net.TCPConn and drive reads from the caller goroutine only during
// an active query; between queries no reader is active, so any
// async-delivered message is silently dropped. COPY FROM/TO is
// supported via a short-lived per-call reader goroutine; only the
// persistent-listener class of operations returns this error.
//
// A *sql.DB opened with a [Connector] bound to a started epoll, io_uring or
// adaptive server gets direct-mode connections too (see [Connector.WithEngine]),
// so LISTEN and UNLISTEN return this error there.
//
// Workarounds: run the listener on the driver's own event loop, which has an
// always-on reader: a [Pool] opened with [WithEngine] on a server that has not
// started yet (and runs no async handlers), or a [Connector] with no engine or
// bound with [Connector.WithEngine] before the server starts. A Connector bound
// to a started epoll, io_uring or adaptive server does not help.
var ErrDirectModeUnsupported = errors.New("celeris-postgres: LISTEN/UNLISTEN/NOTIFY are not supported in direct mode; " +
	"use a Pool on a server that has not started, or a Connector with no engine or bound before the server starts")

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

// connLostError is the error of a request that the connection's end cut off
// before its ReadyForQuery. It matches [ErrClosed] and [io.ErrUnexpectedEOF]
// with errors.Is, and the cause (a reset, a timeout, a protocol error) when
// there is one. It never matches [io.EOF]: database/sql and [Rows.Next] read
// that as the end of a result. Its type is not exported; the sentinels are the
// API.
type connLostError struct {
	cause error
}

func (e *connLostError) Error() string {
	if e.cause == nil {
		return "celeris-postgres: connection closed before the result was complete: unexpected EOF"
	}
	return "celeris-postgres: connection closed before the result was complete: " + e.cause.Error()
}

func (e *connLostError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrClosed, io.ErrUnexpectedEOF}
	}
	return []error{ErrClosed, io.ErrUnexpectedEOF, e.cause}
}

// lostError returns the error for a request cut off by the end of the
// connection with cause. An io.EOF cause (the peer's FIN) is dropped, so the
// result is never mistaken for a clean end. An error that is already a
// connLostError or a server error is returned as it is.
func lostError(cause error) error {
	switch cause {
	case nil, io.EOF, io.ErrUnexpectedEOF, ErrClosed:
		return &connLostError{}
	}
	var le *connLostError
	if errors.As(cause, &le) {
		return cause
	}
	var pe *protocol.PGError
	if errors.As(cause, &pe) {
		return cause
	}
	if errors.Is(cause, io.EOF) {
		return &connLostError{}
	}
	return &connLostError{cause: cause}
}

// fatalServerError reports whether e ends the connection: a FATAL or PANIC
// severity (the non-localized V field when present) or an operator
// intervention shutdown (SQLSTATE 57P01, 57P02, 57P03).
func fatalServerError(e *protocol.PGError) bool {
	if e == nil {
		return false
	}
	sev := e.Severity
	if v := e.Extra['V']; v != "" {
		sev = v
	}
	if sev == "FATAL" || sev == "PANIC" {
		return true
	}
	switch e.Code {
	case "57P01", "57P02", "57P03":
		return true
	}
	return false
}
