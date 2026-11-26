package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
	"github.com/goceleris/celeris/internal/engine"
)

// PoolConfig controls the worker-affinity pool returned by Open.
type PoolConfig struct {
	DSN                string
	MaxOpen            int
	MaxIdlePerWorker   int
	MaxLifetime        time.Duration
	MaxIdleTime        time.Duration
	HealthCheck        time.Duration
	StatementCacheSize int
	Application        string
}

const (
	defaultMaxIdlePerWorker  = 2
	defaultMaxLifetime       = 30 * time.Minute
	defaultMaxIdleTime       = 5 * time.Minute
	defaultHealthCheckPeriod = 30 * time.Second
)

// options is the accumulator for functional options.
type options struct {
	cfg      PoolConfig
	provider engine.EventLoopProvider
	ownsLoop bool
	// hasEngine records whether WithEngine was called with a non-nil
	// provider — independent of whether provider resolution succeeded
	// at Open time (the engine is often not started yet when drivers
	// are wired up). Used to pick the no-yield busy-poll sync path:
	// the handler that calls us will run on the engine's LockOSThread'd
	// worker, so runtime.Gosched would futex-storm.
	hasEngine bool
	// asyncEngine is true when the engine dispatches HTTP handlers async
	// (Config.AsyncHandlers on a supported engine). The handler runs on
	// an unlocked spawned G, so runtime.Gosched is cheap; the yielding
	// sync path is preferred to avoid the busy-spin P-hogging that cuts
	// other handler Gs on the same P off from running.
	asyncEngine bool
	// engine is the server WithEngine was given, kept for its
	// Config.SocketBusyPoll.
	engine ServerProvider
	// maxResultBytes is the result buffer cap WithMaxResultBytes set, and
	// maxResultSet whether it was set (Open refuses a value below 1).
	maxResultBytes int
	maxResultSet   bool
}

// Option configures Open.
type Option func(*options)

// WithEngine routes pool connections through the event loop of a running
// celeris.Server. When unset, Open resolves a standalone loop.
//
// The connections a Pool registers on an event loop take the server's
// Config.SocketBusyPoll as their SO_BUSY_POLL budget (Linux; 0, off, by
// default). In practice that is a Pool opened with a server that does not run
// async handlers and either has not started yet or is on the std engine, which
// has no event loop of its own (the Pool then uses the driver's own loop): a
// Pool without an engine, with a started epoll, io_uring or adaptive server's,
// or with one that runs async handlers (also before it starts), dials through
// Go's netpoll and sets no socket option. The Pool has no busy poll option of
// its own; a [Connector] does ([Connector.WithSocketBusyPoll]).
//
// The driver takes the server's event loop once, when it is opened: open it after
// srv.Addr() is non-nil to share the server's loops; opened earlier it uses the
// driver's own loop for its whole life.
func WithEngine(sp ServerProvider) Option {
	return func(o *options) {
		if sp == nil {
			return
		}
		o.hasEngine = true
		o.engine = sp
		if eventloop.IsAsyncServer(sp) {
			o.asyncEngine = true
		}
		if p := sp.EventLoopProvider(); p != nil {
			o.provider = p
			o.ownsLoop = false
		}
	}
}

// WithMaxOpen sets the total connection cap. Default: NumWorkers * 4.
func WithMaxOpen(n int) Option { return func(o *options) { o.cfg.MaxOpen = n } }

// WithMaxIdlePerWorker bounds each worker's idle list. Default: 2.
func WithMaxIdlePerWorker(n int) Option { return func(o *options) { o.cfg.MaxIdlePerWorker = n } }

// WithMaxLifetime sets the max age of a pooled conn. Default: 30m.
func WithMaxLifetime(d time.Duration) Option { return func(o *options) { o.cfg.MaxLifetime = d } }

// WithMaxIdleTime sets the max idle duration. Default: 5m.
func WithMaxIdleTime(d time.Duration) Option { return func(o *options) { o.cfg.MaxIdleTime = d } }

// WithStatementCacheSize sets the per-conn prepared-statement LRU capacity
// (default 256; the statement_cache_size DSN parameter does the same). A
// statement with arguments is prepared on first use and cached by its text. A
// connection that runs more distinct texts than n evicts the least recently
// used one and closes it on the server: a protocol-level Close is sent ahead of
// the query that evicted it, with no extra round trip and no wait. Pass values
// as arguments, not in the SQL text, or size the cache to the number of
// distinct texts: each miss costs a Parse and Describe round trip.
func WithStatementCacheSize(n int) Option { return func(o *options) { o.cfg.StatementCacheSize = n } }

// WithApplication sets the "application_name" startup parameter.
func WithApplication(name string) Option { return func(o *options) { o.cfg.Application = name } }

// WithMaxResultBytes sets the result buffer cap of the Pool's direct-mode
// connections: the most DataRow payload bytes a single statement's result may
// have, as a number of bytes. A direct-mode connection reads a reply whole into
// memory, and a statement whose result is over the cap fails with
// [ErrResultTooBig] (from [Rows.Err], after the rows that fit). The default is
// 64 MiB. The cap bounds memory, so raising it is the caller's explicit choice: n
// must be at least 1, and [Open] returns an error for a smaller value; there is no
// way to turn the cap off. A Pool on the event loop streams its results and has no
// cap, so the option does nothing there. [Connector.WithMaxResultBytes] is the
// same setting for database/sql.
func WithMaxResultBytes(n int) Option {
	return func(o *options) { o.maxResultBytes, o.maxResultSet = n, true }
}

// WithHealthCheck sets the background sweep interval. Zero disables it.
func WithHealthCheck(d time.Duration) Option { return func(o *options) { o.cfg.HealthCheck = d } }

