package timeout

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/goceleris/celeris"
)

var chanPool = sync.Pool{New: func() any { return make(chan error, 1) }}

// New creates a timeout middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	dur := cfg.Timeout
	timeoutFunc := cfg.TimeoutFunc
	errHandler := cfg.ErrorHandler
	preemptive := cfg.Preemptive
	timeoutErrors := cfg.TimeoutErrors

	if preemptive {
		return preemptiveHandler(skipMap, dur, timeoutFunc, errHandler, cfg.Skip, timeoutErrors)
	}

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		d := dur
		if timeoutFunc != nil {
			if td := timeoutFunc(c); td > 0 {
				d = td
			}
		}

		childCtx, cancel := context.WithTimeout(c.Context(), d)
		defer cancel()

		origCtx := c.Context()
		c.SetContext(childCtx)
		defer c.SetContext(origCtx)

		err := c.Next()

		if childCtx.Err() == context.DeadlineExceeded {
			if c.IsWritten() {
				return ErrServiceUnavailable
			}
			return safeErrHandler(c, context.DeadlineExceeded, errHandler)
		}

		if err != nil && isTimeoutError(err, timeoutErrors) {
			if c.IsWritten() {
				return ErrServiceUnavailable
			}
			return safeErrHandler(c, err, errHandler)
		}

		return err
	}
}

// safeErrHandler calls the error handler with panic recovery. If the error
// handler itself panics, a generic 503 is returned (mirroring the nested
// recovery pattern used by recovery middleware).
func safeErrHandler(c *celeris.Context, triggerErr error, handler func(*celeris.Context, error) error) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = ErrServiceUnavailable
		}
	}()
	return handler(c, triggerErr)
}

// isTimeoutError checks if err matches any of the configured timeout-like errors.
func isTimeoutError(err error, timeoutErrors []error) bool {
	for _, te := range timeoutErrors {
		if errors.Is(err, te) {
			return true
		}
	}
	return false
}

// flushOrReturn handles the error handler result in preemptive mode.
// If the error handler wrote a buffered response (returned nil), flush it.
// If it returned an error, propagate the error without flushing.
func flushOrReturn(c *celeris.Context, err error) error {
	if err != nil {
		return err
	}
	return c.FlushResponse()
}

// Concurrency invariant for preemptive mode:
//
// The handler goroutine and the middleware goroutine share a single
// *celeris.Context. Safety is ensured by the done channel acting as a
// synchronization barrier:
//
//   - The handler goroutine sends exactly one value on done (either the
//     handler's return error or a panic-wrapped error), then exits.
//   - The middleware goroutine blocks on either <-done or <-childCtx.Done().
//   - On the timeout path (<-childCtx.Done()), the middleware immediately
//     blocks on <-done, waiting for the handler goroutine to finish. Only
//     after <-done returns does the middleware access c again.
//
// This guarantees: the middleware never accesses c after <-done returns
// concurrently with the handler goroutine, ensuring no concurrent Context
// access post-return.
func preemptiveHandler(
	skipMap map[string]struct{},
	dur time.Duration,
	timeoutFunc func(*celeris.Context) time.Duration,
	errHandler func(*celeris.Context, error) error,
	skip func(*celeris.Context) bool,
	timeoutErrors []error,
) celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		if skip != nil && skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		d := dur
		if timeoutFunc != nil {
			if td := timeoutFunc(c); td > 0 {
				d = td
			}
		}

		childCtx, cancel := context.WithTimeout(c.Context(), d)
		defer cancel()

		origCtx := c.Context()
		c.SetContext(childCtx)
		defer c.SetContext(origCtx)

		c.BufferResponse()

		done := chanPool.Get().(chan error)
		// Drain any stale value left from a previous pool cycle.
		select {
		case <-done:
		default:
		}

		go func() {
			defer func() {
				if r := recover(); r != nil {
					done <- celeris.NewHTTPError(500, fmt.Sprintf("panic: %v", r))
				}
			}()
			done <- c.Next()
		}()

		select {
		case err := <-done:
			chanPool.Put(done)
			if err != nil && isTimeoutError(err, timeoutErrors) {
				return flushOrReturn(c, safeErrHandler(c, err, errHandler))
			}
			if flushErr := c.FlushResponse(); flushErr != nil {
				return flushErr
			}
			return err
		case <-childCtx.Done():
			// See concurrency invariant above: <-done serializes access
			// to c. The handler goroutine has exited once <-done returns.
			<-done
			chanPool.Put(done)
			// The handler may have buffered a stale response. We do NOT
			// need to explicitly discard it: errHandler's response methods
			// (JSON, String, etc.) overwrite the captured fields while
			// bufferDepth > 0, replacing the stale data. flushOrReturn
			// then flushes the errHandler's response (or propagates its
			// error without flushing).
			return flushOrReturn(c, safeErrHandler(c, context.DeadlineExceeded, errHandler))
		}
	}
}
