// Package std provides an engine implementation backed by net/http.
package std

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"

	"golang.org/x/net/http2"
	//nolint:staticcheck // SA1019: h2c (and, since x/net 0.59, http2.Server) is deprecated, and its replacement
	// (http.Server.Protocols + SetUnencryptedHTTP2) does NOT cover the
	// RFC 7540 3.2 Upgrade handshake -- net/http implements the upgrade path
	// internally but exposes no way for a caller to reach it. celeris#440
	// migrated to the stdlib and silently dropped h2c Upgrade from this
	// engine; the epoll and io_uring engines still honour it, and the
	// nightly caught the asymmetry. Until the stdlib exposes an equivalent,
	// this package is the only way to keep the three engines agreeing.
	"golang.org/x/net/http2/h2c"
)

// Engine wraps net/http.Server to implement the engine.Engine interface.
type Engine struct {
	server   *http.Server
	listener atomic.Pointer[net.Listener]
	handler  stream.Handler
	cfg      resource.Config
	logger   *slog.Logger
	// baseCtx parents every request context this server hands out
	// (http.Server.BaseContext); baseCancel is Shutdown's lever for
	// reaching handlers that are still running when the drain budget
	// runs out. See Shutdown.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	metrics    struct {
		reqCount    atomic.Uint64
		activeConns atomic.Int64
		errCount    atomic.Uint64
		// closeCount is the cumulative close count EngineMetrics declares
		// and this engine used to leave at zero (celeris#624). It is
		// incremented on exactly the ConnState transitions that decrement
		// activeConns, so engine_closed - hook_closed is identically zero
		// here — which is what makes std the control engine when the same
		// difference is nonzero on epoll or io_uring.
		closeCount atomic.Uint64
	}
	once sync.Once
}

// New creates a new StdEngine.
func New(cfg resource.Config, handler stream.Handler) (*Engine, error) {
	cfg = cfg.WithDefaults()

	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("config validation: %w", errs[0])
	}

	e := &Engine{
		handler: handler,
		cfg:     cfg,
		logger:  cfg.Logger,
	}
	e.baseCtx, e.baseCancel = context.WithCancel(context.Background())

	bridge := &Bridge{engine: e, handler: handler}

	// h2c through the deprecated handler rather than http.Protocols,
	// because it is the only one of the two that performs the RFC 7540 3.2
	// Upgrade handshake. See the import comment: dropping it took the
	// std engine out of step with epoll and io_uring, which both honour
	// Config.EnableH2Upgrade, and made the validation harness's h2c-churn
	// slice vacuous on std -- 3,597 upgrade preambles, not one 101.
	//
	// h2c.NewHandler serves BOTH shapes: a client that opens with the
	// HTTP/2 preface (prior knowledge) and one that asks to upgrade from
	// HTTP/1.1. Plain HTTP/1.1 requests fall through to the wrapped
	// handler, so an H2C listener still serves H1.
	var httpHandler http.Handler = bridge
	if cfg.Protocol == engine.H2C || cfg.Protocol == engine.Auto {
		//
		// x/net 0.59 deprecated http2.Server along with the handler; it is
		// still the only type h2c.NewHandler accepts, so the two carry the
		// same waiver. Keep the literal on one line so the waiver covers it.
		h2s := &http2.Server{MaxConcurrentStreams: cfg.MaxConcurrentStreams, MaxReadFrameSize: cfg.MaxFrameSize} //nolint:staticcheck // SA1019: the type h2c.NewHandler takes; see the import comment.
		httpHandler = h2c.NewHandler(bridge, h2s)                                                                //nolint:staticcheck // SA1019: see the import comment -- no stdlib equivalent covers Upgrade.
	}

	e.server = &http.Server{
		Addr:    cfg.Addr,
		Handler: httpHandler,
		// stdTimeout, not the raw value: celeris carries "disabled" as 0
		// and net/http does NOT read 0 as disabled on every field. See
		// stdTimeout (celeris#594).
		ReadTimeout:       stdTimeout(cfg.ReadTimeout),
		ReadHeaderTimeout: stdTimeout(cfg.ReadHeaderTimeout),
		WriteTimeout:      stdTimeout(cfg.WriteTimeout),
		IdleTimeout:       stdTimeout(cfg.IdleTimeout),
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ConnState:         e.connStateHook,
		BaseContext:       func(net.Listener) context.Context { return e.baseCtx },
	}

	return e, nil
}