// Pool is an alternative to database/sql.DB that keeps connections pinned to
// worker-affinity slots. It is usable from any goroutine but routes idle
// connections back to the worker that created them, so a handler running on
// worker N preferentially acquires conns whose event-loop callbacks land on
// worker N.
//
// # Session state
//
// Every call (QueryContext, ExecContext, QueryRow, Ping) takes a connection
// from the pool and puts it back. The pool does not reset the session on
// release: session state a call sets persists on the connection and is seen by
// the next caller that gets it. That covers SET and SET ROLE, set_config with
// is_local false, temporary tables, PREPARE, LISTEN and session-level advisory
// locks. The same holds for a *sql.DB opened with this driver: before it reuses
// a connection, ResetSession sends DISCARD ALL only if BeginTx marked the
// session dirty and the driver's own Commit or Rollback did not clear the mark
// (a transaction ended by COMMIT sent as plain SQL, for example), and never for
// a SET, a temporary table or the other state above (tracked in
// goceleris/celeris#1409).
//
// What is reset is a transaction. A connection released while it is in a
// transaction, open or failed (the server's last ReadyForQuery status is T or
// E), is closed and not put back, so the next caller never runs inside it, and
// the server rolls the transaction back. On the Pool, the call that left it open
// returns [ErrTxLeftOpen] (Exec "BEGIN" does, before anything else runs; Query
// returns it from Rows.Close, and QueryRow from Scan when the scan succeeded or found no row (then errors.Is is true for both sql.ErrNoRows and ErrTxLeftOpen); a
// statement that fails returns its own error, unchanged), so a transaction cannot
// be spread over Exec calls by accident: the connection of the BEGIN is gone
// before the next statement. Pool.BeginTx is the way, and its Tx is the Pool's
// only pinned connection (there is no Pool.Conn): BEGIN, COMMIT, savepoints and
// every statement between them run on that one connection. A *sql.DB opened with
// this driver cannot tell a pooled db.Exec("BEGIN") from the same statement on a
// dedicated *sql.Conn, where it is legitimate, so it returns no error: the
// connection is dropped when it is returned, the transaction is rolled back, and
// the statements that followed ran on another connection, each in its own
// transaction. Use db.BeginTx, or db.Conn for statements that must share one
// connection.
//
// To scope a setting to one piece of work, set it inside a Tx with SET LOCAL or
// select set_config(name, value, true): both end with the transaction. A
// *sql.DB offers a dedicated *sql.Conn for work that needs session state; undo
// the state (RESET ALL, DISCARD ALL) before it is closed, or the next user of
// the connection gets it.
type Pool struct {
	inner *async.Pool[*pgConn]

	providerMu  sync.RWMutex
	provider    engine.EventLoopProvider
	ownsLoop    bool
	hasEngine   bool // WithEngine was supplied; handler runs on engine worker
	asyncEngine bool // engine dispatches handlers async; handler is unlocked G

	closed atomic.Bool

	dsn DSN
	cfg PoolConfig
}

// ErrPoolClosed is returned from Pool methods after Close has been called.
var ErrPoolClosed = errors.New("celeris-postgres: pool is closed")

// Open opens a worker-affinity pool. The DSN is parsed once here; connect
// errors surface on the first Acquire.
func Open(dsnStr string, opts ...Option) (*Pool, error) {
	dsn, err := ParseDSN(dsnStr)
	if err != nil {
		return nil, err
	}
	if err := dsn.CheckSSL(); err != nil {
		return nil, err
	}
	o := &options{cfg: PoolConfig{DSN: dsnStr}}
	for _, fn := range opts {
		fn(o)
	}
	if o.maxResultSet {
		if o.maxResultBytes < 1 {
			return nil, fmt.Errorf("celeris-postgres: WithMaxResultBytes(%d): the cap must be at least 1 byte (there is no way to turn it off)", o.maxResultBytes)
		}
		dsn.Options.maxResultBytes = o.maxResultBytes
	}
	if o.cfg.Application != "" {
		if dsn.Params == nil {
			dsn.Params = map[string]string{}
		}
		dsn.Params["application_name"] = o.cfg.Application
	}
	if o.cfg.StatementCacheSize != 0 {
		dsn.Options.StatementCacheSize = o.cfg.StatementCacheSize
	}
	// Pool.Open is the production entry point for application code; default
	// to auto-prepared+cached statements unless the DSN explicitly opted
	// out (auto_cache_statements=false). Mirrors pgx's default behaviour.
	// Lower-level entry points (dialConn, the database/sql DSN-only path
	// via OpenConnector) keep the field unset to preserve raw DSN parse
	// semantics — this opt-in covers the realistic per-conn-pool path.
	if !dsn.Options.autoCacheStatementsExplicit {
		dsn.Options.AutoCacheStatements = true
	}
	// Only a connection registered on an engine's loop is tuned (dialConn);
	// the pool dials through Go's netpoll otherwise, and sets no option.
	dsn.Options.busyPoll = eventloop.BusyPoll(o.engine, 0)
	if o.provider == nil {
		prov, err := eventloop.Resolve(nil)
		if err != nil {
			return nil, err
		}
		o.provider = prov
		o.ownsLoop = true
	}
	nw := o.provider.NumWorkers()
	if nw <= 0 {
		nw = 1
	}
	if o.cfg.MaxOpen == 0 {
		o.cfg.MaxOpen = nw * 4
	}
	if o.cfg.MaxIdlePerWorker == 0 {
		// Size the per-worker idle list so the pool can keep every
		// open conn idle between round-trips. A tight cap like 2
		// causes constant reconnection under parallel load (each
		// release past the cap closes the conn, and the next acquire
		// pays the full dial + SCRAM-SHA-256 handshake + auto-prepare
		// cycle). Default: ceil(MaxOpen / NumWorkers) so the idle
		// list can absorb every released conn without churn. Users
		// with memory-sensitive deployments can still lower via
		// WithMaxIdlePerWorker.
		per := (o.cfg.MaxOpen + nw - 1) / nw
		if per < defaultMaxIdlePerWorker {
			per = defaultMaxIdlePerWorker
		}
		o.cfg.MaxIdlePerWorker = per
	}
	if o.cfg.MaxLifetime == 0 {
		o.cfg.MaxLifetime = defaultMaxLifetime
	}
	if o.cfg.MaxIdleTime == 0 {
		o.cfg.MaxIdleTime = defaultMaxIdleTime
	}
	if o.cfg.HealthCheck == 0 {
		o.cfg.HealthCheck = defaultHealthCheckPeriod
	}

	asyncCfg := async.PoolConfig{
		MaxOpen:          o.cfg.MaxOpen,
		MaxIdlePerWorker: o.cfg.MaxIdlePerWorker,
		MaxLifetime:      o.cfg.MaxLifetime,
		MaxIdleTime:      o.cfg.MaxIdleTime,
		HealthCheck:      o.cfg.HealthCheck,
		NumWorkers:       nw,
	}
	p := &Pool{
		provider:    o.provider,
		ownsLoop:    o.ownsLoop,
		hasEngine:   o.hasEngine,
		asyncEngine: o.asyncEngine,
		dsn:         dsn,
		cfg:         o.cfg,
	}
	p.inner = async.NewPool[*pgConn](asyncCfg, p.dial)
	return p, nil
}

