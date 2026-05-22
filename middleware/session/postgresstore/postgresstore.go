// Package postgresstore provides a [store.KV] session backend built on
// the native celeris [driver/postgres] client. Sessions are persisted
// as BYTEA values under a configurable table (default "celeris_sessions").
//
// The schema is created on first [New] call if it does not exist. A
// background cleanup goroutine periodically removes expired rows. Call
// [Store.Close] to stop the goroutine in tests or graceful shutdown
// paths.
//
// TLS: driver/postgres does not yet support sslmode=require; use
// sslmode=disable on loopback / VPC deployments.
package postgresstore

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
	"github.com/goceleris/celeris/middleware/store"
)

// Options configure the PostgreSQL-backed session store.
type Options struct {
	// TableName is the target table. Default: "celeris_sessions".
	TableName string

	// CleanupInterval is how often expired rows are removed. Default:
	// 5 minutes. Zero disables cleanup entirely (rows still expire on
	// Get, but the table grows unbounded with never-read keys).
	CleanupInterval time.Duration

	// CleanupContext, when set, cancels the cleanup goroutine when
	// the context is done. If nil, Close is the only way to stop the
	// goroutine.
	CleanupContext context.Context

	// SkipSchemaInit, when true, suppresses the CREATE TABLE IF NOT
	// EXISTS statement issued by New. Use this when the DBA has
	// provisioned the schema out-of-band or the role does not have
	// DDL privileges.
	SkipSchemaInit bool

	// PhaseHook, when non-nil, is invoked with a short tag at each
	// significant step of New() / ensureSchema(). Tags:
	//
	//   ensure_schema:begin_tx           — pool.BeginTx() about to run
	//   ensure_schema:set_lock_timeout   — SET LOCAL lock_timeout
	//   ensure_schema:acquire_lock       — pg_advisory_xact_lock
	//   ensure_schema:lock_acquired      — lock now held
	//   ensure_schema:run_ddl            — CREATE TABLE / INDEX
	//   ensure_schema:commit             — tx.Commit() about to run
	//   ensure_schema:done               — committed cleanly
	//
	// Used by diagnostic harnesses (probatorium matrix nightly) to
	// pinpoint which step blocks under pathological contention. The
	// hook MUST NOT block — it runs inline on the schema-init path
	// and is called with no locks held but on the same goroutine as
	// the SQL operations. Production deployments leave this nil.
	PhaseHook func(phase string)
}

// Store is a [store.KV] backed by a PostgreSQL table. Implements
// [store.PrefixDeleter] via TRUNCATE (empty prefix clears the table).
type Store struct {
	pool  *postgres.Pool
	table string

	// Pre-formatted SQL templates — table name is immutable after
	// construction, so fmt.Sprintf'ing once here avoids an alloc per
	// Get/Set/Delete call.
	qGet          string
	qSet          string
	qDelete       string
	qTruncate     string
	qDeletePrefix string
	qCleanup      string

	// phaseHook is copied from Options.PhaseHook at New() time and
	// remains immutable for the lifetime of the Store. Used today
	// only by ensureSchema's diagnostic path; future Set/Get-level
	// phase events would attach here too. Nil = no-op.
	phaseHook func(phase string)

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	closed bool
}

// emitPhase invokes s.phaseHook if set. Centralised so callers don't
// each carry a nil-check.
func (s *Store) emitPhase(phase string) {
	if s.phaseHook != nil {
		s.phaseHook(phase)
	}
}

