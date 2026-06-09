//go:build linux

package epoll

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/scaler"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// Engine implements the epoll-based I/O engine.
type Engine struct {
	loops        []*Loop
	cfg          resource.Config
	handler      stream.Handler
	addr         atomic.Pointer[net.Addr]
	mu           sync.Mutex
	acceptPaused atomic.Bool
	metrics      struct {
		reqCount    atomic.Uint64
		activeConns atomic.Int64
		errCount    atomic.Uint64
		// asyncPromoted counts inline → dispatch-goroutine promotions
		// across all loops (celeris #300).
		asyncPromoted atomic.Uint64
	}
	// asyncRoutes is the static AsyncRoutes count snapshotted at
	// construction from the handler's AsyncRouteCount (#300 G3).
	asyncRoutes int
}

// New creates a new epoll engine.
func New(cfg resource.Config, handler stream.Handler) (*Engine, error) {
	cfg = cfg.WithDefaults()
	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("config validation: %w", errs[0])
	}

	e := &Engine{
		cfg:     cfg,
		handler: handler,
	}
	// Snapshot the static AsyncRoutes count (#300 G3).
	if r, ok := handler.(interface{ AsyncRouteCount() int }); ok {
		e.asyncRoutes = r.AsyncRouteCount()
	}
	return e, nil
}

// Listen starts the epoll engine and blocks until context is canceled.
func (e *Engine) Listen(ctx context.Context) error {
	// If a listener was provided (StartWithListener), use its bound address
	// and close the Go-managed listener so our raw epoll sockets can bind
	// with SO_REUSEPORT. Log the ownership transfer so users see it.
	if e.cfg.Listener != nil {
		e.cfg.Addr = e.cfg.Listener.Addr().String()
		if e.cfg.Logger != nil {
			e.cfg.Logger.Info("epoll: closing supplied listener to rebind via SO_REUSEPORT",
				"addr", e.cfg.Addr)
		}
		_ = e.cfg.Listener.Close()
		e.cfg.Listener = nil
	}

	resolved := e.cfg.Resources.Resolve()

	topo := platform.DetectNUMA()
	cpus := platform.DistributeWorkers(resolved.Workers, runtime.NumCPU(), topo.NumNodes)

	if topo.NumNodes > 1 {
		resolved.MaxEvents = resolved.MaxEvents / topo.NumNodes
		if resolved.MaxEvents < 64 {
			resolved.MaxEvents = 64
		}
	}

	e.mu.Lock()
	e.loops = make([]*Loop, resolved.Workers)
	for i := range resolved.Workers {
		l := newLoop(i, cpus[i], e.handler,
			resolved, e.cfg,
			&e.metrics.reqCount, &e.metrics.activeConns, &e.metrics.errCount,
			&e.metrics.asyncPromoted, &e.acceptPaused)
		e.loops[i] = l
	}
	e.mu.Unlock()

	innerCtx, innerCancel := context.WithCancel(ctx)
	defer innerCancel()

	var wg sync.WaitGroup
	for _, l := range e.loops {
		wg.Go(func() {
			l.run(innerCtx)
		})
	}

	for _, l := range e.loops {
		if initErr := <-l.ready; initErr != nil {
			innerCancel()
			wg.Wait()
			return initErr
		}
	}

	if len(e.loops) > 0 {
		addr := boundAddr(e.loops[0].listenFD)
		e.addr.Store(&addr)
	}

	e.cfg.Logger.Info("epoll engine listening", "addr", e.cfg.Addr, "loops", resolved.Workers)
	if e.cfg.AsyncHandlers && e.cfg.EnableH2Upgrade {
		e.cfg.Logger.Info(
			"AsyncHandlers + EnableH2Upgrade: async dispatch applies to HTTP/1.1 only; H2 conns still run inline on the worker",
		)
	}

	// Dynamic loop scaler. Typed cfg.WorkerScaling takes precedence over
	// env vars. Suppressed when wrapped by adaptive — adaptive runs ONE
	// higher-level scaler that delegates to the active sub-engine. The
	// algorithm itself lives in engine/scaler.
	if !e.cfg.SkipBuiltinScaler {
		if scalerCfg := scaler.Resolve(e.cfg, len(e.loops)); scalerCfg.Enabled {
			go e.runScaler(innerCtx, scalerCfg, &e.metrics.activeConns)
		}
	}

	<-ctx.Done()
	wg.Wait()
	return nil
}

