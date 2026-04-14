// Package h1 implements a zero-copy HTTP/1.1 parser.
//
// This is a low-level parser package consumed by the celeris engine layer
// (engine/iouring, engine/epoll, engine/std bridge); application code
// should not import it directly. The parser returns header and body slices
// that alias the connection's read buffer — callers must materialize
// (clone) any value they retain past the next [ParseRequest] call on the
// same connection.
package h1
