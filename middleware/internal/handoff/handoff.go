// Package handoff copies what a coalescing leader hands the requests that
// waited on it: the error its handler returned and the value it panicked
// with. middleware/singleflight and middleware/cache's coalesced fill
// (through middleware/internal/sf) use it.
//
// On epoll and io_uring (and Adaptive, which runs them) request strings are
// views of the leader connection's receive buffer. The leader's request ends
// once it has handed its result over, and the engine receives that
// connection's next request into the buffer (or, once it closes, gives the
// buffer to another connection). A waiter formats the error or the panic
// value later, on its own connection: in its logger, its span, its recovery
// middleware, or the router answering an [celeris.HTTPError]. An error built
// from a request string, errors.New(c.Param("id")) or
// celeris.NewHTTPError(400, c.Header("x-name")), would then read another
// request's bytes, possibly another client's (celeris#732).
package handoff

import (
	"errors"
	"strings"

	"github.com/goceleris/celeris"
)

// Error returns err for a request other than the one whose handler returned
// it, while that request is still running. The result's message is a copy
// of err's, taken now. It unwraps to err, so [errors.Is] finds what err's
// chain holds, sentinels included. [errors.As] to a *[celeris.HTTPError]
// finds a copy of the first one in err's chain, whose Message is a copy too
// and whose wrapped error is handed off in turn; [errors.As] to any other
// type finds err's own value, whose fields are shared. The result is not
// err itself: == and a type assertion on it do not match err. A nil err
// stays nil.
//
// An Error method that panics (a typed nil pointer returned as an error,
// say) does not panic here: Error returns err itself, which the waiter then
// formats as before. The leader calls Error between taking its waiters and
// releasing them, so a panic here would never release them.
func Error(err error) (out error) {
	if err == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			out = err
		}
	}()
	h := &handedOff{msg: strings.Clone(err.Error()), err: err}
	var he *celeris.HTTPError
	if errors.As(err, &he) && he != nil {
		h.http = &celeris.HTTPError{Code: he.Code, Message: strings.Clone(he.Message), Err: Error(he.Err)}
	}
	return h
}

// Panic returns a panic value for a request other than the one that
// panicked with it: a copy of a string, an error handed off as by [Error],
// and any other value as it is.
func Panic(v any) any {
	switch p := v.(type) {
	case string:
		return strings.Clone(p)
	case error:
		return Error(p)
	}
	return v
}

// handedOff is an error whose message was copied while the request that
// produced it was running.
type handedOff struct {
	msg  string
	err  error
	http *celeris.HTTPError
}

func (e *handedOff) Error() string { return e.msg }

func (e *handedOff) Unwrap() error { return e.err }

// As lets errors.As to a *celeris.HTTPError find the copy, not the
// original, whose Message may be a view.
func (e *handedOff) As(target any) bool {
	if p, ok := target.(**celeris.HTTPError); ok && e.http != nil {
		*p = e.http
		return true
	}
	return false
}