func (p *Pool) dial(ctx context.Context, workerID int) (*pgConn, error) {
	// Direct mode when the caller runs on an unlocked G: standalone
	// (no engine) or WithEngine on an engine that dispatches handlers
	// async. In both cases net.Conn.Read parks the G through Go's
	// netpoll — no LockOSThread involvement, so we skip the mini-loop
	// entirely and keep the dispatch model symmetric with memcached.
	directMode := !p.hasEngine || p.asyncEngine

	// Additional case: WithEngine on an engine whose WorkerLoop does NOT
	// implement syncRoundTripper (io_uring, epoll today). Without a sync
	// path the driver's async write + channel-wait deadlocks on inline
	// handlers: the engine worker is the goroutine executing the handler
	// (LockOSThread'd), so when wait() parks on doneCh the M sits idle
	// and never drains the CQE carrying PG's reply. Netpoll is M-safe,
	// so direct mode sidesteps the deadlock with no correctness impact.
	if !directMode {
		p.providerMu.RLock()
		prov := p.provider
		p.providerMu.RUnlock()
		if prov == nil {
			return nil, ErrPoolClosed
		}
		if wl := prov.WorkerLoop(0); wl != nil {
			if _, ok := wl.(syncRoundTripper); !ok {
				directMode = true
			}
		}
	}

	if directMode {
		c, err := dialDirectConn(ctx, p.dsn)
		if err != nil {
			return nil, err
		}
		c.workerID = workerID
		c.maxLifetime = p.cfg.MaxLifetime
		c.maxIdleTime = p.cfg.MaxIdleTime
		return c, nil
	}

	p.providerMu.RLock()
	prov := p.provider
	p.providerMu.RUnlock()
	if prov == nil {
		return nil, ErrPoolClosed
	}
	c, err := dialConn(ctx, prov, nil, p.dsn, workerID)
	if err != nil {
		return nil, err
	}
	c.maxLifetime = p.cfg.MaxLifetime
	c.maxIdleTime = p.cfg.MaxIdleTime
	// busy-poll only on engines that run handlers inline on a locked
	// worker M. When the engine dispatches handlers async (caller is
	// an unlocked G), runtime.Gosched is cheap — the yielding sync
	// path is preferred since the busy-spin P-hog starves other
	// handler Gs on the same P.
	c.useBusySync(p.hasEngine && !p.asyncEngine)
	return c, nil
}

// Close closes the pool and releases the event loop if it was owned. Safe to
// call concurrently with Acquire — Acquire returns ErrPoolClosed once Close
// has set the closed flag.
func (p *Pool) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := p.inner.Close()
	p.providerMu.Lock()
	prov := p.provider
	owned := p.ownsLoop
	p.provider = nil
	p.providerMu.Unlock()
	if owned && prov != nil {
		eventloop.Release(prov)
	}
	return err
}

// Stats returns a snapshot of pool occupancy.
func (p *Pool) Stats() PoolStats { return p.inner.Stats() }

// IdleConnWorkers returns the Worker() IDs of every currently-idle connection
// across all worker slots. Same worker ID may appear multiple times when the
// slot holds more than one idle conn. In-use conns do not appear.
//
// Intended for tests and introspection asserting that per-CPU affinity is
// actually honored by the dial path — callers should not use it for load
// balancing decisions.
func (p *Pool) IdleConnWorkers() []int { return p.inner.IdleConnWorkers() }

// workerCtxKey is the unexported key for the worker hint in Context.
type workerCtxKey struct{}

// WithWorker returns ctx with a worker-affinity hint. Pool.Acquire uses the
// hint as the preferred worker index.
//
// The hint is a preference, not a pin. The Pool has one worker slot per event
// loop of the engine it was opened with (len of Stats().PerWorker); a hint
// outside [0, that count) is spread round-robin, and a call whose hinted
// worker has no idle connection takes an idle connection of another worker (or
// dials one), so consecutive calls with the same hint can run on different
// connections. The default number of loops is GOMAXPROCS minus a reserve (see
// Config.Workers of the celeris package: 7 on 8 CPUs, so a hint of 7 is out of
// range there). The Pool's own count, len(Stats().PerWorker), is the number to
// use (the engine's observe.EngineMetrics.Workers is the loops started, summed
// over both sub-engines for the adaptive engine). Code that used GOMAXPROCS as
// the worker count should read it instead.
func WithWorker(ctx context.Context, workerID int) context.Context {
	return context.WithValue(ctx, workerCtxKey{}, workerID)
}

// workerFromCtx returns -1 if no hint is set.
func workerFromCtx(ctx context.Context) int {
	v := ctx.Value(workerCtxKey{})
	if v == nil {
		return -1
	}
	if n, ok := v.(int); ok {
		return n
	}
	return -1
}

// acquire pops (or dials) a conn under ctx's worker hint. Callers must
// Release or Discard the conn.
//
// If the popped idle conn is already closed (e.g. peer closed, health check
// evicted) we discard it and retry, bounded to 5 attempts to avoid unbounded
// recursion / stack overflow in a pathological pool-churn scenario. The
// retry also honors ctx cancellation.
func (p *Pool) acquire(ctx context.Context) (*pgConn, error) {
	if p.closed.Load() {
		return nil, ErrPoolClosed
	}
	hint := workerFromCtx(ctx)
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p.closed.Load() {
			return nil, ErrPoolClosed
		}
		c, err := p.inner.Acquire(ctx, hint)
		if err != nil {
			return nil, err
		}
		if !c.closed.Load() {
			return c, nil
		}
		p.inner.Discard(c)
	}
	return nil, errors.New("celeris-postgres: pool could not acquire a healthy conn after retries")
}

// release returns c to its home worker's idle list.
func (p *Pool) release(c *pgConn) { _ = p.releaseChecked(c) }

// releaseChecked is release that reports a conn it had to discard because the
// statement left a transaction open: the server's last ReadyForQuery said T (in
// a transaction) or E (in a failed one). The conn is closed, not put back (the
// server rolls the transaction back, and the next caller never runs inside it),
// and the error is [ErrTxLeftOpen], for the caller of a pool-level call to
// return. A closed conn is discarded without an error.
func (p *Pool) releaseChecked(c *pgConn) error {
	if c.closed.Load() {
		p.inner.Discard(c)
		return nil
	}
	if c.inTransaction() {
		p.inner.Discard(c)
		return ErrTxLeftOpen
	}
	p.inner.Release(c)
	return nil
}

// QueryContext runs query on an acquired conn, returns a Rows wrapper that
// returns the conn to the pool when closed.
func (p *Pool) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	named, slot := anysToNamed(args)
	drows, err := c.QueryContext(ctx, query, named)
	releaseNamedArgs(slot)
	if err != nil {
		p.release(c)
		return nil, err
	}
	return acquireRows(drows, c, p), nil
}

// ExecContext runs a statement and returns a [Result]. A statement that
// succeeds and leaves a transaction open on the connection (Exec "BEGIN")
// returns [ErrTxLeftOpen]: see the Session state section of [Pool]. A statement
// that fails returns its own error, unchanged, as before (a connection left in
// a failed transaction by it is discarded all the same).
func (p *Pool) ExecContext(ctx context.Context, query string, args ...any) (res Result, err error) {
	c, aerr := p.acquire(ctx)
	if aerr != nil {
		return Result{}, aerr
	}
	defer func() {
		// ErrTxLeftOpen only for a statement that succeeded: one that failed
		// returns its own error, as it always did (a duplicate key in
		// "BEGIN; INSERT ...; COMMIT" is a *PGError 23505, not a leftover).
		if rerr := p.releaseChecked(c); rerr != nil && err == nil {
			res = Result{}
			err = rerr
		}
	}()
	named, slot := anysToNamed(args)
	r, err := c.ExecContext(ctx, query, named)
	releaseNamedArgs(slot)
	if err != nil {
		return Result{}, err
	}
	n, _ := r.RowsAffected()
	return Result{rowsAffected: n}, nil
}