// stdTimeout translates celeris's internal "disabled" encoding into the value
// net/http actually treats as disabled for the field it is assigned to.
//
// resource.Config.WithDefaults resolves the documented -1 "no timeout"
// sentinel to 0, because 0 is what every celeris consumer reads as off (the
// `> 0` guards in the iouring and epoll loops). net/http is not uniform about
// 0 (go1.27 src/net/http/server.go):
//
//	ReadTimeout        zero OR negative => no timeout
//	                   (readRequest, server.go:1039: `if d := c.server.ReadTimeout; d > 0`)
//	WriteTimeout       zero OR negative => no timeout
//	                   (readRequest, server.go:1042: `if d := c.server.WriteTimeout; d > 0`)
//	ReadHeaderTimeout  zero => FALL BACK TO ReadTimeout; negative => no timeout
//	                   (Server.readHeaderTimeout, server.go:3752, consumed at
//	                   server.go:2038 and :2177 with `d > 0`)
//	IdleTimeout        zero => FALL BACK TO ReadTimeout; negative => no timeout
//	                   (Server.idleTimeout, server.go:3745, consumed at
//	                   server.go:2163 with `d > 0`)
//
// So a 0 ReadHeaderTimeout next to the 60s default ReadTimeout is not
// "disabled", it is "60s" — which is exactly how celeris#594 survived into the
// std engine after WithDefaults was made idempotent: the sentinel reached
// http.Server as 0 and net/http quietly re-armed it from ReadTimeout. The same
// holds for IdleTimeout.
//
// Mapping to -1 is safe on all four fields: every consumption site listed
// above guards with `d > 0`, so a negative value is never turned into a
// deadline — it only ever means "no timeout", and on ReadHeaderTimeout and
// IdleTimeout it additionally suppresses the ReadTimeout fallback. Using the
// same encoding for all four keeps the intent legible rather than relying on
// two fields happening to accept 0.
func stdTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return -1 // net/http: a negative duration is "no timeout" on all four fields
	}
	return d
}

// Listen starts the server and blocks until the context is canceled or an error occurs.
func (e *Engine) Listen(ctx context.Context) error {
	var ln net.Listener
	var err error
	if e.cfg.Listener != nil {
		ln = e.cfg.Listener
	} else {
		ln, err = net.Listen("tcp", e.cfg.Addr)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}
	// Wrap the listener so a transient Accept error doesn't tear the whole
	// server down (see resilientListener).
	ln = &resilientListener{Listener: ln, logger: e.logger}
	e.listener.Store(&ln)
	e.logger.Info("std engine listening", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		if err := e.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		return e.Shutdown(context.Background())
	case err := <-errCh:
		return err
	}
}

// Shutdown gracefully shuts down the server: in-flight requests drain
// until ctx expires, and whatever is still running once that budget is
// spent is woken through its request context.
//
// http.Server.Shutdown only waits for connections to go idle — it cancels
// nothing. A detached stream on std (an SSE handler with heartbeats off,
// parked on client.Context()) runs inline in ServeHTTP, so its connection
// never goes idle: the drain burns its whole budget and the handler
// goroutine, its request context and the connection then survive shutdown
// for the lifetime of the process (celeris#498). So the drain budget also
// bounds how long a request context stays live: cancelling the base
// context propagates to every r.Context() and the handlers unwind
// cooperatively, rather than having their connections yanked out from
// under them. Ordinary requests are untouched while the budget holds —
// they drain exactly as before.
//
// The escalation is armed off ctx rather than only after the drain
// returns because callers race for the once: Listen shuts down with a
// background context when its own context is cancelled, so it can win the
// drain with no budget at all while the caller that does have one
// (Server.Shutdown with Config.ShutdownTimeout) waits behind it.
func (e *Engine) Shutdown(ctx context.Context) error {
	stop := context.AfterFunc(ctx, e.baseCancel)
	var err error
	e.once.Do(func() {
		err = e.server.Shutdown(ctx)
	})
	if err != nil {
		// Cancel here too: when ctx expires, Shutdown's own select and
		// the AfterFunc callback race, and we must not report the drain
		// as over before the escalation is guaranteed. CancelFunc is
		// idempotent.
		e.baseCancel()
		return err
	}
	// Drained cleanly with budget left, so nothing needs waking: disarm,
	// or the caller's usual `defer cancel()` would cancel the contexts of
	// handlers a graceful shutdown deliberately leaves alone (a hijacked
	// WebSocket session, which http.Server stops tracking on hijack).
	stop()
	return nil
}