// New creates a PostgreSQL-backed session store. The schema is
// auto-initialized on first call unless [Options.SkipSchemaInit] is
// true. A background cleanup goroutine is started if
// [Options.CleanupInterval] > 0.
func New(ctx context.Context, pool *postgres.Pool, opts ...Options) (*Store, error) {
	o := Options{TableName: "celeris_sessions", CleanupInterval: 5 * time.Minute}
	if len(opts) > 0 {
		if opts[0].TableName != "" {
			o.TableName = opts[0].TableName
		}
		if opts[0].CleanupInterval != 0 {
			o.CleanupInterval = opts[0].CleanupInterval
		}
		o.SkipSchemaInit = opts[0].SkipSchemaInit
		o.CleanupContext = opts[0].CleanupContext
		o.PhaseHook = opts[0].PhaseHook
	}
	if !validIdent(o.TableName) {
		return nil, fmt.Errorf("postgresstore: invalid TableName %q — must be [A-Za-z_][A-Za-z0-9_]*", o.TableName)
	}
	if pool == nil {
		return nil, errors.New("postgresstore: pool must not be nil")
	}

	s := &Store{
		pool:  pool,
		table: o.TableName,
		qGet: fmt.Sprintf(
			`SELECT value FROM %s WHERE id=$1 AND (expires_at IS NULL OR expires_at > NOW())`,
			o.TableName,
		),
		qSet: fmt.Sprintf(`INSERT INTO %s (id, value, expires_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value, expires_at=EXCLUDED.expires_at`, o.TableName),
		qDelete:       fmt.Sprintf(`DELETE FROM %s WHERE id=$1`, o.TableName),
		qTruncate:     fmt.Sprintf(`TRUNCATE TABLE %s`, o.TableName),
		qDeletePrefix: fmt.Sprintf(`DELETE FROM %s WHERE id LIKE $1 ESCAPE '\'`, o.TableName),
		qCleanup:      fmt.Sprintf(`DELETE FROM %s WHERE expires_at IS NOT NULL AND expires_at <= NOW()`, o.TableName),
		phaseHook:     o.PhaseHook,
	}

	if !o.SkipSchemaInit {
		if err := s.ensureSchema(ctx); err != nil {
			return nil, err
		}
	}

	if o.CleanupInterval > 0 {
		parent := o.CleanupContext
		if parent == nil {
			parent = context.Background()
		}
		goctx, cancel := context.WithCancel(parent)
		s.cancel = cancel
		s.done = make(chan struct{})
		go s.cleanupLoop(goctx, o.CleanupInterval)
	}

	return s, nil
}

// ensureSchemaLockTimeout caps the worst-case wait when acquiring the
// transaction-scoped advisory lock that serializes schema-init racers.
// Set as `SET LOCAL lock_timeout = '3s'` on the schema-init tx; with the
// lock held only across two trivial IF-NOT-EXISTS DDL statements +
// COMMIT (typical wall time ~10 ms), 3 s is two orders of magnitude
// above the normal-case wait — long enough to absorb a brief stall, far
// short of the validator-side 10 s ReadyTimeout that bounds refapp
// boot.
//
// Surfaced by probatorium soak 26132324582 (post-v1.4.9): 6 errored
// Tier-3 seeds timed out at exactly 10.001 s (sub-millisecond precision
// at the validator's ReadyTimeout boundary) with no captured refapp
// stderr — only possible if blocked silently in this advisory-lock
// path. Pre-fix, the lock had no timeout; a stuck holder (e.g. orphaned
// conn awaiting TCP-keepalive grace) could block waiters indefinitely.
// Post-fix, contended waiters surface SQLSTATE 55P03 (lock_not_available)
// within 3 s and the caller can decide whether to retry, fail fast, or
// log diagnostically.
const ensureSchemaLockTimeout = "3s"

// ensureSchema creates the sessions table and its supporting index if
// they do not already exist.
//
// The DDL is wrapped in a transaction-scoped advisory lock
// (pg_advisory_xact_lock) keyed on the table name. CREATE TABLE IF NOT
// EXISTS is NOT atomic in PostgreSQL: the existence pre-check happens
// outside the row-insertion path into pg_class/pg_type, so two
// concurrent racers can both pass the "does it exist?" gate and then
// race in the catalog. The loser surfaces SQLSTATE 23505 (unique
// violation on pg_type_typname_nsp_index) — a class of failure
// observed in the probatorium matrix nightly when both arch hosts
// (amd64 + arm64) boot a driver_postgres refapp simultaneously against
// the same PostgreSQL instance.
//
// The advisory key is derived from a 64-bit FNV-1a hash of
// "postgresstore:ensureSchema:" + table name. Different tables get
// different keys (no blocking between unrelated stores); the same
// table always gets the same key (concurrent racers serialize). The
// lock is released automatically at COMMIT/ROLLBACK.
//
// The lock acquisition is bounded by `SET LOCAL lock_timeout =
// ensureSchemaLockTimeout` — see the constant for the rationale.
func (s *Store) ensureSchema(ctx context.Context) error {
	s.emitPhase("ensure_schema:begin_tx")
	tx, err := s.pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgresstore: ensure schema: begin tx: %w", err)
	}
	// Defer rollback for the error paths; a no-op after Commit succeeds.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// SET LOCAL applies only for the duration of this transaction —
	// no global state mutation, no risk of leaking the override to
	// other code paths sharing the pool.
	s.emitPhase("ensure_schema:set_lock_timeout")
	if _, err := tx.ExecContext(ctx,
		"SET LOCAL lock_timeout = '"+ensureSchemaLockTimeout+"'"); err != nil {
		return fmt.Errorf("postgresstore: ensure schema: set lock_timeout: %w", err)
	}

	s.emitPhase("ensure_schema:acquire_lock")
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", advisoryLockKey(s.table)); err != nil {
		// Distinguishable failure modes:
		//  - 55P03 lock_not_available: another racer is holding the lock
		//    past lock_timeout. Caller can retry safely; the lock will
		//    eventually free even if the prior holder's conn was orphaned
		//    (postgres reclaims it via TCP keepalive).
		//  - anything else: legitimate error worth surfacing as-is.
		return fmt.Errorf("postgresstore: ensure schema: acquire advisory lock: %w", err)
	}
	s.emitPhase("ensure_schema:lock_acquired")

	s.emitPhase("ensure_schema:run_ddl")
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id TEXT PRIMARY KEY,
			value BYTEA NOT NULL,
			expires_at TIMESTAMPTZ
		)`, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_expires_at
			ON %s (expires_at) WHERE expires_at IS NOT NULL`, s.table, s.table),
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("postgresstore: ensure schema: %w", err)
		}
	}

	s.emitPhase("ensure_schema:commit")
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgresstore: ensure schema: commit: %w", err)
	}
	committed = true
	s.emitPhase("ensure_schema:done")
	return nil
}

