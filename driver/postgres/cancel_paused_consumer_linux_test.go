//go:build linux

package postgres

import (
	"context"
	"database/sql/driver"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// Linux only: one epoll worker serves every conn registered on it. The other
// loops give each conn a goroutine of its own, so a paused consumer holds no one
// else.

// A streaming consumer that is paused (a Rows held open and not read) holds the
// event-loop worker in sendRow, and so every other conn on that worker. That is
// the design of the bounded rowCh (back-pressure to the socket); what the
// victims on the same worker see is what this test pins:
//
//   - a statement under a context on another conn of the worker returns, with the
//     context's error, within the cancel drain bound (it used to wait for a
//     doneCh only the blocked worker could fire, for good) and its conn is
//     closed, so the pool replaces it;
//   - a NEW conn registered on that worker returns an error at its own connect
//     bound, not never (the startup cannot be read while the worker is held).
func TestPausedStreamingConsumerVictimsAreBounded(t *testing.T) {
	const rows = 4000
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 0, 0, func(c net.Conn) {
			for {
				typ, body, err := readMsg(c)
				if err != nil || typ == protocol.MsgTerminate {
					return
				}
				if typ != protocol.MsgQuery {
					continue
				}
				col := buildRowDescription([]colSpec{{name: "v", typeOID: protocol.OIDText, typeSize: -1, format: protocol.FormatText}})
				_ = writeMsg(c, protocol.BackendRowDescription, col)
				n := rows
				if strings.HasPrefix(string(body), "SELECT 1") {
					n = 1
				}
				pad := strings.Repeat("x", 200)
				for i := 1; i <= n; i++ {
					if err := writeMsg(c, protocol.BackendDataRow, buildDataRow([][]byte{[]byte(fmt.Sprintf("%d-%s", i, pad))})); err != nil {
						return
					}
				}
				_ = writeCommandComplete(c, fmt.Sprintf("SELECT %d", n))
				_ = writeReadyForQuery(c, 'I')
			}
		})
	})
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16, ConnectTimeout: time.Second}}

	oldDrain := cancelDrainTimeout
	cancelDrainTimeout = time.Second
	defer func() { cancelDrainTimeout = oldDrain }()

	l, err := eventloop.New(1) // one worker: every conn shares it
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	a, err := dialConn(ctx, l, nil, dsn, 0)
	if err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	b, err := dialConn(ctx, l, nil, dsn, 0)
	if err != nil {
		_ = l.Close()
		t.Fatal(err)
	}
	a.syncLoop, b.syncLoop = nil, nil // the asynchronous path
	var paused driver.Rows
	defer func() {
		// Resume the consumer (Close drains) so the worker is free, then close, bounded.
		done := make(chan struct{})
		go func() {
			if paused != nil {
				_ = paused.Close()
			}
			_ = a.Close()
			_ = b.Close()
			_ = l.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()

	// A streams and is paused: Rows taken, never read.
	paused, err = a.QueryContext(ctx, "SELECT v FROM big", nil)
	if err != nil {
		t.Fatal(err)
	}
	sr, ok := paused.(*streamRows)
	if !ok {
		t.Fatalf("A's rows are %T, want a stream", paused)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		sr.req.chMu.Lock()
		blocked := sr.req.abortCh != nil
		sr.req.chMu.Unlock()
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never blocked on the paused consumer's rowCh")
		}
	}

	// B, on the same worker, under a context.
	type res struct {
		err error
		d   time.Duration
	}
	out := make(chan res, 1)
	go func() {
		bctx, bcancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer bcancel()
		start := time.Now()
		r, err := b.QueryContext(bctx, "SELECT 1", nil)
		if r != nil {
			_ = r.Close()
		}
		out <- res{err, time.Since(start)}
	}()
	select {
	case r := <-out:
		if r.err == nil || r.d > 5*time.Second {
			t.Fatalf("B returned %v after %v, want the context's error within the %v drain bound", r.err, r.d, cancelDrainTimeout)
		}
		t.Logf("B (same worker, 200 ms context, drain bound %v): returned %v after %v", cancelDrainTimeout, r.err, r.d.Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Fatal("B did not return: a statement behind a paused consumer waits for a doneCh only the blocked worker can fire")
	}
	if !b.closed.Load() {
		t.Fatal("B's conn was not closed after the drain bound: it would be reused with its statement still pending")
	}

	// A NEW conn on the worker: bounded by its connect timeout, never forever.
	dout := make(chan res, 1)
	go func() {
		start := time.Now()
		c, err := dialConn(ctx, l, nil, dsn, 0)
		if c != nil {
			_ = c.Close()
		}
		dout <- res{err, time.Since(start)}
	}()
	select {
	case r := <-dout:
		t.Logf("a new conn on the same worker: err=%v after %v (connect_timeout 1 s)", r.err, r.d.Round(time.Millisecond))
		if r.d > 8*time.Second {
			t.Fatalf("the dial returned after %v, past its 1 s connect bound", r.d)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a dial on the worker held by a paused consumer never returned")
	}
}
