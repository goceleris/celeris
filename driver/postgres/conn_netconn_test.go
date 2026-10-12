package postgres

// The net.Conn transport (conn_netconn.go), which is the only transport on a
// platform without unix file descriptors (windows). It is compiled on every
// platform, so these tests run on Linux CI and on windows-latest alike, over
// a portable eventloop (eventloop.NewPortable) and an in-process fake server
// on loopback TCP.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
	"github.com/goceleris/celeris/internal/engine"
)

func netConnProvider(t *testing.T) *eventloop.Loop {
	t.Helper()
	l, err := eventloop.NewPortable(2)
	if err != nil {
		t.Fatalf("NewPortable: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func netConnDSN(t *testing.T, addr string) DSN {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	return DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 4}}
}

// netConnFakePG answers every simple Query with CommandComplete and counts
// the queries; it reports on sawEOF when the client's socket closes.
func netConnFakePG(t *testing.T, queries *atomic.Int32, sawEOF chan<- struct{}) string {
	t.Helper()
	return startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 7, 9, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil {
					select {
					case sawEOF <- struct{}{}:
					default:
					}
					return
				}
				switch typ {
				case protocol.MsgTerminate:
					continue // the client closes next: wait for EOF
				case protocol.MsgQuery:
					queries.Add(1)
					_ = writeCommandComplete(c, "SELECT 1")
					_ = writeReadyForQuery(c, 'I')
				}
			}
		})
	})
}

func TestPgNetConnStartupRoundTripAndClose(t *testing.T) {
	var queries atomic.Int32
	sawEOF := make(chan struct{}, 1)
	addr := netConnFakePG(t, &queries, sawEOF)
	prov := netConnProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := dialConnNetConn(ctx, prov, nil, netConnDSN(t, addr), 1)
	if err != nil {
		t.Fatalf("dialConnNetConn: %v", err)
	}
	if !eventloop.IsNetConnID(c.fd) {
		t.Fatalf("c.fd = %d, want a net.Conn identifier", c.fd)
	}
	if c.fdFile != nil || c.useDirect {
		t.Fatalf("fdFile=%v useDirect=%v: the conn must hold neither a descriptor nor a direct net.Conn", c.fdFile, c.useDirect)
	}
	if c.workerID != 1 {
		t.Fatalf("workerID = %d, want the hint 1", c.workerID)
	}
	if c.pid != 7 {
		t.Fatalf("backend pid = %d, want 7 (startup answered through the loop)", c.pid)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := c.simpleExec(ctx, "SELECT 1"); err != nil {
			t.Fatalf("simpleExec #%d: %v", i, err)
		}
	}
	if got := queries.Load(); got != 3 {
		t.Fatalf("server saw %d queries, want 3", got)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.loop.UnregisterConn(c.fd); !errors.Is(err, engine.ErrUnknownFD) {
		t.Fatalf("UnregisterConn after Close: got %v, want ErrUnknownFD", err)
	}
	select {
	case <-sawEOF:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the client's socket close")
	}
}

func TestPgNetConnServerHangUpFailsLaterQueries(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		// Startup, then hang up.
		fakePGTrustStartup(t, c, 1, 2, nil)
	})
	prov := netConnProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialConnNetConn(ctx, prov, nil, netConnDSN(t, addr), 0)
	if err != nil {
		t.Fatalf("dialConnNetConn: %v", err)
	}
	defer func() { _ = c.Close() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := c.simpleExec(ctx, "SELECT 1"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queries on a conn whose server hung up keep succeeding")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPgNetConnLoopCloseFailsTheConn(t *testing.T) {
	var queries atomic.Int32
	addr := netConnFakePG(t, &queries, make(chan struct{}, 1))
	prov := netConnProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := dialConnNetConn(ctx, prov, nil, netConnDSN(t, addr), 0)
	if err != nil {
		t.Fatalf("dialConnNetConn: %v", err)
	}
	if err := prov.Close(); err != nil {
		t.Fatalf("Loop.Close: %v", err)
	}
	// onClose(ErrLoopClosed) reached the conn: its queries fail, and Close
	// still returns (the loop already forgot the conn; no descriptor to leak).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := c.simpleExec(ctx, "SELECT 1"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queries keep succeeding after the loop closed")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung after the loop closed")
	}
}

type noNetConnProvider struct{}

type noNetConnWorker struct{ engine.WorkerLoop }

func (noNetConnProvider) NumWorkers() int                  { return 1 }
func (noNetConnProvider) WorkerLoop(int) engine.WorkerLoop { return noNetConnWorker{} }

func TestPgNetConnDialErrorsCloseTheSocket(t *testing.T) {
	closed := netConnProvider(t)
	_ = closed.Close() // a closed loop refuses RegisterNetConn with ErrLoopClosed
	for _, tc := range []struct {
		name string
		prov engine.EventLoopProvider
		want string
		is   error
	}{
		{"zero workers", zeroWorkerProvider{}, "0 workers", nil},
		{"worker cannot adopt a net.Conn", noNetConnProvider{}, "cannot adopt a net.Conn", nil},
		{"loop closed", closed, "", eventloop.ErrLoopClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sawEOF := make(chan struct{}, 1)
			accepted := make(chan struct{}, 1)
			addr := startFakePG(t, func(c net.Conn) {
				defer func() { _ = c.Close() }()
				accepted <- struct{}{}
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						sawEOF <- struct{}{}
						return
					}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := dialConnNetConn(ctx, tc.prov, nil, netConnDSN(t, addr), 0)
			switch {
			case err == nil:
				t.Fatal("dial succeeded")
			case tc.is != nil && !errors.Is(err, tc.is):
				t.Fatalf("dial error = %v, want %v", err, tc.is)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("dial error = %v, want one containing %q", err, tc.want)
			}
			select {
			case <-sawEOF:
			case <-time.After(5 * time.Second):
				t.Fatal("the server never saw the client's socket close")
			}
		})
	}
}
