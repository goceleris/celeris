package postgres

import (
	"database/sql/driver"
	"errors"

	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// pgResult implements driver.Result. PG doesn't expose a generic
// LastInsertId (returning clauses are query-level), so that method always
// errors.
type pgResult struct {
	// n is the pre-parsed row count from the CommandComplete tag. We
	// store the count directly (not the tag string) so RowsAffected is
	// zero-alloc and the Exec hot path doesn't need to materialize a
	// tag string for every round trip.
	n int64
	// tag is the string form of the CommandComplete tag. Empty when
	// constructed via newPGResultFromCount — callers that need the tag
	// string should use newPGResult.
	tag string
}

func newPGResult(tag string) driver.Result {
	n, _ := protocol.RowsAffected(tag)
	return &pgResult{n: n, tag: tag}
}

// newPGResultFromCount constructs a pgResult carrying only the pre-parsed
// row count. Used by the Exec hot path which parses the count via
// protocol.RowsAffectedBytes directly off the CommandComplete bytes,
// skipping the tag-string allocation.
func newPGResultFromCount(n int64) driver.Result {
	return &pgResult{n: n}
}

// ErrNoLastInsertId is returned from pgResult.LastInsertId. PG exposes
// sequence values via RETURNING rather than a generic last-insert-id.
var ErrNoLastInsertId = errors.New("celeris-postgres: LastInsertId is not supported; use RETURNING")

func (r *pgResult) LastInsertId() (int64, error) {
	return 0, ErrNoLastInsertId
}

func (r *pgResult) RowsAffected() (int64, error) {
	return r.n, nil
}
