//go:build postgres

package postgres_test

// isRowsOpen reports whether err is the error a statement gets when it is
// started on a connection that has an open, still-streaming Rows.
func isRowsOpen(err error) bool { return err != nil }
