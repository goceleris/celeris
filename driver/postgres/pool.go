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

	providerMu sync.RWMutex
	provider   engine.EventLoopProvider
	ownsLoop   bool

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
		o.cfg.MaxIdlePerWorker = defaultMaxIdlePerWorker
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
		provider: o.provider,
		ownsLoop: o.ownsLoop,
		dsn:      dsn,
		cfg:      o.cfg,
	}
	p.inner = async.NewPool[*pgConn](asyncCfg, p.dial)
	return p, nil
}

func (p *Pool) dial(ctx context.Context, workerID int) (*pgConn, error) {
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

	// current holds the driver.Values decoded by the last successful Next call.
	current []driver.Value
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
func (r *Rows) Next() bool {
	if r.done {
		return false
	}
	// Reuse the current slice across iterations to avoid per-call allocation.
	// On the first call, current is nil; we size it from the column count.
	if r.current == nil {
		r.current = make([]driver.Value, len(r.inner.Columns()))
	}
	// Clear the values from the previous iteration (prevents stale refs
	// from pinning old data between rows).
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

// Scan copies the columns from the current row into dest. The number of
// dest arguments must match the number of columns. Each dest must be a
// pointer; Scan assigns the driver.Value to the pointed-to variable with
// basic type conversion.
func (r *Rows) Scan(dest ...any) error {
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
			return fmt.Errorf("cannot convert string %q to int", s)
		default:
			return fmt.Errorf("cannot convert %T to int", src)
		}
	case *int64:
		switch s := src.(type) {
		case int64:
			*d = s
		case float64:
			*d = int64(s)
		default:
			return fmt.Errorf("cannot convert %T to int64", src)
		}
	case *int32:
		switch s := src.(type) {
		case int64:
			*d = int32(s)
		default:
			return fmt.Errorf("cannot convert %T to int32", src)
		}
	case *int16:
		switch s := src.(type) {
		case int64:
			*d = int16(s)
		default:
			return fmt.Errorf("cannot convert %T to int16", src)
		}
	case *float64:
		switch s := src.(type) {
		case float64:
			*d = s
		case int64:
			*d = float64(s)
		default:
			return fmt.Errorf("cannot convert %T to float64", src)
		}
	case *float32:
		switch s := src.(type) {
		case float64:
			*d = float32(s)
		case int64:
			*d = float32(s)
		default:
			return fmt.Errorf("cannot convert %T to float32", src)
		}
	case *bool:
		switch s := src.(type) {
		case bool:
			*d = s
		case int64:
			*d = s != 0
		default:
			return fmt.Errorf("cannot convert %T to bool", src)
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
			return fmt.Errorf("cannot convert %T to []byte", src)
		}
	case *any:
		*d = src
	case *time.Time:
		switch s := src.(type) {
		case time.Time:
			*d = s
		default:
			return fmt.Errorf("cannot convert %T to time.Time", src)
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
