package postgres

// isRowsOpenErr reports whether err is the error a statement gets when it is
// started on a conn that has an open, still-streaming Rows. (Any error, until
// the sentinel exists.)
func isRowsOpenErr(err error) bool { return err != nil }