// Ping acquires a conn, pings, and returns it.
func (p *Pool) Ping(ctx context.Context) error {
	c, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	defer p.release(c)
	return c.Ping(ctx)
}

// BeginTx acquires a conn, issues BEGIN, and returns a Tx that pins the
// conn until Commit/Rollback.
func (p *Pool) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	var dopts driver.TxOptions
	if opts != nil {
		dopts = driver.TxOptions{ReadOnly: opts.ReadOnly, Isolation: driver.IsolationLevel(opts.Isolation)}
	}
	tx, err := c.BeginTx(ctx, dopts)
	if err != nil {
		p.release(c)
		return nil, err
	}
	return &Tx{inner: tx.(*pgTx), conn: c, pool: p}, nil
}

// namedArgsSlot holds a reusable []driver.NamedValue slab pooled via
// namedArgsPool. The slab header lives on the heap exactly once (when
// the pool's New runs); subsequent Get/Put round-trips reuse the same
// *namedArgsSlot pointer so there is no per-call slice-header alloc.
type namedArgsSlot struct {
	buf []driver.NamedValue
}

var namedArgsPool = sync.Pool{
	New: func() any {
		return &namedArgsSlot{buf: make([]driver.NamedValue, 0, 8)}
	},
}

// anysToNamed converts a variadic ...any arg list into driver.NamedValues.
// On success it returns the slot for releaseNamedArgs to return to the
// pool; for len(args)==0 it returns (nil, nil) so callers can skip the
// release call entirely.
func anysToNamed(args []any) ([]driver.NamedValue, *namedArgsSlot) {
	if len(args) == 0 {
		return nil, nil
	}
	slot := namedArgsPool.Get().(*namedArgsSlot)
	if cap(slot.buf) < len(args) {
		slot.buf = make([]driver.NamedValue, len(args))
	} else {
		slot.buf = slot.buf[:len(args)]
	}
	for i, a := range args {
		slot.buf[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return slot.buf, slot
}

// releaseNamedArgs returns the slot to the pool after zeroing the
// backing slots so captured Values are not pinned. A nil slot is a no-op
// (matches the zero-arg fast path).
func releaseNamedArgs(slot *namedArgsSlot) {
	if slot == nil {
		return
	}
	for i := range slot.buf {
		slot.buf[i] = driver.NamedValue{}
	}
	slot.buf = slot.buf[:0]
	namedArgsPool.Put(slot)
}

// acquireRows wraps inner in a fresh *Rows. The wrapper is the pointer the
// caller keeps, so it is never recycled: a caller can hold it after Close (a
// deferred Close plus an explicit one, a Row that was scanned already) and a
// recycled wrapper would be another query's Rows by then. What Close releases
// is what the wrapper points at: the pooled pgRows and request slabs, and the
// conn. The wrapper costs one small allocation per query.
func acquireRows(inner driver.Rows, conn *pgConn, pool *Pool) *Rows {
	return &Rows{inner: inner, conn: conn, pool: pool}
}

// errRowsClosed is what Scan returns after Close.
var errRowsClosed = errors.New("celeris-postgres: Rows are closed")

// Rows is the pool-level rows wrapper. It returns the underlying conn to the
// pool when Close is called. The iteration API matches database/sql.Rows:
//
//	for rows.Next() {
//	    if err := rows.Scan(&id, &name); err != nil { ... }
//	}
//	if err := rows.Err(); err != nil { ... }
type Rows struct {
	inner driver.Rows
	conn  *pgConn
	pool  *Pool
	// done is set by the first Close (which also drops inner, conn and pool and
	// the raw views below), so a later Close, Next, Scan or Columns is inert.
	done atomic.Bool
	// closemu is held for read by Next, Scan, Columns and Err and for write by Close
	// while it drops the fields below, as database/sql.Rows.closemu: a Close on
	// another goroutine (a watchdog) never recycles the pooled pgRows or releases the
	// conn under a call that is inside them.
	closemu sync.RWMutex

	// current holds the driver.Values decoded by the last successful
	// Next call. Populated only when the inner is NOT *pgRows (i.e.
	// in tests or alternate paths); production path uses rawRow.
	current []driver.Value

	// rawRow / rawCodecs / rawColumns / rawTextFormat short-circuit the
	// slow driver.Value path when inner is *pgRows. rawRow holds a view
	// into the underlying pgRequest slab and is valid until the next
	// Next / Close.
	rawRow        [][]byte
	rawCodecs     []*protocol.TypeCodec
	rawColumns    []protocol.ColumnDesc
	rawTextFormat bool
	rawAvailable  bool

	// iterErr records the first non-EOF error encountered during Next. It
	// survives Close, so for rows.Next() {}; rows.Close(); rows.Err() reports it.
	iterErr error
}

// Columns returns column names, or nil once the Rows is closed.
func (r *Rows) Columns() []string {
	r.closemu.RLock()
	defer r.closemu.RUnlock()
	if r.done.Load() {
		return nil
	}
	return r.inner.Columns()
}

// Close releases resources and returns the conn to the pool. A statement that
// left a transaction open on the conn (Query "BEGIN") makes Close return
// [ErrTxLeftOpen] after discarding the conn. Close is idempotent: every call
// after the first returns nil and does nothing (so ErrTxLeftOpen is reported
// once), which makes `defer rows.Close()` next to an explicit Close fine. After
// Close, Next returns false, Scan returns an error and Err keeps the error
// iteration ended with. Close may be called from another goroutine while Next or
// Scan is running (as with database/sql.Rows): it waits for that call, which sees
// the Rows closed on its next use. Next, Scan and Columns themselves are not safe
// to call concurrently with each other. A sql.Scanner must not call any method of
// the same Rows from inside Scan: Close would wait for itself, and Err or Columns
// would take the read lock a second time behind a Close that is waiting for it
// (the same hazard as database/sql).
func (r *Rows) Close() error {
	if !r.done.CompareAndSwap(false, true) {
		return nil
	}
	r.closemu.Lock()
	inner, conn, pool := r.inner, r.conn, r.pool
	// Drop everything that points into released memory: the rawRow view is a slice
	// of the request slab that inner.Close hands back to its pool, and inner is
	// itself pooled.
	r.inner, r.conn, r.pool = nil, nil, nil
	r.current = nil
	r.rawRow, r.rawCodecs, r.rawColumns, r.rawAvailable = nil, nil, nil, false
	r.closemu.Unlock()
	var err error
	if inner != nil {
		err = inner.Close()
	}
	if pool != nil && conn != nil {
		if rerr := pool.releaseChecked(conn); rerr != nil && err == nil {
			err = rerr
		}
	}
	return err
}

// Next advances the cursor to the next row. It returns false when no more
// rows are available or an error occurred. After Next returns false, call
// [Rows.Err] to distinguish normal end-of-data from an error.
//
// Fast path: when inner is *pgRows (the production path), Next captures
// the raw wire bytes + per-column codecs without boxing into
// driver.Value. Scan() then decodes directly into user pointers, saving
// one heap alloc per non-zero-size cell (int64 / time.Time etc. would
// otherwise escape via interface conversion).
func (r *Rows) Next() bool {
	r.closemu.RLock()
	defer r.closemu.RUnlock()
	if r.done.Load() {
		return false
	}
	if pgr, ok := r.inner.(*pgRows); ok {
		row, codecs, cols, textFormat, err := pgr.nextRaw()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.iterErr = err
			}
			r.rawAvailable = false
			return false
		}
		r.rawRow = row
		r.rawCodecs = codecs
		r.rawColumns = cols
		r.rawTextFormat = textFormat
		r.rawAvailable = true
		return true
	}
	// Legacy path for non-pgRows inners.
	if r.current == nil {
		r.current = make([]driver.Value, len(r.inner.Columns()))
	}
	for i := range r.current {
		r.current[i] = nil
	}
	if err := r.inner.Next(r.current); err != nil {
		if !errors.Is(err, io.EOF) {
			r.iterErr = err
		}
		return false
	}
	return true
}

