// Package postgresstore provides a [store.KV] session backend built on
// the native celeris [driver/postgres] client. Sessions are persisted
// as BYTEA values under a configurable table (default "celeris_sessions").
//
// The schema is created on first [New] call if it does not exist. A
// background cleanup goroutine periodically removes expired rows. Call
// [Store.Close] to stop the goroutine in tests or graceful shutdown
// paths.
//
// TLS: driver/postgres v1.4.0 does not support sslmode=require; use
// sslmode=disable on loopback / VPC deployments.
package postgresstore

import (
	"context"
	"errors"
	"fmt"
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
}

// Store is a [store.KV] backed by a PostgreSQL table. Implements
// [store.PrefixDeleter] via TRUNCATE (empty prefix clears the table).
type Store struct {
	pool  *postgres.Pool
	table string

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	closed bool
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
	}
	if !validIdent(o.TableName) {
		return nil, fmt.Errorf("postgresstore: invalid TableName %q — must be [A-Za-z_][A-Za-z0-9_]*", o.TableName)
	}
	if pool == nil {
		return nil, errors.New("postgresstore: pool must not be nil")
	}

	s := &Store{pool: pool, table: o.TableName}

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

func (s *Store) ensureSchema(ctx context.Context) error {
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
		if _, err := s.pool.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("postgresstore: ensure schema: %w", err)
		}
	}
	return nil
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
	q := fmt.Sprintf(`SELECT value FROM %s WHERE id=$1 AND (expires_at IS NULL OR expires_at > NOW())`, s.table)
	rows, err := s.pool.QueryContext(ctx, q, key)
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
	q := fmt.Sprintf(`INSERT INTO %s (id, value, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET value=EXCLUDED.value, expires_at=EXCLUDED.expires_at`, s.table)
	_, err := s.pool.ExecContext(ctx, q, key, value, expires)
	return err
}

// Delete implements [store.KV].
func (s *Store) Delete(ctx context.Context, key string) error {
	q := fmt.Sprintf(`DELETE FROM %s WHERE id=$1`, s.table)
	_, err := s.pool.ExecContext(ctx, q, key)
	return err
}

// DeletePrefix implements [store.PrefixDeleter]. An empty prefix
// TRUNCATEs the table; any other prefix runs a DELETE with a LIKE
// predicate. Prefix characters are escaped to avoid LIKE wildcards
// being interpreted.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		q := fmt.Sprintf(`TRUNCATE TABLE %s`, s.table)
		_, err := s.pool.ExecContext(ctx, q)
		return err
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE id LIKE $1 ESCAPE '\'`, s.table)
	_, err := s.pool.ExecContext(ctx, q, escapeLike(prefix)+"%")
	return err
}

func (s *Store) cleanupLoop(ctx context.Context, interval time.Duration) {
	defer close(s.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	q := fmt.Sprintf(`DELETE FROM %s WHERE expires_at IS NOT NULL AND expires_at <= NOW()`, s.table)
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
		first := i == 0
		if first {
			if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
				return false
			}
			continue
		}
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
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
