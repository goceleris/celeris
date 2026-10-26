package celeris

import (
	"errors"
	"strconv"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// ErrNoCookie is returned by Context.Cookie when the named cookie is not present.
var ErrNoCookie = errors.New("celeris: named cookie not present")

// ErrResponseWritten is returned when a response method is called after
// a response has already been written.
var ErrResponseWritten = errors.New("celeris: response already written")

// ErrEmptyBody is returned by Bind, BindJSON, and BindXML when the request
// body is empty.
var ErrEmptyBody = errors.New("celeris: empty request body")

// ErrInvalidRedirectCode is returned by [Context.Redirect] when the status
// code is not in the range 300–308.
var ErrInvalidRedirectCode = errors.New("celeris: redirect status code must be 3xx")

// ErrHijackNotSupported is returned by Hijack when the connection does not
// support takeover (e.g. HTTP/2 multiplexed streams).
var ErrHijackNotSupported = stream.ErrHijackNotSupported

// ErrAcceptControlNotSupported is returned by PauseAccept/ResumeAccept when
// the active engine does not implement accept control (e.g. std engine).
var ErrAcceptControlNotSupported = errors.New("celeris: engine does not support accept control")

// ErrDetached is returned by response methods (JSON, Blob, XML, HTML, String,
// File, StreamReader) when the Context has been detached. After Detach(), the
// connection is owned by streaming middleware (WebSocket frames, SSE event
// stream, etc.) and the standard response writers would corrupt the stream.
// Use [Context.StreamWriter] or the relevant middleware's write API instead.
var ErrDetached = errors.New("celeris: cannot write standard response on detached context")

// ErrStreamRefused is what a [StreamWriter] write returns on an HTTP/1.1
// connection of the epoll, io_uring or Adaptive engine once the engine has
// refused bytes of the response: the response staged and not yet sent is at
// the engine's 64 MiB limit, so the response is cut and the connection is
// closed once what was staged has gone out. [StreamWriter.Write],
// [StreamWriter.WriteHeader], [StreamWriter.Flush] and [StreamWriter.Close]
// all return it, the one that hit the limit and each one after it. Test for it
// with errors.Is.
//
// Before [Context.Detach] a Write that would take the response past the limit
// stages nothing of its chunk. After Detach the Write that finds the unsent
// bytes over the limit is dropped; the one that took them over was staged, so
// the unsent bytes are bounded at the limit plus one chunk.
//
// It is not an error of a connection that went away: that one wraps
// [net.ErrClosed], so a handler can tell a peer that left (stop quietly) from
// a response the engine cut (the client sees a body that never ends, and the
// handler may want to log it). The std engine and HTTP/2 do not return it:
// they report their own errors.
var ErrStreamRefused = conn.ErrWriteRefused

// ErrUnauthorized is the canonical 401 [HTTPError] used by all auth-style
// middleware (jwt, keyauth, basicauth). Each package re-exports it under
// the same name so callers can `errors.Is(err, celeris.ErrUnauthorized)`
// across mixed auth stacks.
var ErrUnauthorized = NewHTTPError(401, "Unauthorized")

// ErrServiceUnavailable is the canonical 503 [HTTPError] used by
// infrastructure middleware that sheds load (timeout, circuitbreaker,
// ratelimit). Re-exported by each package so callers can match across
// stacks with errors.Is.
var ErrServiceUnavailable = NewHTTPError(503, "Service Unavailable")

// HTTPError is a structured error that carries an HTTP status code.
// Handlers return HTTPError to signal a specific status code to the
// routerAdapter safety net. Use NewHTTPError to create one.
type HTTPError struct {
	// Code is the HTTP status code (e.g. 400, 404, 500).
	Code int
	// Message is a human-readable error description sent in the response body.
	Message string
	// Err is an optional wrapped error for use with errors.Is / errors.As.
	Err error
}

// Error returns a string representation including the status code and message.
func (e *HTTPError) Error() string {
	// Direct concat with a 128-byte stack buffer avoids fmt.Sprintf's
	// formatter-state alloc on the hot auth-deny path, where every
	// rejected request flows through HTTPError.Error during logging.
	var buf [128]byte
	dst := append(buf[:0], "code="...)
	dst = strconv.AppendInt(dst, int64(e.Code), 10)
	dst = append(dst, ", message="...)
	dst = append(dst, e.Message...)
	if e.Err != nil {
		dst = append(dst, ", err="...)
		dst = append(dst, e.Err.Error()...)
	}
	return string(dst)
}

// Unwrap returns the wrapped error for use with errors.Is and errors.As.
func (e *HTTPError) Unwrap() error { return e.Err }

// WithError sets the wrapped error and returns the HTTPError for chaining.
func (e *HTTPError) WithError(err error) *HTTPError {
	e.Err = err
	return e
}

// NewHTTPError creates an HTTPError with the given status code and message.
// To wrap an existing error, use the WithError method on the returned HTTPError.
func NewHTTPError(code int, message string) *HTTPError {
	return &HTTPError{Code: code, Message: message}
}