// Scan copies the columns from the current row into dest. Each dest
// must be a pointer. On the fast path (inner is *pgRows), Scan decodes
// directly from the wire bytes into dest without ever materialising a
// driver.Value — zero interface{} boxing per cell.
//
// A NULL cell never leaves its dest as it was, so a variable reused across
// rows cannot carry the previous row's value. NULL sets dest to its zero
// value: "" for *string, nil for *[]byte, *json.RawMessage, *any and a
// pointer-to-pointer (**T, which a non-NULL value allocates), 0 for the
// numeric kinds, false for *bool and the zero time.Time for *time.Time. A
// sql.Scanner (sql.NullString, sql.NullInt64, ...) receives Scan(nil) and sets
// Valid=false; scan into one, into *any or into **T when NULL must be told
// apart from a zero value. A dest Scan does not support fails for NULL with
// the same "unsupported scan dest type" error as for any other value.
func (r *Rows) Scan(dest ...any) error {
	r.closemu.RLock()
	defer r.closemu.RUnlock()
	if r.done.Load() {
		return errRowsClosed
	}
	if r.rawAvailable {
		if len(dest) != len(r.rawRow) {
			return fmt.Errorf("celeris-postgres: Scan expected %d dest, got %d", len(r.rawRow), len(dest))
		}
		for i, raw := range r.rawRow {
			var col protocol.ColumnDesc
			var codec *protocol.TypeCodec
			if i < len(r.rawColumns) {
				col = r.rawColumns[i]
			}
			if i < len(r.rawCodecs) {
				codec = r.rawCodecs[i]
			}
			if err := scanRaw(dest[i], raw, codec, col, r.rawTextFormat); err != nil {
				return fmt.Errorf("celeris-postgres: column %d: %w", i, err)
			}
		}
		return nil
	}
	if r.current == nil {
		return errors.New("celeris-postgres: Scan called without a preceding Next")
	}
	if len(dest) != len(r.current) {
		return fmt.Errorf("celeris-postgres: Scan expected %d dest, got %d", len(r.current), len(dest))
	}
	sr, _ := r.inner.(*streamRows)
	for i, v := range r.current {
		if sr != nil && i < len(sr.current) && sr.current[i] != nil && i < len(sr.codecs) && i < len(sr.columns) {
			// A streamed row: infinity into a string is read from the raw cell.
			binaryFmt := !sr.textFormat && sr.columns[i].FormatCode != protocol.FormatText
			if sr.codecs[i] == nil && binaryFmt {
				if err := binaryOnlyGuard(dest[i], sr.codecs[i], sr.columns[i], binaryFmt); err != nil {
					return fmt.Errorf("celeris-postgres: column %d: %w", i, err)
				}
			}
			if sr.codecs[i] != nil && isTimeOID(sr.codecs[i].OID) && rawCellGuard(dest[i], sr.current[i], sr.codecs[i], binaryFmt) {
				continue
			}
		}
		if err := scanValue(dest[i], v); err != nil {
			return fmt.Errorf("celeris-postgres: column %d: %w", i, err)
		}
	}
	return nil
}

// Err returns the first non-EOF error encountered during iteration. It is still
// that error after Close. Always check it after the loop: on a direct-mode Pool
// it is the only place that reports [ErrResultTooBig] (Next just returns false
// and Close returns nil), and a server error that arrives after some rows is
// reported here too.
func (r *Rows) Err() error {
	r.closemu.RLock()
	defer r.closemu.RUnlock()
	return r.iterErr
}

// scanRaw decodes wire bytes directly into the caller's pointer without
// allocating an intermediate driver.Value. This is the hot path for
// Rows.Scan when the inner driver is *pgRows; the goal is to eliminate
// the interface{} boxing that forces int64 / time.Time / float64 cells
// to escape to heap. For types this path doesn't cover (sql.Scanner,
// []interface{}, rare PG types), it falls through to the driver.Value
// path via the codec and scanValue so behaviour stays identical.
func scanRaw(dest any, raw []byte, codec *protocol.TypeCodec, col protocol.ColumnDesc, textFormat bool) error {
	// sql.Scanner gets first dibs — users expect Scan(nil) to fire.
	if scanner, ok := dest.(sql.Scanner); ok {
		if raw == nil {
			return scanner.Scan(nil)
		}
		v, err := decodeToValue(raw, codec, col, textFormat)
		if err != nil {
			return err
		}
		return scanner.Scan(v)
	}
	if raw == nil {
		// Nil cell — zero the target if it's a common primitive.
		return assignNil(dest)
	}
	binaryFmt := !textFormat && col.FormatCode != protocol.FormatText
	if codec == nil && binaryFmt {
		if err := binaryOnlyGuard(dest, codec, col, binaryFmt); err != nil {
			return err
		}
	}
	// Binary format for known codecs — direct decode into typed pointer.
	if codec != nil && binaryFmt {
		if ok, err := decodeBinaryInto(dest, raw, codec); ok {
			return err
		}
	}
	if codec != nil && !binaryFmt {
		if ok, err := decodeTextInto(dest, raw, codec); ok {
			return err
		}
	}
	// Fallback: go through driver.Value.
	v, err := decodeToValue(raw, codec, col, textFormat)
	if err != nil {
		return err
	}
	// A date, timestamp or timestamptz into a string is RFC3339Nano
	// (as database/sql formats it). Done here, on the raw path, and not in
	// convertAssign: a streamed row scans through convertAssign exactly as in
	// v1.5.11, where a time.Time into *string was fmt's %v form.
	if t, ok := v.(time.Time); ok {
		if d, ok := dest.(*string); ok {
			*d = t.Format(time.RFC3339Nano)
			return nil
		}
	}
	return scanValue(dest, v)
}

