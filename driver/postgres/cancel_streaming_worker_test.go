package postgres

// waitForQueryRows' ctx branch against a streaming reply nobody drains. The
// select may take ctx.Done although colsCh is ready too: rowCh is published
// and full, the event-loop worker blocks in sendRow on it, and the caller
// waited for a doneCh that only the worker can fire. The worker also serves
// every other connection registered on it. The caller must release the worker,
// let it read on to ReadyForQuery, and return; the next query on the connection
// must return its own rows.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// gateCtx is a context that is cancelled, whose Done blocks until gate is
// closed: waitForQueryRows evaluates ctx.Done() before it parks in its select,
// so the test controls the moment the select sees colsCh and ctx.Done ready
// together, with the worker already blocked on the full rowCh.
type gateCtx struct {
	context.Context
	gate chan struct{}
	done chan struct{}
}

func (g gateCtx) Done() <-chan struct{} { <-g.gate; return g.done }
func (g gateCtx) Err() error            { return context.Canceled }

func TestWaitForQueryRowsCtxReleasesBlockedWorker(t *testing.T) {
	const rows = 4000
	var queries atomic.Int32
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 0, 0, func(c net.Conn) {
			for {
				typ, _, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				q := queries.Add(1)
				_ = writeMsg(c, protocol.BackendRowDescription, buildRowDescription([]colSpec{{name: "v", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText}}))
				pad := strings.Repeat("x", 200)
				for i := 1; i <= rows; i++ {
					if err := writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{[]byte(fmt.Sprintf("q%d-%d-%s", q, i, pad))})); err != nil {
						return
					}
				}
				_ = writeCommandComplete(c, fmt.Sprintf("SELECT %d", rows))
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	l, err := eventloop.New(1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := dialConn(ctx, l, nil, dsn, 0)
	if err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	// Closed under a bound: with the worker blocked (the failure this test
	// reports) closing the loop could wait for it.
	defer func() {
		done := make(chan struct{})
		go func() { _ = conn.Close(); _ = l.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()
	conn.syncLoop = nil // the asynchronous path, which streams

	// The query is written as simpleQuery writes it; the wait is the call under test.
	start := func(ctx context.Context) *pgRequest {
		req := acquirePgRequest()
		req.ctx = ctx
		req.kind = reqSimple
		req.colsCh = make(chan struct{})
		conn.enqueue(req)
		conn.writerMu.Lock()
		payload := protocol.WriteQueryInto(conn.writer, "SELECT v FROM t")
		werr := conn.writeRaw(payload)
		conn.writerMu.Unlock()
		if werr != nil {
			t.Fatal(werr)
		}
		return req
	}

	// With colsCh and ctx.Done ready together the select picks either at random:
	// repeat until it picks ctx.Done (a pick of colsCh returns the stream, which
	// is closed and drained).
	branch := ""
	for i := 0; i < 64 && branch == ""; i++ {
		gate := make(chan struct{})
		g := gateCtx{Context: context.Background(), gate: gate, done: make(chan struct{})}
		close(g.done)
		req := start(g)
		// Wait until the worker has promoted the request and blocks in sendRow
		// on the full rowCh (sendRow creates abortCh when it has to wait).
		deadline := time.Now().Add(10 * time.Second)
		for {
			req.chMu.Lock()
			blocked := req.rowCh != nil && req.abortCh != nil
			req.chMu.Unlock()
			if blocked {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: the worker never blocked on rowCh", i)
			}
			time.Sleep(time.Millisecond)
		}
		type res struct {
			rows driver.Rows
			err  error
		}
		out := make(chan res, 1)
		go func() {
			r, err := conn.waitForQueryRows(g, req, true)
			out <- res{r, err}
		}()
		close(gate)
		select {
		case r := <-out:
			if r.err == nil {
				branch = "" // colsCh won: the streaming rows
				_ = r.rows.Close()
				continue
			}
			if r.err != context.Canceled {
				t.Fatalf("iteration %d: err = %v, want context.Canceled", i, r.err)
			}
			branch = "ctx"
		case <-time.After(15 * time.Second):
			t.Fatalf("iteration %d: waitForQueryRows did not return 15 s after its ctx ended: the worker is blocked in sendRow on a full rowCh that nothing drains", i)
		}
	}
	if branch != "ctx" {
		t.Fatal("the select never took ctx.Done in 64 tries")
	}

	// The connection is reusable and clean: the next query returns its own rows.
	want := queries.Load() + 1
	nctx, ncancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer ncancel()
	next := make(chan error, 1)
	go func() {
		r, err := conn.QueryContext(nctx, "SELECT v FROM t", nil)
		if err != nil {
			next <- err
			return
		}
		defer func() { _ = r.Close() }()
		dest := make([]driver.Value, 1)
		n := 0
		for {
			if err := r.Next(dest); err != nil {
				if n != rows {
					next <- fmt.Errorf("%d rows, want %d (%v)", n, rows, err)
					return
				}
				next <- nil
				return
			}
			n++
			s, _ := dest[0].(string)
			if b, ok := dest[0].([]byte); ok {
				s = string(b)
			}
			if prefix := fmt.Sprintf("q%d-%d-", want, n); !strings.HasPrefix(s, prefix) {
				next <- fmt.Errorf("row %d = %.40q, want prefix %q: a row of another query surfaced", n, s, prefix)
				return
			}
		}
	}()
	select {
	case err := <-next:
		if err != nil {
			t.Fatalf("the query after the cancelled one: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the query after the cancelled one never returned")
	}
}

// A caller whose context ended before the request was promoted to a stream
// abandons it: the rows that still arrive (the server may not act on the cancel
// at once, or at all when it reaches an idle backend) are dropped, not buffered
// (the rest of the result, whatever its size, would sit in req.rows until the
// reply ends), and the request is never promoted to a stream nobody reads.
func TestAbandonedRequestDropsRows(t *testing.T) {
	row := [][]byte{[]byte("a"), []byte("b")}
	req := acquirePgRequest()
	defer releasePgRequest(req)
	req.kind = reqSimple
	req.colsCh = make(chan struct{})
	for i := 0; i < 10; i++ {
		req.OnRow(row)
	}
	if len(req.rows) != 10 {
		t.Fatalf("control: %d rows buffered before the abandon, want 10", len(req.rows))
	}

	req.doneMu.Lock()
	req.abandoned = true
	req.doneMu.Unlock()
	req.closeRowCh()
	for i := 0; i < 1000; i++ {
		req.OnRow(row)
	}
	if len(req.rows) != 10 {
		t.Fatalf("%d rows buffered after the abandon, want the 10 of before it: later rows are dropped", len(req.rows))
	}
	req.chMu.Lock()
	streaming := req.rowCh != nil
	req.chMu.Unlock()
	if streaming {
		t.Fatal("an abandoned request was promoted to a stream")
	}
}
