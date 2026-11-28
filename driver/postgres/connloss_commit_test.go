package postgres

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A COMMIT whose CommandComplete or ReadyForQuery never arrived is not a commit
// the caller can count: Tx.Commit returns an error (the transaction may or may
// not have been committed on the server; that is what the error says), on
// database/sql and on the native Tx, on the event loop and in direct mode.
func commitServer(tb testing.TB, afterC bool) func(net.Conn) {
	return func(c net.Conn) {
		fakePGTrustStartupB(tb, c, 1, 2, func(c net.Conn) {
			for {
				typ, body, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				q := strings.TrimRight(string(body), "\x00")
				switch q {
				case "BEGIN":
					_ = writeCommandComplete(c, "BEGIN")
					_ = writeReadyForQuery(c, 'T')
				case "COMMIT":
					if afterC {
						_ = writeCommandComplete(c, "COMMIT")
					}
					time.Sleep(20 * time.Millisecond)
					_ = c.Close()
					return
				default:
					_ = writeCommandComplete(c, "SELECT 0")
					_ = writeReadyForQuery(c, 'T')
				}
			}
		})
	}
}

func TestConnLossCommitIsNotSuccess(t *testing.T) {
	for _, afterC := range []bool{false, true} {
		for _, carrier := range []string{"sqlTx", "poolTx-loop", "poolTx-direct"} {
			t.Run(fmt.Sprintf("%s/afterC=%v", carrier, afterC), func(t *testing.T) {
				addr := startFakePG(t, commitServer(t, afterC))
				ctx := tctx(t)
				var err error
				switch carrier {
				case "sqlTx":
					db := lossSQL(t, addr)
					tx, berr := db.BeginTx(ctx, nil)
					if berr != nil {
						t.Fatal(berr)
					}
					err = tx.Commit()
				default:
					p := lossPool(t, addr, carrier == "poolTx-direct")
					tx, berr := p.BeginTx(ctx, nil)
					if berr != nil {
						t.Fatal(berr)
					}
					err = tx.Commit()
				}
				if err == nil {
					t.Fatalf("Commit returned nil but the COMMIT never completed")
				}
				if err == io.EOF || (errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) {
					t.Fatalf("Commit returned a bare io.EOF: %v", err)
				}
			})
		}
	}
}