// Metrics returns a snapshot of engine metrics.
func (e *Engine) Metrics() engine.EngineMetrics {
	return engine.EngineMetrics{
		RequestCount:      e.metrics.reqCount.Load(),
		ActiveConnections: e.metrics.activeConns.Load(),
		ErrorCount:        e.metrics.errCount.Load(),
		CloseCount:        e.metrics.closeCount.Load(),
	}
}

// Type returns the engine type.
func (e *Engine) Type() engine.EngineType {
	return engine.Std
}

// Addr returns the bound listener address. Returns nil if not yet listening.
func (e *Engine) Addr() net.Addr {
	if lnp := e.listener.Load(); lnp != nil {
		return (*lnp).Addr()
	}
	return nil
}

// resilientListener wraps a net.Listener so the std engine survives transient
// Accept errors. net/http's Server.Serve returns — tearing down the entire
// server — on any Accept error whose (deprecated, unreliable) net.Error.
// Temporary() reports false. Under heavy connection churn on Linux some
// genuinely-transient accept errnos are classified non-temporary, which
// surfaced as an I-LIVENESS exit(1) in probatorium validation: Serve returned,
// Engine.Listen propagated the error to the caller, and the refapp log.Fatalf'd.
// Here every Accept error except a closed listener is logged and retried with
// bounded backoff (matching net/http's own temporary-error strategy), so a
// recoverable condition never kills the server. net.ErrClosed — what graceful
// Shutdown produces when it closes the listener — propagates unchanged so Serve
// returns ErrServerClosed and stops cleanly instead of looping forever.
type resilientListener struct {
	net.Listener
	logger *slog.Logger
}

func (l *resilientListener) Accept() (net.Conn, error) {
	const (
		baseDelay = 5 * time.Millisecond
		maxDelay  = time.Second
	)
	var delay time.Duration
	for {
		conn, err := l.Listener.Accept()
		if err == nil {
			return conn, nil
		}
		// Graceful shutdown closes the listener — let net/http observe it so
		// Serve returns ErrServerClosed rather than spinning.
		if errors.Is(err, net.ErrClosed) {
			return nil, err
		}
		if delay == 0 {
			delay = baseDelay
		} else {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
		if l.logger != nil {
			l.logger.Warn("std engine: transient accept error, retrying",
				"error", err, "retry_in", delay.String())
		}
		time.Sleep(delay)
	}
}

var _ engine.Engine = (*Engine)(nil)

func (e *Engine) connStateHook(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		e.metrics.activeConns.Add(1)
		if e.cfg.OnConnect != nil {
			e.safeCallback(e.cfg.OnConnect, conn.RemoteAddr().String())
		}
	case http.StateClosed, http.StateHijacked:
		e.metrics.activeConns.Add(-1)
		e.metrics.closeCount.Add(1)
		if e.cfg.OnDisconnect != nil {
			e.safeCallback(e.cfg.OnDisconnect, conn.RemoteAddr().String())
		}
	}
}

// safeCallback invokes a user-provided callback with panic recovery to prevent
// a panicking callback from crashing the connection state handler.
func (e *Engine) safeCallback(fn func(string), arg string) {
	defer func() {
		if r := recover(); r != nil {
			e.logger.Error("callback panic", "error", r)
		}
	}()
	fn(arg)
}
