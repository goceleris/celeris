package postgres

import (
	"errors"
	"io"
	"testing"
)

// The native Rows.Close returns the error the result ended with, also when the
// loop already read it from Rows.Err (the godoc of Rows.Close says so), and a
// second Close returns nil.
func TestConnLossNativeCloseReturnsErrAfterDrain(t *testing.T) {
	for _, engine := range []string{"loop", "direct"} {
		for _, n := range []int{50, 500} {
			t.Run(engine, func(t *testing.T) {
				addr := startFakePG(t, lossServer(t, lossCfg{kind: lossAfterLast, n: n}))
				p := lossPool(t, addr, engine == "direct")
				rows, err := p.QueryContext(tctx(t), "SELECT g FROM t")
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
				}
				if rows.Err() == nil {
					t.Fatalf("Err() == nil after %d rows", n)
				}
				cerr := rows.Close()
				if !errors.Is(cerr, ErrClosed) || !errors.Is(cerr, io.ErrUnexpectedEOF) {
					t.Fatalf("Close after a drained, cut-off result = %v, want ErrClosed and io.ErrUnexpectedEOF (the error Err() returned)", cerr)
				}
				if again := rows.Close(); again != nil {
					t.Fatalf("second Close = %v, want nil", again)
				}
			})
		}
	}
}

// QueryRow returns a server error that arrives after its first row (behaviour
// change: it returned the row and dropped the error), on the native Pool and on
// database/sql, in direct mode and on the loop.
func TestQueryRowReturnsServerErrorAfterRow(t *testing.T) {
	for _, engine := range []string{"loop", "direct", "sql"} {
		t.Run(engine, func(t *testing.T) {
			addr := startFakePG(t, echoPGServer(t))
			var err error
			var v int64
			if engine == "sql" {
				err = lossSQL(t, addr).QueryRowContext(tctx(t), "SELECT err").Scan(&v)
			} else {
				err = lossPool(t, addr, engine == "direct").QueryRow(tctx(t), "SELECT err").Scan(&v)
			}
			var pe *PGError
			if !errors.As(err, &pe) || pe.Code != "22012" {
				t.Fatalf("QueryRow(...).Scan = %v (v=%d), want the server's 22012 after the first row", err, v)
			}
		})
	}
}
