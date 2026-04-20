package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
	"github.com/goceleris/celeris/engine"
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
}

// Option configures Open.
type Option func(*options)

// WithEngine routes pool connections through the event loop of a running
// celeris.Server. When unset, Open resolves a standalone loop.
func WithEngine(sp eventloop.ServerProvider) Option {
	return func(o *options) {
		if sp == nil {
			return
		}
		o.hasEngine = true
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

// WithStatementCacheSize sets the per-conn prepared-statement LRU capacity.
func WithStatementCacheSize(n int) Option { return func(o *options) { o.cfg.StatementCacheSize = n } }

// WithApplication sets the "application_name" startup parameter.
func WithApplication(name string) Option { return func(o *options) { o.cfg.Application = name } }

// WithHealthCheck sets the background sweep interval. Zero disables it.
func WithHealthCheck(d time.Duration) Option { return func(o *options) { o.cfg.HealthCheck = d } }

// Pool is an alternative to database/sql.DB that keeps connections pinned to
// worker-affinity slots. It is usable from any goroutine but routes idle
// connections back to the worker that created them, so a handler running on
// worker N preferentially acquires conns whose event-loop callbacks land on
// worker N.
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
	if o.cfg.Application != "" {
		if dsn.Params == nil {
			dsn.Params = map[string]string{}
		}
		dsn.Params["application_name"] = o.cfg.Application
	}
	if o.cfg.StatementCacheSize != 0 {
		dsn.Options.StatementCacheSize = o.cfg.StatementCacheSize
	}
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
func (p *Pool) Stats() async.PoolStats { return p.inner.Stats() }

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
func (p *Pool) release(c *pgConn) {
	if c.closed.Load() {
		p.inner.Discard(c)
		return
	}
	p.inner.Release(c)
}

// QueryContext runs query on an acquired conn, returns a Rows wrapper that
// returns the conn to the pool when closed.
func (p *Pool) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	named := anysToNamed(args)
	drows, err := c.QueryContext(ctx, query, named)
	if err != nil {
		p.release(c)
		return nil, err
	}
	return &Rows{inner: drows, conn: c, pool: p}, nil
}

// ExecContext runs a statement and returns a [Result].
func (p *Pool) ExecContext(ctx context.Context, query string, args ...any) (Result, error) {
	c, err := p.acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer p.release(c)
	named := anysToNamed(args)
	r, err := c.ExecContext(ctx, query, named)
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

// anysToNamed converts a variadic ...any arg list into driver.NamedValues.
func anysToNamed(args []any) []driver.NamedValue {
	if len(args) == 0 {
		return nil
	}
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

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
	done  bool

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

	// iterErr records the first non-EOF error encountered during Next.
	iterErr error
}

// Columns returns column names.
func (r *Rows) Columns() []string { return r.inner.Columns() }

// Close releases resources and returns the conn to the pool.
func (r *Rows) Close() error {
	if r.done {
		return nil
	}
	r.done = true
	err := r.inner.Close()
	if r.pool != nil && r.conn != nil {
		r.pool.release(r.conn)
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
	if r.done {
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
func (r *Rows) Scan(dest ...any) error {
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
	for i, v := range r.current {
		if err := scanValue(dest[i], v); err != nil {
			return fmt.Errorf("celeris-postgres: column %d: %w", i, err)
		}
	}
	return nil
}

// Err returns the first non-EOF error encountered during iteration.
func (r *Rows) Err() error { return r.iterErr }

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
	// Binary format for known codecs — direct decode into typed pointer.
	binaryFmt := !textFormat && col.FormatCode != protocol.FormatText
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
			return codec.DecodeText(raw)
		}
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp, nil
	}
	if codec.DecodeBinary != nil {
		return codec.DecodeBinary(raw)
	}
	if codec.DecodeText != nil {
		return codec.DecodeText(raw)
	}
	cp := make([]byte, len(raw))
	copy(cp, raw)
	return cp, nil
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
		*d = int(n)
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
		*d = int32(n)
		return true, nil
	case *int16:
		n, err := protocol.DecodeIntBinary(raw, codec)
		if err != nil {
			return true, err
		}
		*d = int16(n)
		return true, nil
	case *uint32:
		n, err := protocol.DecodeIntBinary(raw, codec)
		if err != nil {
			return true, err
		}
		*d = uint32(n)
		return true, nil
	case *string:
		// Fast path for text/varchar/etc.: no interface boxing, single
		// string allocation out of the raw slab bytes.
		*d = string(raw)
		return true, nil
	case *[]byte:
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
func decodeTextInto(dest any, raw []byte, _ *protocol.TypeCodec) (bool, error) {
	switch d := dest.(type) {
	case *int:
		n, err := protocol.ParseIntTextASCII(raw)
		if err != nil {
			return true, err
		}
		*d = int(n)
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
		*d = int32(n)
		return true, nil
	case *string:
		*d = string(raw)
		return true, nil
	case *[]byte:
		cp := make([]byte, len(raw))
		copy(cp, raw)
		*d = cp
		return true, nil
	}
	return false, nil
}

// assignNil zeros the pointed-to primitive when the column is NULL.
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
	case *string:
		*d = ""
	case *[]byte:
		*d = nil
	case *bool:
		*d = false
	default:
		// Unknown target — let scanValue handle it via convertAssign(nil).
		return scanValue(dest, nil)
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
		return nil
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
		default:
			*d = fmt.Sprintf("%v", src)
		}
	case *int:
		switch s := src.(type) {
		case int64:
			*d = int(s)
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
			*d = int32(s)
		default:
			return fmt.Errorf("celeris-postgres: scan: cannot convert %T to int32", src)
		}
	case *int16:
		switch s := src.(type) {
		case int64:
			*d = int16(s)
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
// is closed automatically.
func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer func() { _ = r.rows.Close() }()
	if !r.rows.Next() {
		if r.rows.Err() != nil {
			return r.rows.Err()
		}
		return sql.ErrNoRows
	}
	return r.rows.Scan(dest...)
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

// Tx is the pool-level transaction wrapper.
type Tx struct {
	inner *pgTx
	conn  *pgConn
	pool  *Pool
	done  bool
}

// Commit commits the transaction and returns the conn to the pool.
// On failure, done is left false so a deferred Rollback can still fire.
func (t *Tx) Commit() error {
	if t.done {
		return errors.New("celeris-postgres: tx already finished")
	}
	err := t.inner.Commit()
	if err != nil {
		return err
	}
	t.done = true
	t.pool.release(t.conn)
	return nil
}

// Rollback rolls back the transaction and returns the conn to the pool.
func (t *Tx) Rollback() error {
	if t.done {
		return errors.New("celeris-postgres: tx already finished")
	}
	t.done = true
	err := t.inner.Rollback()
	t.pool.release(t.conn)
	return err
}

// ExecContext runs a statement on the pinned conn.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (Result, error) {
	if t.done {
		return Result{}, errors.New("celeris-postgres: tx already finished")
	}
	named := anysToNamed(args)
	r, err := t.conn.ExecContext(ctx, query, named)
	if err != nil {
		return Result{}, err
	}
	n, _ := r.RowsAffected()
	return Result{rowsAffected: n}, nil
}

// QueryContext runs a query on the pinned conn. The returned Rows must be
// closed before Commit/Rollback.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	if t.done {
		return nil, errors.New("celeris-postgres: tx already finished")
	}
	named := anysToNamed(args)
	drows, err := t.conn.QueryContext(ctx, query, named)
	if err != nil {
		return nil, err
	}
	return &Rows{inner: drows, conn: nil, pool: nil}, nil
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
	if t.done {
		return errors.New("celeris-postgres: tx already finished")
	}
	return t.conn.Savepoint(ctx, name)
}

// ReleaseSavepoint issues RELEASE SAVEPOINT <name>. Same name rules as
// Savepoint.
func (t *Tx) ReleaseSavepoint(ctx context.Context, name string) error {
	if t.done {
		return errors.New("celeris-postgres: tx already finished")
	}
	return t.conn.ReleaseSavepoint(ctx, name)
}

// RollbackToSavepoint issues ROLLBACK TO SAVEPOINT <name>. Same name rules
// as Savepoint.
func (t *Tx) RollbackToSavepoint(ctx context.Context, name string) error {
	if t.done {
		return errors.New("celeris-postgres: tx already finished")
	}
	return t.conn.RollbackToSavepoint(ctx, name)
}
