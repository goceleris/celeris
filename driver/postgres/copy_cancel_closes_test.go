package postgres

// A COPY FROM whose context ends while it waits for the CopyInResponse returns the
// context's error at once and closes the connection (copyFrom): a server that
// has not answered, or that answers later and waits in COPY IN for data, is not
// waited for, and the conn is not reused with its COPY in an unknown state.

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

func TestCopyFromCtxBeforeCopyInResponseClosesConn(t *testing.T) {
	gotQuery := make(chan struct{}, 1)
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 0, 0, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ == protocol.MsgQuery {
					gotQuery <- struct{}{} // and never answers
				}
			}
		})
	})
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	l, err := eventloop.New(1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, l, nil, dsn, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	cctx, ccancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() {
		_, err := conn.copyFrom(cctx, "t", []string{"a"}, CopyFromSlice([][]any{{1}}))
		errc <- err
	}()
	<-gotQuery
	ccancel()
	select {
	case err := <-errc:
		if err != context.Canceled {
			t.Fatalf("copyFrom returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("copyFrom did not return after its context ended before the CopyInResponse: it waits for a reply that never completes the request")
	}
	if !conn.closed.Load() {
		t.Fatal("the conn was not closed: it would be reused with its COPY in an unknown state")
	}
}
