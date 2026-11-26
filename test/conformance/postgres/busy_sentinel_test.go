//go:build postgres

package postgres_test

import (
	"errors"

	"github.com/goceleris/celeris/driver/postgres"
)

// isRowsOpen reports whether err is the error a statement gets when it is
// started on a connection that has an open, still-streaming Rows.
func isRowsOpen(err error) bool { return errors.Is(err, postgres.ErrRowsOpen) }
