package postgres

import (
	"errors"
	"io"
	"testing"
)

// A stream that the connection's teardown cuts off ends with an error, not with
// the end of the rows. The teardown comes from the caller (Close, a discard: the
// native Tx.Rollback of a connection with an open stream does it) or from the
// loop (the peer's FIN reaches onClose as nil): a Rows being drained afterwards
// must report it from Err().
func TestConnLossOpenStreamTeardown(t *testing.T) {
	for _, how := range []string{"conn.Close", "onClose(nil)", "onClose(EOF)"} {
		for _, tx := range []bool{false, true} {
			name := how
			if tx {
				name += "/tx"
			}
			t.Run(name, func(t *testing.T) {
				addr := startFakePG(t, lossServer(t, lossCfg{kind: lossStall, n: 500}))
				p := lossPool(t, addr, false)
				ctx := tctx(t)
				var rows *Rows
				var conn *pgConn
				var err error
				if tx {
					var x *Tx
					x, err = p.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					rows, err = x.QueryContext(ctx, "SELECT g FROM t")
					conn = x.conn
				} else {
					rows, err = p.QueryContext(ctx, "SELECT g FROM t")
					if rows != nil {
						conn = rows.conn
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 3; i++ {
					if !rows.Next() {
						t.Fatalf("Next=false after %d rows, Err=%v", i, rows.Err())
					}
				}
				switch how {
				case "conn.Close":
					_ = conn.Close()
				case "onClose(nil)":
					conn.onClose(nil)
				case "onClose(EOF)":
					conn.onClose(io.EOF)
				}
				n := 3
				for rows.Next() {
					n++
				}
				err = rows.Err()
				if err == nil {
					t.Fatalf("the stream ended with Err()==nil after %d of 260 rows", n)
				}
				if errors.Is(err, io.EOF) || !errors.Is(err, ErrClosed) || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("Err() = %v: want ErrClosed and io.ErrUnexpectedEOF, never io.EOF", err)
				}
				_ = rows.Close()
			})
		}
	}
}
