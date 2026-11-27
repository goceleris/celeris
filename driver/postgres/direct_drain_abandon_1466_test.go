package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// A direct conn whose statement's context ended drains the cancelled
// statement's reply (cancelDrainDirect) to keep the wire aligned. The rows that
// reach it in that drain belong to a call that already got the context's error:
// they are read and dropped, not buffered (celeris#1466). The backend here
// sends 300,000 DataRow messages only after it has read the CancelRequest, so
// every one of them is read by the drain, strictly after the context ended; a
// backend that ignores the cancel (or reads it after the statement produced its
// reply) behaves the same. The call must still end with the context's error and
// leave the conn usable (the drain ran to ReadyForQuery).
func TestDirectDrainDropsRowsAfterTheContextEnded(t *testing.T) {
	const before, after = 100, 300000

	sent := make(chan struct{}, 1)       // the first `before` rows are on the wire
	cancelSeen := make(chan struct{}, 1) // the CancelRequest reached the backend

	oneRow := func() []byte {
		payload := buildDataRow([][]byte{[]byte("x")})
		msg := make([]byte, 5, 5+len(payload))
		msg[0] = protocol.BackendDataRow
		binary.BigEndian.PutUint32(msg[1:5], uint32(4+len(payload)))
		return append(msg, payload...)
	}()
	repeat := func(n int) []byte {
		buf := make([]byte, 0, n*len(oneRow))
		for i := 0; i < n; i++ {
			buf = append(buf, oneRow...)
		}
		return buf
	}
	beforeRows, afterRows := repeat(before), repeat(after)
	cols := []colSpec{{name: "v", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText}}

	addr := startFakePG(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		var l [4]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint32(l[:]))
		if n < 8 {
			return
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(c, body); err != nil {
			return
		}
		if n == 16 && int32(binary.BigEndian.Uint32(body[0:4])) == cancelRequestCode {
			cancelSeen <- struct{}{} // the deferred Close ends the cancel exchange
			return
		}
		if writeAuthOK(c) != nil || writeBackendKeyData(c, 7, 9) != nil || writeReadyForQuery(c, 'I') != nil {
			return
		}
		for {
			typ, payload, err := readMsg(c)
			if err != nil || typ == protocol.MsgTerminate {
				return
			}
			if typ != protocol.MsgQuery {
				continue
			}
			_ = writeMsg(c, protocol.BackendRowDescription, buildRowDescription(cols))
			if string(payload) == "SELECT next\x00" {
				_ = writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{[]byte("next")}))
				_ = writeCommandComplete(c, "SELECT 1")
				_ = writeReadyForQuery(c, 'I')
				continue
			}
			_, _ = c.Write(beforeRows)
			sent <- struct{}{}
			<-cancelSeen // the statement does not act on the cancel: the rest of its reply follows it
			_, _ = c.Write(afterRows)
			_ = writeCommandComplete(c, "SELECT 300100")
			_ = writeReadyForQuery(c, 'I')
		}
	})

	host, port, _ := net.SplitHostPort(addr)
	c, err := dialDirectConn(context.Background(), DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // the context ends once the first rows are on the wire, not after a delay
		<-sent
		cancel()
	}()

	got := 0
	var callErr error
	watchdog(t, 20*time.Second, "the cancelled query", func() {
		rows, err := c.QueryContext(ctx, "SELECT big", nil)
		if err != nil {
			callErr = err
			return
		}
		dest := make([]driver.Value, 1)
		for {
			if callErr = rows.Next(dest); callErr != nil {
				break
			}
			got++
		}
		_ = rows.Close()
	})
	if !errors.Is(callErr, context.Canceled) {
		t.Fatalf("the call ended with %v, want the context's error", callErr)
	}
	if got > before {
		t.Fatalf("%d rows reached the caller, at most %d were sent before the context ended: the rest were buffered by the drain after it", got, before)
	}

	// The drain ran to ReadyForQuery: the conn is reusable and the next
	// statement gets its own reply, not the cancelled one's.
	if c.closed.Load() {
		t.Fatal("the conn was failed although the cancelled statement was drained")
	}
	rows, err := c.QueryContext(context.Background(), "SELECT next", nil)
	if err != nil {
		t.Fatalf("the statement after the cancelled one: %v", err)
	}
	dest := make([]driver.Value, 1)
	if err := rows.Next(dest); err != nil {
		t.Fatalf("the statement after the cancelled one: %v", err)
	}
	if s, _ := dest[0].(string); s != "next" {
		if b, _ := dest[0].([]byte); string(b) != "next" {
			t.Fatalf("the statement after the cancelled one read %v, want its own row", dest[0])
		}
	}
	_ = rows.Close()
}
