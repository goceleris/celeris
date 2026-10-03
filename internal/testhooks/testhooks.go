// Package testhooks gives the module's test infrastructure (celeristest,
// and the tests of the middleware) access to the internals of a
// [github.com/goceleris/celeris.Context]. The root package sets every
// variable in its init, so any package that has a *celeris.Context to pass
// in can call them.
//
// testhooks cannot import the root package, which imports it. A parameter
// or result that is a *celeris.Context is therefore typed any, and so is a
// []celeris.HandlerFunc chain. Each hook panics when it is given a value of
// any other type.
//
// End-user tests use celeristest, whose NewContext, NewContextT and With*
// options build the Context and its Stream, set up the recorder, and
// register the cleanup.
package testhooks

import (
	"net"
	"time"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

var (
	// AcquireContext returns a *celeris.Context from the pool, bound to s.
	AcquireContext func(s *stream.Stream) any

	// ReleaseContext returns a *celeris.Context to the pool, firing its
	// OnRelease callbacks.
	ReleaseContext func(c any)

	// Stream returns the stream of a *celeris.Context, or nil.
	Stream func(c any) *stream.Stream

	// SetStartTime sets the start time of a *celeris.Context.
	SetStartTime func(c any, t time.Time)

	// SetFullPath sets the full path of a *celeris.Context.
	SetFullPath func(c any, path string)

	// SetTrustedNets sets the trusted proxy networks of a *celeris.Context.
	SetTrustedNets func(c any, nets []*net.IPNet)

	// AddParam appends a route parameter to a *celeris.Context.
	AddParam func(c any, key, value string)

	// SetHandlers installs handlers, a []celeris.HandlerFunc, as the
	// handler chain of a *celeris.Context.
	SetHandlers func(c any, handlers any)

	// SetScheme sets the scheme override of a *celeris.Context.
	SetScheme func(c any, scheme string)
)
