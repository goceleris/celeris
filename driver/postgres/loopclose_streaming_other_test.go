//go:build !linux && unix

package postgres

// Loop.Close while a streaming query is being read, on the descriptor path of
// the non-Linux worker. The fake server streams DataRows without end; the
// client consumes rows; the loop is closed under it. onClose must not run
// while onRecv still can: a "send on closed channel" panic in the loop's
// reader goroutine used to kill the test binary.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

func TestPgLoopCloseDuringStreamingDoesNotPanic(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 1, 2, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				rd := buildRowDescription([]colSpec{{name: "id", typeOID: protocol.OIDInt4, typeSize: 4, format: protocol.FormatText}})
				_ = writeMsg(c, protocol.BackendRowDescription, rd)
				for i := 0; ; i++ {
					if err := writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{[]byte(fmt.Sprintf("%d", i))})); err != nil {
						return
					}
				}
			}
		})
	})
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	for round := 0; round < 30; round++ {
		l, err := eventloop.New(1)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		conn, err := dialConn(ctx, l, nil, dsn, 0)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		rows, err := conn.QueryContext(ctx, "SELECT id FROM t", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		dest := make([]driver.Value, 1)
		consumed := make(chan struct{})
		go func() {
			defer close(consumed)
			for rows.Next(dest) == nil {
			}
		}()
		time.Sleep(5 * time.Millisecond)
		_ = l.Close()
		select {
		case <-consumed:
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: the row consumer never saw the end after Loop.Close", round)
		}
		_ = rows.Close()
		_ = conn.Close()
		cancel()
	}
}