// decodeToValue runs the codec's standard decoder and returns the boxed
// driver.Value. Used by scanRaw's fallback paths.
func decodeToValue(raw []byte, codec *protocol.TypeCodec, col protocol.ColumnDesc, textFormat bool) (driver.Value, error) {
	if codec == nil {
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp, nil
	}
	if textFormat || col.FormatCode == protocol.FormatText {
		if codec.DecodeText != nil {
			return decodeText(codec, raw)
		}
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp, nil
	}
	if codec.DecodeBinary != nil {
		return codec.DecodeBinary(raw)
	}
	if codec.DecodeText != nil {
		return decodeText(codec, raw)
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return cp, nil
}

// The int64→narrower helpers below are the scan-side counterpart of the
// encoder's "int4 overflow" / "int2 overflow" checks: a value that does not
// fit the destination fails the scan instead of wrapping silently
// (celeris#502). Each returns the zero value with the error so callers can
// leave the destination untouched.

func int64ToInt(n int64) (int, error) {
	if n < math.MinInt || n > math.MaxInt {
		return 0, fmt.Errorf("celeris-postgres: scan: value %d out of range for int", n)
	}
	return int(n), nil
}

func int64ToInt32(n int64) (int32, error) {
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("celeris-postgres: scan: value %d out of range for int32", n)
	}
	return int32(n), nil
}

func int64ToInt16(n int64) (int16, error) {
	if n < math.MinInt16 || n > math.MaxInt16 {
		return 0, fmt.Errorf("celeris-postgres: scan: value %d out of range for int16", n)
	}
	return int16(n), nil
}

func int64ToUint32(n int64) (uint32, error) {
	if n < 0 || n > math.MaxUint32 {
		return 0, fmt.Errorf("celeris-postgres: scan: value %d out of range for uint32", n)
	}
	return uint32(n), nil
}

// binaryFormIsValue reports whether a column of codec's type carries, in
// binary result format, exactly the bytes a string destination should receive. That holds for the text-like types (text, varchar, json) and
// for bytea; every other registered codec decodes through its DecodeBinary.
func binaryFormIsValue(codec *protocol.TypeCodec) bool {
	switch codec.OID {
	case protocol.OIDText, protocol.OIDVarchar, protocol.OIDJSON, protocol.OIDBytea:
		return true
	}
	return false
}

// decodeBinaryInto decodes raw binary-format bytes directly into dest
// for the common primitive types. Returns (true, err) when the type was
// handled (err may be non-nil for decode failures); (false, nil) when
// the caller should fall through to the driver.Value path.
func decodeBinaryInto(dest any, raw []byte, codec *protocol.TypeCodec) (bool, error) {
	switch d := dest.(type) {
	case *int:
		n, err := protocol.DecodeIntBinary(raw, codec)
		if err != nil {
			return true, err
		}
		v, err := int64ToInt(n)
		if err != nil {
			return true, err
		}
		*d = v
		return true, nil
	case *int64:
		n, err := protocol.DecodeIntBinary(raw, codec)
		if err != nil {
			return true, err
		}
		*d = n
		return true, nil
	case *int32:
		n, err := protocol.DecodeIntBinary(raw, codec)
		if err != nil {
			return true, err
		}
		v, err := int64ToInt32(n)
		if err != nil {
			return true, err
		}
		*d = v
		return true, nil
	case *int16:
		n, err := protocol.DecodeIntBinary(raw, codec)
		if err != nil {
			return true, err
		}
		v, err := int64ToInt16(n)
		if err != nil {
			return true, err
		}
		*d = v
		return true, nil
	case *uint32:
		n, err := protocol.DecodeIntBinary(raw, codec)
		if err != nil {
			return true, err
		}
		v, err := int64ToUint32(n)
		if err != nil {
			return true, err
		}
		*d = v
		return true, nil
	case *string:
		// Fast path for text/varchar/etc.: no interface boxing, single
		// string allocation out of the raw slab bytes. Only for codecs
		// whose binary form is the value itself; jsonb / jsonpath carry a
		// version byte, int4 / uuid / timestamp / ... are fixed-width binary.
		if !binaryFormIsValue(codec) {
			// A date, timestamp or timestamptz infinity is the server's own text,
			// read from the wire value (the PGInfinity sentinel is also a real
			// instant): no other cell pays for the check.
			if txt, ok := infinityText(raw, codec.OID, true); ok {
				*d = txt
				return true, nil
			}
			return false, nil
		}
		*d = string(raw)
		return true, nil
	case *[]byte:
		// The value's wire bytes, for every type, exactly as v1.5.11: scan into
		// a string for text.
		cp := make([]byte, len(raw))
		copy(cp, raw)
		*d = cp
		return true, nil
	case *bool:
		// PG binary bool is a single byte (0/1).
		if len(raw) != 1 {
			return false, nil
		}
		*d = raw[0] != 0
		return true, nil
	}
	return false, nil
}

// decodeTextInto is the text-format counterpart to decodeBinaryInto.
func decodeTextInto(dest any, raw []byte, codec *protocol.TypeCodec) (bool, error) {
	switch d := dest.(type) {
	case *int:
		n, err := protocol.ParseIntTextASCII(raw)
		if err != nil {
			return true, err
		}
		v, err := int64ToInt(n)
		if err != nil {
			return true, err
		}
		*d = v
		return true, nil
	case *int64:
		n, err := protocol.ParseIntTextASCII(raw)
		if err != nil {
			return true, err
		}
		*d = n
		return true, nil
	case *int32:
		n, err := protocol.ParseIntTextASCII(raw)
		if err != nil {
			return true, err
		}
		v, err := int64ToInt32(n)
		if err != nil {
			return true, err
		}
		*d = v
		return true, nil
	case *string:
		if codec.OID == protocol.OIDBytea {
			return false, nil // text form is the hex escape, not the value
		}
		*d = string(raw)
		return true, nil
	case *[]byte:
		if codec.OID == protocol.OIDBytea {
			return false, nil
		}
		cp := make([]byte, len(raw))
		copy(cp, raw)
		*d = cp
		return true, nil
	}
	return false, nil
}

