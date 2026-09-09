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

	e.server = &http.Server{
		Addr:              cfg.Addr,
		Handler:           bridge,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ConnState:         e.connStateHook,
		BaseContext:       func(net.Listener) context.Context { return e.baseCtx },
	}

	if cfg.Protocol == engine.H2C || cfg.Protocol == engine.Auto {
		// Cleartext HTTP/2 through net/http's own HTTP/2 server, selected by
		// Protocols.SetUnencryptedHTTP2. This replaces x/net/http2/h2c, which
		// is deprecated (celeris#440). HTTP/1.1 stays enabled alongside it so
		// a plain H1 request on an H2C listener is still served, matching what
		// h2c.NewHandler did by falling through to the wrapped handler.
		//
		// Scope: this covers prior-knowledge h2c (client preface on a fresh
		// connection). It does NOT cover the RFC 7540 3.2 HTTP/1.1 Upgrade
		// handshake, which RFC 9113 removed from the specification and
		// net/http does not implement. The io_uring and epoll engines still
		// honour Config.EnableH2Upgrade; the std engine no longer does.
		p := new(http.Protocols)
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		e.server.Protocols = p
		e.server.HTTP2 = &http.HTTP2Config{
			MaxConcurrentStreams: int(cfg.MaxConcurrentStreams),
			MaxReadFrameSize:     int(cfg.MaxFrameSize),
		}
	}

	return e, nil
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
