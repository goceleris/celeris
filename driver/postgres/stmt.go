package postgres

import (
	"context"
	"database/sql/driver"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// pgStmt is a prepared statement backed by a server-side statement plus the
// post-describe metadata needed to decode result rows.
type pgStmt struct {
	conn   *pgConn
	prep   *protocol.PreparedStmt
	query  string
	cached bool
	closed bool
}

var (
	_ driver.Stmt             = (*pgStmt)(nil)
	_ driver.StmtExecContext  = (*pgStmt)(nil)
	_ driver.StmtQueryContext = (*pgStmt)(nil)
)

// Close releases the server-side statement. Cached entries are closed only
// when evicted from the LRU, not on pgStmt.Close — so multiple database/sql
// Stmt handles to the same query share a single server-side plan.
func (s *pgStmt) Close() error {
	s.closed = true
	return nil
}

// NumInput returns the parameter count from the Describe metadata. If the
// statement has not been described (e.g. the cache was populated by a
// prior call that skipped Describe) we return -1 so database/sql performs
// no pre-validation.
func (s *pgStmt) NumInput() int {
	if s.prep == nil {
		return -1
	}
	return len(s.prep.ParamOIDs)
}

// Exec implements driver.Stmt's deprecated Exec. database/sql prefers
// ExecContext but some older code paths still call this.
func (s *pgStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), toNamed(args))
}

// Query implements driver.Stmt's deprecated Query.
func (s *pgStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), toNamed(args))
}

// ExecContext runs Bind + Execute + Sync against the cached plan.
func (s *pgStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if s.conn == nil {
		return nil, ErrClosed
	}
	return s.conn.extendedExec(ctx, s.prep.Name, s.query, args)
}

// QueryContext runs Bind + Describe + Execute + Sync.
func (s *pgStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.conn == nil {
		return nil, ErrClosed
	}
	return s.conn.extendedQuery(ctx, s.prep.Name, s.query, args)
}

func toNamed(vals []driver.Value) []driver.NamedValue {
	if len(vals) == 0 {
		return nil
	}
	out := make([]driver.NamedValue, len(vals))
	for i, v := range vals {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}