// advisoryLockKey derives a stable int64 key for pg_advisory_xact_lock
// from the target table name. FNV-1a/64 is a non-cryptographic hash;
// collisions across unrelated table names are acceptable here — the
// worst case is two stores serializing their schema-init unnecessarily,
// not a correctness issue. The int64 cast preserves the bit pattern;
// PostgreSQL accepts the full int64 range.
func advisoryLockKey(table string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("postgresstore:ensureSchema:"))
	_, _ = h.Write([]byte(table))
	return int64(h.Sum64()) // #nosec G115 — intentional bitwise cast
}

// Close stops the cleanup goroutine. Safe to call multiple times.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}

// Get implements [store.KV].
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	rows, err := s.pool.QueryContext(ctx, s.qGet, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if rerr := rows.Err(); rerr != nil {
			return nil, rerr
		}
		return nil, store.ErrNotFound
	}
	var v []byte
	if err := rows.Scan(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Set implements [store.KV]. A ttl <= 0 stores NULL for expires_at
// (no expiry). Positive ttl converts to NOW() + INTERVAL.
//
// Expires is passed as either a time.Time (positive TTL) or nil (no
// expiry). The celeris-postgres driver accepts those forms natively;
// passing sql.NullTime would fail with "unsupported argument type".
func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	var expires any
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	} else {
		expires = nil
	}
	_, err := s.pool.ExecContext(ctx, s.qSet, key, value, expires)
	return err
}

// Delete implements [store.KV].
func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.pool.ExecContext(ctx, s.qDelete, key)
	return err
}

// DeletePrefix implements [store.PrefixDeleter]. An empty prefix
// TRUNCATEs the table; any other prefix runs a DELETE with a LIKE
// predicate. Prefix characters are escaped to avoid LIKE wildcards
// being interpreted.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		_, err := s.pool.ExecContext(ctx, s.qTruncate)
		return err
	}
	_, err := s.pool.ExecContext(ctx, s.qDeletePrefix, escapeLike(prefix)+"%")
	return err
}

func (s *Store) cleanupLoop(ctx context.Context, interval time.Duration) {
	defer close(s.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	q := s.qCleanup // hoisted local to keep the select body tight.
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Use a short-timeout sub-context so a stuck cleanup does not
			// wedge the goroutine on Close.
			subCtx, cancel := context.WithTimeout(ctx, interval/2+time.Second)
			_, _ = s.pool.ExecContext(subCtx, q)
			cancel()
		}
	}
}

// validIdent returns true when s is a SQL identifier ([A-Za-z_][A-Za-z0-9_]*).
// Used to validate the user-supplied table name before interpolating it into
// statements (no parameter binding is possible for identifiers in Postgres).
func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if i == 0 {
			if c != '_' && !isAlpha {
				return false
			}
			continue
		}
		if c != '_' && !isAlpha && !isDigit {
			return false
		}
	}
	return true
}

func escapeLike(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}
