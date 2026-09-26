// Package h1 implements a zero-copy HTTP/1.1 parser.
//
// This is a low-level parser package consumed by the celeris engine layer
// (engine/epoll and engine/iouring, through internal/conn; the std engine
// uses net/http's parser); application code should not import it directly.
// The parser returns header and body slices that alias the buffer passed to
// [Parser.Reset]: an engine's read buffer, or a per-connection buffer the
// engine gathered the request into. Callers must materialize (clone) any
// value they retain past the next [Parser.ParseRequest] call on the same
// connection.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/engines
package h1
