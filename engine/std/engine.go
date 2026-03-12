// Package std provides an engine implementation backed by net/http.
package std

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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
		httpHandler = h2c.NewHandler(bridge, h2s)
	}

	e.server = &http.Server{
		Addr:           cfg.Addr,
		Handler:        httpHandler,
		ReadTimeout:    cfg.ReadTimeout,
		WriteTimeout:   cfg.WriteTimeout,
		IdleTimeout:    cfg.IdleTimeout,
		MaxHeaderBytes: cfg.MaxHeaderBytes,
		ConnState:      e.connStateHook,
	}

	return e, nil
}

// Listen starts the server and blocks until the context is canceled or an error occurs.
func (e *Engine) Listen(ctx context.Context) error {
	ln, err := net.Listen("tcp", e.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
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

var _ engine.Engine = (*Engine)(nil)

func (e *Engine) connStateHook(_ net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		e.metrics.activeConns.Add(1)
	case http.StateClosed, http.StateHijacked:
		e.metrics.activeConns.Add(-1)
	}
}