// Shutdown is a no-op for the epoll engine — graceful shutdown is
// driven by context cancellation on Listen's parent context. The
// Server calls Listen with its managed context and cancels it during
// Server.Shutdown; the Listen goroutine returns after running
// Loop.shutdown (which closes connections and joins async dispatch
// goroutines via asyncWG).
//
// The context parameter is accepted for interface parity with engines
// that do run async drain operations on Shutdown (e.g. std's
// http.Server.Shutdown), and for future use if epoll Shutdown gains
// explicit drain semantics.
func (e *Engine) Shutdown(_ context.Context) error {
	return nil
}

// Sendfile implements engine.SendfileCapable using sendfile(2) (celeris#317).
// Writes `headers` via write(2) then transfers the file body to the socket
// via the kernel's zero-copy path. Returns the number of body bytes sent.
//
// The connection FD must be non-blocking (the epoll engine sets SOCK_NONBLOCK
// on accept). EAGAIN is surfaced to the caller, which defers the rest of the
// response to the next epoll_wait iteration. The source file is owned by
// the caller; the engine does not close it.
func (e *Engine) Sendfile(fdOut int, file *os.File, offset, length int64, headers []byte) (int64, error) {
	return sendfileH1(fdOut, file, offset, length, headers)
}

// Metrics returns a snapshot of engine metrics.
func (e *Engine) Metrics() engine.EngineMetrics {
	return engine.EngineMetrics{
		RequestCount:       e.metrics.reqCount.Load(),
		ActiveConnections:  e.metrics.activeConns.Load(),
		ErrorCount:         e.metrics.errCount.Load(),
		AsyncRoutes:        e.asyncRoutes,
		AsyncPromotedConns: e.metrics.asyncPromoted.Load(),
	}
}

// Type returns the engine type.
func (e *Engine) Type() engine.EngineType {
	return engine.Epoll
}

// PauseAccept stops accepting new connections. Synchronous — blocks
// until every loop has closed its listen FD (and drained pending
// accepts in the kernel queue with FIN, not RST). The adaptive engine
// relies on this: until the standby's listen sockets are gone from the
// SO_REUSEPORT routing pool, fresh dials may land on the about-to-pause
// engine and get RST'd when its FD closes. Synchronous Pause means
// callers can expose Addr() knowing only the active engine listens.
func (e *Engine) PauseAccept() error {
	e.acceptPaused.Store(true)
	e.mu.Lock()
	loops := append([]*Loop(nil), e.loops...)
	e.mu.Unlock()
	if len(loops) == 0 {
		return nil
	}
	// Short bound on the wait. The worker normally observes the flag
	// and drains its accept queue in well under 1 ms; capping at 100 ms
	// means even a worker stuck briefly (mid-iteration on a long CQE
	// burst) doesn't make Pause callers hold critical locks long
	// enough to cascade into other timeouts. If we time out, the FD
	// will still close on the next worker iteration — we just don't
	// guarantee it has happened by the time we return.
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		allClosed := true
		for _, l := range loops {
			if !l.listenFDClosed.Load() {
				allClosed = false
				break
			}
		}
		if allClosed {
			return nil
		}
		if time.Now().After(deadline) {
			return nil // best-effort: do not surface the timeout, the FD will close shortly
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// ResumeAccept starts accepting new connections again.
// Wakes any suspended loops so they re-create listen sockets.
func (e *Engine) ResumeAccept() error {
	e.acceptPaused.Store(false)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.loops {
		// Re-arm the close-confirmation flag so a subsequent Pause cycle
		// blocks on the new listen FD rather than the previously-closed
		// one.
		l.listenFDClosed.Store(false)
		l.wakeMu.Lock()
		if l.suspended.Load() {
			close(l.wake)
			l.wake = make(chan struct{})
			l.suspended.Store(false)
		}
		l.wakeMu.Unlock()
	}
	return nil
}

var (
	_ engine.Engine           = (*Engine)(nil)
	_ engine.AcceptController = (*Engine)(nil)
	_ engine.WorkerScaler     = (*Engine)(nil)
)

// Addr returns the bound listener address.
func (e *Engine) Addr() net.Addr {
	if p := e.addr.Load(); p != nil {
		return *p
	}
	return nil
}
