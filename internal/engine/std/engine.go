// Package std provides an engine implementation backed by net/http.
package std

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"

	// golang.org/x/net/http2's Server, ServeConn and ConfigureServer are
	// deprecated, and their replacement
	// (http.Server.Protocols + SetUnencryptedHTTP2) does NOT cover the
	// RFC 7540 3.2 Upgrade handshake -- net/http implements the upgrade path
	// internally but exposes no way for a caller to reach it. celeris#440
	// migrated to the stdlib and silently dropped h2c Upgrade from this
	// engine; the epoll and io_uring engines still honour it, and the
	// nightly caught the asymmetry. Until the stdlib exposes an equivalent,
	// http2.Server.ServeConn (behind h2cHandler) is the only way to keep the
	// three engines agreeing. Every use below carries the SA1019 waiver.
	"golang.org/x/net/http2"
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
	// drainCtx is the context the one drain (http.Server.Shutdown) runs
	// under, whichever call starts it; drainCancel ends it. Every
	// Shutdown(ctx) arms drainCancel on its ctx, so the drain keeps the
	// budget of every caller, not only of the one that won the once
	// (celeris#753). The cancel carries a cause, the ctx error of the call
	// whose budget ended the drain, so a call whose own ctx is still live
	// can report whose it was (celeris#879); a cause the ctx carries of its
	// own is kept too (budgetCause). drainErr is the drain's
	// result, read by every caller after once.Do has returned.
	drainCtx    context.Context
	drainCancel context.CancelCauseFunc
	drainErr    error
	// draining is set when the drain begins, before http.Server.Shutdown
	// is called; see Bridge.ServeHTTP.
	draining atomic.Bool
	// drained is set once the drain has ended, whichever way. A drain that
	// saw the h2Streams count reach zero has closed it too (see
	// closeH2Streams); one whose budget ran out first has not, and this is
	// all that tells Bridge.ServeHTTP. Only the closed count is airtight
	// (one CAS); this flag is a plain load, so a stream that passes it just
	// before the drain stores it still runs, with an already cancelled
	// request context. That is within the contract: a drain whose budget
	// ran out leaves its handlers running. After a clean drain no h2c
	// request is handed to a handler.
	drained atomic.Bool
	// h2Streams counts the HTTP/2 (h2c) requests in their handler
	// (Bridge.ServeHTTP). net/http hands an h2c connection over (hijack)
	// and stops tracking it, so http.Server.Shutdown does not wait for
	// its streams; the drain waits for this count instead (celeris#759).
	// Once the drain has seen it at zero it is closed: moved to a large
	// negative value (h2StreamsClosed) in the same atomic step, so a stream
	// that arrives after the drain has decided to end is refused rather
	// than served after Shutdown has returned (celeris#878).
	h2Streams atomic.Int64
	metrics   struct {
		reqCount    atomic.Uint64
		activeConns atomic.Int64
		// errs is the per-cause ErrorCount breakdown (celeris#645).
		// EngineMetrics.ErrorCount is its sum; no separate total exists.
		// std reaches only the two request-path buckets: it never accepts
		// a descriptor of its own (net/http does) and never transplants.
		errs errclass.Counters
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
	e.drainCtx, e.drainCancel = context.WithCancelCause(context.Background())

	bridge := &Bridge{engine: e, handler: handler}

	// h2c through h2cHandler rather than http.Protocols, because it is the
	// only one of the two that performs the RFC 7540 3.2 Upgrade handshake.
	// See the import comment: dropping it took the std engine out of step
	// with epoll and io_uring, which both honour Config.EnableH2Upgrade, and
	// made the validation harness's h2c-churn slice vacuous on std -- 3,597
	// upgrade preambles, not one 101.
	//
	// h2cHandler serves BOTH shapes: a client that opens with the HTTP/2
	// preface (prior knowledge) and one that asks to upgrade from HTTP/1.1.
	// Plain HTTP/1.1 requests fall through to the wrapped handler, so an
	// H2C listener still serves H1.
	var httpHandler http.Handler = bridge
	var h2s *http2.Server //nolint:staticcheck // SA1019: see the import comment.
	if cfg.Protocol == engine.H2C || cfg.Protocol == engine.Auto {
		//
		// x/net 0.59 deprecated http2.Server along with the handler; it is
		// still the only type the h2c front end can serve with, so the two
		// carry the same waiver. Keep the literal on one line so the waiver
		// covers it.
		h2s = &http2.Server{MaxConcurrentStreams: cfg.MaxConcurrentStreams, MaxReadFrameSize: cfg.MaxFrameSize} //nolint:staticcheck // SA1019: the type h2cHandler serves with; see the import comment.
		httpHandler = &h2cHandler{next: bridge, h2s: h2s}
		// Auto with the upgrade disabled (Config.EnableH2Upgrade = &false,
		// celeris#964): keep prior-knowledge h2c, which is not the
		// handshake, and serve a request that asks for the RFC 7540 3.2
		// upgrade as the plain HTTP/1.1 request it also is, as epoll,
		// io_uring and adaptive do. cfg.EnableH2Upgrade is true for every
		// Auto listener that did not turn it off (WithDefaults), so only an
		// explicit false lands here. H2C is left as it was: the resolved
		// flag is false there for nil and for &false alike, and std
		// upgrades on H2C (TestStdEngineAnswersH2CUpgradeWith101).
		if cfg.Protocol == engine.Auto && !cfg.EnableH2Upgrade {
			httpHandler = &h2cNoUpgradeHandler{h2c: httpHandler, plain: bridge}
		}
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

	if h2s != nil {
		// Register h2s with e.server, so the connections h2cHandler serves
		// through h2s.ServeConn belong to it: http.Server.Shutdown then sends
		// each of them GOAWAY at the start of the drain (celeris#878).
		// net/http hands an h2c connection over (hijack) and stops tracking
		// it, so without this nothing reached it: it kept accepting streams
		// during the drain's wait and after Shutdown returned. Must come
		// after e.server's timeouts are set, which the h2 idle timeout is
		// read from, and before it serves.
		if err := http2.ConfigureServer(e.server, h2s); err != nil { //nolint:staticcheck // SA1019: see the import comment.
			return nil, fmt.Errorf("h2c server registration: %w", err)
		}
	}

	return e, nil
}

// h2cNoUpgradeHandler is the std handler for Protocol Auto with the h2c
// upgrade disabled: a request that asks for the RFC 7540 3.2 upgrade goes
// straight to the plain handler, with its headers untouched, and everything
// else (prior-knowledge h2c, HTTP/1.1) goes through the h2c front end.
type h2cNoUpgradeHandler struct {
	h2c   http.Handler
	plain http.Handler
}

func (h *h2cNoUpgradeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isH2CUpgrade(r.Header) {
		h.plain.ServeHTTP(w, r)
		return
	}
	h.h2c.ServeHTTP(w, r)
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
		// Drain, with no budget of Listen's own: a Shutdown call that
		// carries one ends this drain when its ctx expires, even though
		// this call started it (celeris#753). A drain cut short that way
		// is that Shutdown's error to report, not Listen's.
		if err := e.drain(); err != nil && e.drainCtx.Err() == nil {
			return err
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// drain runs http.Server.Shutdown once, under drainCtx, and returns its
// result to every caller; a caller that did not start it waits for it in
// once.Do.
//
// http.Server.Shutdown sends every h2c connection GOAWAY as it starts
// (celeris#878): h2cHandler serves them through the http2.Server registered
// with the server, which net/http stops tracking at the hijack but whose
// OnShutdown hook still reaches. Once the drain has ended, whichever way, an
// h2c request that still arrives is refused (see enterH2Stream).
func (e *Engine) drain() error {
	e.once.Do(func() {
		e.draining.Store(true)
		err := e.server.Shutdown(e.drainCtx)
		if err == nil {
			err = e.waitH2Streams(e.drainCtx)
		}
		e.drainErr = err
		e.drained.Store(true)
	})
	return e.drainErr
}

// budgetCause is what a Shutdown call's expired ctx tells the drain: its
// error, which the callers' errors.Is checks rely on, and, if the ctx
// carries a cause of its own (WithCancelCause, WithTimeoutCause), that too.
func budgetCause(ctx context.Context) error {
	err, cause := ctx.Err(), context.Cause(ctx)
	if cause == nil || cause == err { //nolint:errorlint // identity: no cause of its own
		return err
	}
	return fmt.Errorf("%w: %w", err, cause)
}

// h2StreamsPoll is how often the drain looks at h2Streams while it waits.
const h2StreamsPoll = 5 * time.Millisecond

// h2StreamsClosed is what the drain moves h2Streams to when it has seen it at
// zero: far enough below zero that no number of arrivals brings it back above.
const h2StreamsClosed = math.MinInt64 / 2

// closeH2Streams closes the h2Streams count if it is zero, in one atomic step,
// and reports whether it did. Closing it and an arriving stream's increment
// cannot both succeed: the stream was counted, and the drain waits for it, or
// it saw the closed count, and is refused (enterH2Stream).
func (e *Engine) closeH2Streams() bool {
	return e.h2Streams.CompareAndSwap(0, h2StreamsClosed)
}

// enterH2Stream counts an h2c request into its handler and reports whether it
// may run. It may not once the drain has ended (celeris#878): the drain has
// returned, or will with the count closed, and the OnShutdown hooks run after
// it. The caller that is refused has nothing to undo; the one that is
// admitted ends with h2Streams.Add(-1).
func (e *Engine) enterH2Stream() bool {
	if e.h2Streams.Add(1) <= 0 || e.drained.Load() {
		e.h2Streams.Add(-1)
		return false
	}
	return true
}

// waitH2Streams waits, bounded by ctx, until no HTTP/2 (h2c) request is in
// its handler (celeris#759), and closes the count (see closeH2Streams).
// http.Server.Shutdown does not wait for them: net/http serves h2c on a
// connection it has handed over and no longer tracks, so the OnShutdown hooks
// ran, and a direct Shutdown returned, while an h2c handler was still
// running. The connection itself is left as http.Server.Shutdown leaves a
// hijacked one: its handlers are what the drain waits for, and Shutdown has
// sent it GOAWAY, so it takes no new stream while it waits (celeris#878).
func (e *Engine) waitH2Streams(ctx context.Context) error {
	if e.closeH2Streams() {
		return nil
	}
	t := time.NewTicker(h2StreamsPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		if e.closeH2Streams() {
			return nil
		}
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
// Callers race for the drain: Listen drains when its own context is
// cancelled, and under Server.StartWithContext that cancel reaches Listen
// before the Server.Shutdown that carries Config.ShutdownTimeout reaches
// this method. So the budget is armed off ctx, not handed to the drain
// call: whichever call started the drain, it ends when the ctx of any
// Shutdown call expires, and the escalation fires with it. Before
// celeris#753 the budget went only to the call that won the once, and
// Listen's won with none, so the hooks and the Start call waited for the
// last handler however long it ran.
//
// There is one drain, so overlapping calls share it, and the shortest
// budget governs: a second call whose ctx is already done (a second signal
// that means "stop now") ends the drain of the first. What each call gets
// back says whose budget it was (celeris#879). A call whose own ctx expired
// returns that ctx's error. A call whose ctx is still live, when another
// call's budget ended the drain with handlers possibly still running,
// returns an error that wraps that call's ctx error (errors.Is reports
// context.DeadlineExceeded or context.Canceled for it, and any cause the ctx
// carries of its own, context.WithCancelCause or WithTimeoutCause), never nil, as the
// OnShutdown hooks are about to run, and never a bare context.Canceled of an
// internal context. A call that arrives after the drain has ended returns
// the same.
func (e *Engine) Shutdown(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		e.drainCancel(budgetCause(ctx))
		e.baseCancel()
	})
	if err := e.drain(); err != nil {
		// Cancel here too: when ctx expires, the drain's return and the
		// AfterFunc callback race, and we must not report the drain as
		// over before the escalation is guaranteed. CancelFunc is
		// idempotent.
		e.baseCancel()
		if cerr := ctx.Err(); cerr != nil && e.drainCtx.Err() != nil {
			// This call's budget ran out: report it as the caller's own
			// deadline (or cancel), not as the internal drainCtx's.
			return cerr
		}
		if e.drainCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			// This call's ctx is live: another call's budget ended the
			// drain. Say so, and whose it was.
			return fmt.Errorf("std: the drain was ended by a Shutdown call whose budget ran out: %w", context.Cause(e.drainCtx))
		}
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
	m := engine.EngineMetrics{
		RequestCount:      e.metrics.reqCount.Load(),
		ActiveConnections: e.metrics.activeConns.Load(),
		CloseCount:        e.metrics.closeCount.Load(),
	}
	// ErrorCount and its buckets, together, from one snapshot (celeris#645).
	engine.FillErrorClasses(&m, e.metrics.errs.Snapshot())
	return m
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
