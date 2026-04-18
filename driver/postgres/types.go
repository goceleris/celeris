package postgres

// Conn is the public alias of the driver's underlying connection type. It
// is exposed so that callers using database/sql can reach celeris-specific
// connection features (Savepoint, ReleaseSavepoint, RollbackToSavepoint,
// ServerParam) via sql.Conn.Raw:
//
//	db.Conn(ctx, func(c *sql.Conn) error {
//	    return c.Raw(func(dc any) error {
//	        pc := dc.(*postgres.Conn)
//	        return pc.Savepoint(ctx, "sp1")
//	    })
//	})
//
// The type is otherwise opaque; methods intended for external use are
// defined on *pgConn in conn.go.
type Conn = pgConn