// assignNil stores a NULL cell into dest. It never leaves dest as it was:
// a variable reused across rows would otherwise carry the previous row's
// value into a row whose column is NULL, with no error.
//
// The destination is set to its zero value: "" for *string, nil for
// *[]byte, *json.RawMessage and *any, 0 for the numeric kinds, false for
// *bool, the zero time.Time for *time.Time, and nil for a pointer-to-pointer
// (**T). sql.Scanner destinations (sql.Null*, a user type) are handled by the
// caller via Scan(nil). Any other destination fails with the error a non-NULL
// value gets, so a destination Scan does not support never succeeds silently.
func assignNil(dest any) error {
	switch d := dest.(type) {
	case *int:
		*d = 0
	case *int64:
		*d = 0
	case *int32:
		*d = 0
	case *int16:
		*d = 0
	case *uint32:
		*d = 0
	case *float64:
		*d = 0
	case *float32:
		*d = 0
	case *string:
		*d = ""
	case *[]byte:
		*d = nil
	case *json.RawMessage:
		*d = nil
	case *bool:
		*d = false
	case *time.Time:
		*d = time.Time{}
	case *any:
		*d = nil
	default:
		// **T: a nullable column scans into *T as nil.
		if rv := reflect.ValueOf(dest); rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Kind() == reflect.Pointer {
			rv.Elem().SetZero()
			return nil
		}
		return fmt.Errorf("unsupported scan dest type %T", dest)
	}
	return nil
}

// scanValue assigns driver.Value v to the pointer dest with basic type
// conversion. It covers the types returned by the Postgres codec layer.
func scanValue(dest any, v driver.Value) error {
	// sql.Scanner gets first priority so types like sql.NullString, uuid.UUID,
	// pgtype.Inet, etc. work out of the box. Scan(nil) lets nullable types
	// set their Valid flag to false.
	if scanner, ok := dest.(sql.Scanner); ok {
		return scanner.Scan(v)
	}
	if v == nil {
		return assignNil(dest)
	}
	return convertAssign(dest, v)
}

// convertAssign assigns src to dest pointer, performing common conversions.
func convertAssign(dest any, src any) error {
	switch d := dest.(type) {
	case *string:
		switch s := src.(type) {
		case string:
			*d = s
		case []byte:
			*d = string(s)
		case int64:
			*d = fmt.Sprintf("%d", s)
		case float64:
			*d = fmt.Sprintf("%g", s)
		case bool:
			if s {
				*d = "true"
			} else {
				*d = "false"
			}
		case []driver.Value:
			return errors.New("celeris-postgres: scan: cannot convert array to string; scan into *any or cast ::text")
		default:
			*d = fmt.Sprintf("%v", src)
		}
	case *int:
		switch s := src.(type) {
		case int64:
			v, err := int64ToInt(s)
			if err != nil {
				return err
			}
			*d = v
		case float64:
			*d = int(s)
		case string:
			return fmt.Errorf("celeris-postgres: scan: cannot convert string %q to int", s)
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to int", src)
		}
	case *int64:
		switch s := src.(type) {
		case int64:
			*d = s
		case float64:
			*d = int64(s)
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to int64", src)
		}
	case *int32:
		switch s := src.(type) {
		case int64:
			v, err := int64ToInt32(s)
			if err != nil {
				return err
			}
			*d = v
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to int32", src)
		}
	case *int16:
		switch s := src.(type) {
		case int64:
			v, err := int64ToInt16(s)
			if err != nil {
				return err
			}
			*d = v
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to int16", src)
		}
	case *float64:
		switch s := src.(type) {
		case float64:
			*d = s
		case int64:
			*d = float64(s)
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to float64", src)
		}
	case *float32:
		switch s := src.(type) {
		case float64:
			*d = float32(s)
		case int64:
			*d = float32(s)
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to float32", src)
		}
	case *bool:
		switch s := src.(type) {
		case bool:
			*d = s
		case int64:
			*d = s != 0
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to bool", src)
		}
	case *[]byte:
		switch s := src.(type) {
		case []byte:
			cp := make([]byte, len(s))
			copy(cp, s)
			*d = cp
		case string:
			*d = []byte(s)
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to []byte", src)
		}
	case *json.RawMessage:
		switch s := src.(type) {
		case []byte:
			*d = append(json.RawMessage(nil), s...)
		case string:
			*d = append(json.RawMessage(nil), s...)
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to json.RawMessage", src)
		}
	case *any:
		*d = src
	case *time.Time:
		switch s := src.(type) {
		case time.Time:
			*d = s
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to time.Time", src)
		}
	default:
		// **T: allocate a T and scan into it, so a nullable column scans
		// into *T (nil for NULL, see assignNil).
		if rv := reflect.ValueOf(dest); rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Kind() == reflect.Pointer {
			nv := reflect.New(rv.Elem().Type().Elem())
			if err := scanValue(nv.Interface(), src); err != nil {
				return err
			}
			rv.Elem().Set(nv)
			return nil
		}
		return fmt.Errorf("unsupported scan dest type %T", dest)
	}
	return nil
}

// Row is the result of calling [Pool.QueryRow]. It wraps a single-row
// query with automatic Close semantics, matching database/sql.Row.
type Row struct {
	rows *Rows
	err  error
}

// Scan copies the columns from the single result row into dest. If the
// query returned no rows, Scan returns sql.ErrNoRows. The underlying Rows
// is closed automatically, and a second Scan (the rows are gone) returns
// sql.ErrNoRows too.
func (r *Row) Scan(dest ...any) (err error) {
	if r.err != nil {
		return r.err
	}
	rows := r.rows
	if rows == nil {
		return sql.ErrNoRows
	}
	r.rows = nil
	defer func() {
		// Only the error of a transaction left open on the conn is the
		// caller's, and only when the scan went well or found no row; any other
		// error of Close or of the scan is left as it was.
		if cerr := rows.Close(); errors.Is(cerr, ErrTxLeftOpen) {
			switch {
			case err == nil:
				err = cerr
			case errors.Is(err, sql.ErrNoRows):
				// A statement that returns no row (QueryRow("BEGIN")): both
				// are true, so a caller testing for either sees it.
				err = errors.Join(err, cerr)
			}
		}
	}()
	if !rows.Next() {
		if rows.Err() != nil {
			return rows.Err()
		}
		return sql.ErrNoRows
	}
	return rows.Scan(dest...)
}

// Err returns the error, if any, encountered during the query. Unlike
// [Row.Scan], it does not close the underlying Rows.
func (r *Row) Err() error { return r.err }

// QueryRow executes a query expected to return at most one row. The
// returned [Row] is always non-nil; errors are deferred until [Row.Scan].
func (p *Pool) QueryRow(ctx context.Context, query string, args ...any) *Row {
	rows, err := p.QueryContext(ctx, query, args...)
	return &Row{rows: rows, err: err}
}

// Result is a pool-level Result that implements database/sql/driver.Result.
type Result struct {
	rowsAffected int64
}

// LastInsertId is not supported by PostgreSQL. Use RETURNING instead.
func (r Result) LastInsertId() (int64, error) {
	return 0, errors.New("celeris-postgres: LastInsertId not supported, use RETURNING")
}

