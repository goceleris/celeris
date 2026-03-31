package celeris

import (
	"errors"
	"fmt"

	"github.com/goceleris/celeris/protocol/h2/stream"
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
	if e.Err != nil {
		return fmt.Sprintf("code=%d, message=%s, err=%v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("code=%d, message=%s", e.Code, e.Message)
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
