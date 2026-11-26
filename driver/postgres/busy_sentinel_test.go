package postgres

import "errors"

// isRowsOpenErr reports whether err is the error a statement gets when it is
// started on a conn that has an open, still-streaming Rows.
func isRowsOpenErr(err error) bool { return errors.Is(err, ErrRowsOpen) }