// RowsAffected returns the number of affected rows parsed from the
// CommandComplete tag.
func (r Result) RowsAffected() (int64, error) { return r.rowsAffected, nil }

// Tx is the pool-level transaction wrapper. A Tx is obtained from
// Pool.BeginTx; the zero value is not usable.
type Tx struct {
	inner *pgTx
	conn  *pgConn
	pool  *Pool
	// done is set by the call that finishes the tx; atomic, so a deferred
	// Rollback reads it safely while another goroutine finishes the tx.
	done atomic.Bool
}

// txDoneError is returned by a Tx used after Commit or Rollback. It keeps the
// message the driver always had and matches sql.ErrTxDone with errors.Is, as
// code written against database/sql expects.
type txDoneError struct{}

func (txDoneError) Error() string { return "celeris-postgres: tx already finished" }
func (txDoneError) Unwrap() error { return sql.ErrTxDone }

var errTxDone error = txDoneError{}

// Commit commits the transaction and returns the conn to the pool.
// On failure, done is left false so a deferred Rollback can still fire, except
// for ErrTxCommitRollback: the server rolled the transaction back, so it is over
// (done stays set, the conn is released) and a later Rollback returns "tx already
// finished". After the tx is finished it returns an error that matches
// sql.ErrTxDone. A Rollback from another goroutine that runs while the Commit is
// still in flight also gets ErrTxDone, even if that Commit then fails; the
// goroutine that called Commit can still Rollback after the failure.
func (t *Tx) Commit() error {
	if !t.done.CompareAndSwap(false, true) {
		return errTxDone
	}
	err := t.inner.Commit()
	if errors.Is(err, ErrTxCommitRollback) {
		// The server rolled the transaction back: it is over (done is set by the
		// CAS above and stays), the conn is free.
		t.pool.release(t.conn)
		return err
	}
	if err != nil {
		t.done.Store(false)
		return err
	}
	t.pool.release(t.conn)
	return nil
}

// Rollback rolls back the transaction and returns the conn to the pool. After
// the tx is finished it returns an error that matches sql.ErrTxDone.
func (t *Tx) Rollback() error {
	if !t.done.CompareAndSwap(false, true) {
		return errTxDone
	}
	err := t.inner.Rollback()
	t.pool.release(t.conn)
	return err
}

// ExecContext runs a statement on the pinned conn.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (Result, error) {
	if t.done.Load() {
		return Result{}, errTxDone
	}
	named, slot := anysToNamed(args)
	r, err := t.conn.ExecContext(ctx, query, named)
	releaseNamedArgs(slot)
	if err != nil {
		return Result{}, err
	}
	n, _ := r.RowsAffected()
	return Result{rowsAffected: n}, nil
}

// QueryContext runs a query on the pinned conn. The returned Rows must be
// closed before Commit/Rollback.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	if t.done.Load() {
		return nil, errTxDone
	}
	named, slot := anysToNamed(args)
	drows, err := t.conn.QueryContext(ctx, query, named)
	releaseNamedArgs(slot)
	if err != nil {
		return nil, err
	}
	return acquireRows(drows, nil, nil), nil
}

// QueryRow executes a query expected to return at most one row on the
// pinned conn. The returned [Row] is always non-nil; errors are deferred
// until [Row.Scan].
func (t *Tx) QueryRow(ctx context.Context, query string, args ...any) *Row {
	rows, err := t.QueryContext(ctx, query, args...)
	return &Row{rows: rows, err: err}
}

// Savepoint issues SAVEPOINT <name> inside this transaction. name must
// match [A-Za-z0-9_]+; anything else is rejected before the wire write to
// avoid SQL-injection.
func (t *Tx) Savepoint(ctx context.Context, name string) error {
	if t.done.Load() {
		return errTxDone
	}
	return t.conn.Savepoint(ctx, name)
}

// ReleaseSavepoint issues RELEASE SAVEPOINT <name>. Same name rules as
// Savepoint.
func (t *Tx) ReleaseSavepoint(ctx context.Context, name string) error {
	if t.done.Load() {
		return errTxDone
	}
	return t.conn.ReleaseSavepoint(ctx, name)
}

// RollbackToSavepoint issues ROLLBACK TO SAVEPOINT <name>. Same name rules
// as Savepoint.
func (t *Tx) RollbackToSavepoint(ctx context.Context, name string) error {
	if t.done.Load() {
		return errTxDone
	}
	return t.conn.RollbackToSavepoint(ctx, name)
}

// isTimeOID reports whether oid is date, timestamp or timestamptz.
func isTimeOID(oid uint32) bool {
	return oid == protocol.OIDDate || oid == protocol.OIDTimestamp || oid == protocol.OIDTimestamptz
}

// rawCellGuard handles, for a *string destination (a *[]byte gets the wire bytes,
// as in v1.5.11), the cells whose
// raw wire value says more than the decoded driver.Value can. It reports
// whether it filled dest. Today that is the infinity of a
// date, timestamp or timestamptz: the server's own text, read from the wire
// value before any time.Time exists, because the PGInfinity sentinel is also a
// real instant. Both the buffered (scanRaw) and the streamed (Rows.Scan over a
// streamRows) paths call it; every other cell goes the unchanged way.
func rawCellGuard(dest any, raw []byte, codec *protocol.TypeCodec, binaryFmt bool) bool {
	d, ok := dest.(*string)
	if !ok || codec == nil {
		return false
	}
	switch codec.OID {
	case protocol.OIDDate, protocol.OIDTimestamp, protocol.OIDTimestamptz:
	default:
		return false
	}
	if txt, ok := infinityText(raw, codec.OID, binaryFmt); ok {
		*d = txt
		return true
	}
	return false
}

// infinityText reports whether raw, a date, timestamp or timestamptz cell of
// the given type and format, is PostgreSQL's infinity or -infinity, and returns
// the text the server prints for it. The binary form is the int32 days
// (date) or int64 microseconds (timestamp, timestamptz) sentinel MaxInt /
// MinInt; the text form is "infinity" / "-infinity" itself.
func infinityText(raw []byte, oid uint32, binaryFmt bool) (string, bool) {
	switch oid {
	case protocol.OIDDate, protocol.OIDTimestamp, protocol.OIDTimestamptz:
	default:
		return "", false
	}
	if !binaryFmt {
		switch string(raw) {
		case "infinity":
			return "infinity", true
		case "-infinity":
			return "-infinity", true
		}
		return "", false
	}
	switch {
	case oid == protocol.OIDDate && len(raw) == 4:
		switch int32(binary.BigEndian.Uint32(raw)) {
		case math.MaxInt32:
			return "infinity", true
		case math.MinInt32:
			return "-infinity", true
		}
	case oid != protocol.OIDDate && len(raw) == 8:
		switch int64(binary.BigEndian.Uint64(raw)) {
		case math.MaxInt64:
			return "infinity", true
		case math.MinInt64:
			return "-infinity", true
		}
	}
	return "", false
}
