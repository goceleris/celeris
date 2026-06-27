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
	"golang.org/x/net/http2/h2c" //nolint:staticcheck // SA1019: h2c is deprecated in favour of http.Server.Protocols, which is only available on Go 1.27+. We pin Go 1.26.3 (see go.mod), so we keep h2c.NewHandler until the toolchain upgrade.
)

// Engine wraps net/http.Server to implement the engine.Engine interface.
type Engine struct {
	server   *http.Server
	listener atomic.Pointer[net.Listener]
	handler  stream.Handler
	cfg      resource.Config
	logger   *slog.Logger
	metrics  struct {
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

	bridge := &Bridge{engine: e, handler: handler}

	var httpHandler http.Handler = bridge
	if cfg.Protocol == engine.H2C || cfg.Protocol == engine.Auto {
		h2s := &http2.Server{
			MaxConcurrentStreams: cfg.MaxConcurrentStreams,
			MaxReadFrameSize:     cfg.MaxFrameSize,
		}
		httpHandler = h2c.NewHandler(bridge, h2s) //nolint:staticcheck // SA1019: h2c.NewHandler is deprecated in favour of http.Server.Protocols (Go 1.27+); we still target Go 1.26.3.
	}

	e.server = &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpHandler,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ConnState:         e.connStateHook,
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

// Shutdown gracefully shuts down the server.
func (e *Engine) Shutdown(ctx context.Context) error {
	var err error
	e.once.Do(func() {
		err = e.server.Shutdown(ctx)
	})
	return err
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
