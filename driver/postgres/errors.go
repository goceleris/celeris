package postgres

import (
	"errors"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// ErrSSLNotSupported is returned from Connect / Open when the DSN requests
// sslmode=require, verify-ca, or verify-full. Plaintext (sslmode=disable or
// prefer) is the only supported mode in v1.4.0.
var ErrSSLNotSupported = errors.New("celeris-postgres: TLS/SSL not supported in v1.4.0; use sslmode=disable for VPC/loopback deployments or wait for v1.4.x TLS support")

// ErrUnsupportedAuth is returned when the server demands an authentication
// method this driver cannot fulfill (for example GSS, SSPI, or a SASL
// mechanism other than SCRAM-SHA-256).
var ErrUnsupportedAuth = errors.New("celeris-postgres: unsupported authentication method")

// ErrClosed is returned from operations on a closed pgConn.
var ErrClosed = errors.New("celeris-postgres: connection closed")

// ErrBadConn is returned when a connection is in an unusable state; returning
// it causes database/sql to evict the conn and open a fresh one.
var ErrBadConn = errors.New("celeris-postgres: bad connection")

// PGError re-exports the server-side ErrorResponse type so callers can
// type-assert without importing the protocol package.
type PGError = protocol.PGError

// isPreparedStatementNotFound reports whether err is a PG error with SQLSTATE
// 26000 (invalid_sql_statement_name), which the server returns when a Bind
// references a prepared statement that has been deallocated (e.g. by DISCARD
// ALL or DEALLOCATE ALL issued on pool return).
func isPreparedStatementNotFound(err error) bool {
	var pgErr *protocol.PGError
	return errors.As(err, &pgErr) && pgErr.Code == "26000"
}
