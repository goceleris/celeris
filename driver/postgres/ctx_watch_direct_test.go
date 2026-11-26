package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// markerPG is a backend whose every reply carries the marker of its own
// statement, so that a crossed reply (the answer of one statement handed to the
// next one on the conn) and a conn that is not clean are visible, which a reply
// that is the same for every statement hides. Like a real backend it acts on a
// CancelRequest only while a statement is running (one that waits for it); a cancel
// that reaches an idle backend is dropped. Statements:
//
//	SELECT <x>         one row, x, at once
//	SELECT slow        no byte until a cancel; then 57014 + ReadyForQuery
//	SELECT drip        a RowDescription and a row, then as slow
//	SELECT drip2       as drip, with one more row 80 ms after the first, and the
//	                   57014 drainDelay after the cancel
//	SELECT draindelay  as slow, but the 57014 comes drainDelay after the cancel
//	SELECT copyout     CopyOutResponse and a few CopyData rows, then as slow
//	COPY ... FROM STDIN  CopyInResponse, then it reads CopyData until CopyDone or
//	                     CopyFail, and answers the way a backend does (57014 after
//	                     a CopyFail), or when a cancel arrives
type markerPG struct {
	addr       string
	drainDelay time.Duration
	// cancelHold: how long the cancel socket stays open after the request (a
	// backend's exit); the client waits for the close.
	cancelHold time.Duration

	// onQuery and afterReply run on the backend's goroutine: when a statement
	// reaches it (before any reply) and after its reply was written (for a
	// statement that waits for a cancel: when it is waiting).
	onQuery    atomic.Pointer[func(q string)]
	afterReply atomic.Pointer[func(q string)]

	mu       sync.Mutex
	log      []string
	sessions int
	cancels  int
	waiting  chan struct{} // non-nil while a statement waits for a cancel
	done     chan struct{}
}

func startMarkerPG(t *testing.T) *markerPG {
	t.Helper()
	f := &markerPG{done: make(chan struct{})}
	f.addr = startFakePG(t, f.serve)
	t.Cleanup(func() { close(f.done) })
	return f
}

func (f *markerPG) record(ev string) {
	f.mu.Lock()
	f.log = append(f.log, ev)
	f.mu.Unlock()
}

func (f *markerPG) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *markerPG) has(ev string) bool {
	for _, e := range f.events() {
		if e == ev {
			return true
		}
	}
	return false
}

func (f *markerPG) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions
}

func (f *markerPG) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}

// waitCancel blocks until a cancel reaches the backend while it waits.
func (f *markerPG) waitCancel() bool {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	f.waiting = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.waiting = nil
		f.mu.Unlock()
	}()
	if h := f.afterReply.Load(); h != nil {
		(*h)("waiting")
	}
	select {
	case <-ch:
		return true
	case <-f.done:
		return false
	}
}

func (f *markerPG) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	var lenBuf [4]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		return
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:]))
	if n < 8 {
		return
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return
	}
	if n == 16 && int32(binary.BigEndian.Uint32(body[0:4])) == cancelRequestCode {
		f.mu.Lock()
		f.cancels++
		w := f.waiting
		f.mu.Unlock()
		f.record("cancel-request")
		if w != nil {
			select {
			case w <- struct{}{}:
			default:
			}
		}
		if f.cancelHold > 0 {
			select {
			case <-time.After(f.cancelHold):
			case <-f.done:
			}
		}
		f.record("cancel-closed")
		return
	}
	f.mu.Lock()
	f.sessions++
	f.mu.Unlock()
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
		q := string(bytes.TrimRight(payload, "\x00"))
		f.record("query:" + q)
		if h := f.onQuery.Load(); h != nil {
			(*h)(q)
		}
		switch {
		case q == "SELECT slow", q == "SELECT drip", q == "SELECT drip2", q == "SELECT draindelay":
			if q == "SELECT drip" || q == "SELECT drip2" {
				_ = writeMsg(c, protocol.BackendRowDescription, markerRowDesc())
				_ = writeMsg(c, protocol.BackendDataRow, markerDataRow("partial"))
			}
			if q == "SELECT drip2" {
				// more bytes 80 ms later: the blocked read returns then
				select {
				case <-time.After(80 * time.Millisecond):
				case <-f.done:
					return
				}
				_ = writeMsg(c, protocol.BackendDataRow, markerDataRow("more"))
			}
			if !f.waitCancel() {
				return
			}
			if q == "SELECT draindelay" || q == "SELECT drip2" {
				select {
				case <-time.After(f.drainDelay):
				case <-f.done:
					return
				}
			}
			f.writeCancelled(c)
		case q == "SELECT copyout":
			_ = writeMsg(c, protocol.BackendCopyOutResponse, []byte{0, 0, 1, 0, 0})
			for i := range 3 {
				_ = writeMsg(c, protocol.BackendCopyData, fmt.Appendf(nil, "row%d\n", i))
			}
			if !f.waitCancel() {
				return
			}
			f.writeCancelled(c)
		case len(q) > 4 && q[:4] == "COPY" && bytes.Contains([]byte(q), []byte("FROM STDIN")):
			if !f.serveCopyIn(c) {
				return
			}
		default:
			marker := q
			if len(q) > 7 && q[:7] == "SELECT " {
				marker = q[7:]
			}
			_ = writeMsg(c, protocol.BackendRowDescription, markerRowDesc())
			_ = writeMsg(c, protocol.BackendDataRow, markerDataRow(marker))
			_ = writeCommandComplete(c, "SELECT 1")
			_ = writeReadyForQuery(c, 'I')
			if h := f.afterReply.Load(); h != nil {
				(*h)(q)
			}
		}
	}
}

func (f *markerPG) writeCancelled(c net.Conn) {
	_ = writeMsg(c, protocol.BackendErrorResponse, []byte("SERROR\x00VERROR\x00C57014\x00Mcanceling statement due to user request\x00\x00"))
	_ = writeReadyForQuery(c, 'I')
}

// serveCopyIn runs a COPY FROM STDIN: it reads the client's frames until CopyDone
// (CommandComplete), CopyFail (57014) or a cancel. It reports whether the session
// goes on.
func (f *markerPG) serveCopyIn(c net.Conn) bool {
	_ = writeMsg(c, protocol.BackendCopyInResponse, []byte{0, 0, 1, 0, 0})
	cancelled := make(chan struct{}, 1)
	f.mu.Lock()
	f.waiting = cancelled
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.waiting = nil
		f.mu.Unlock()
	}()
	if h := f.afterReply.Load(); h != nil {
		(*h)("copyin")
	}
	frames := make(chan byte, 64)
	go func() {
		defer close(frames)
		for {
			typ, _, err := readMsg(c)
			if err != nil {
				return
			}
			frames <- typ
			if typ == protocol.MsgCopyDone || typ == protocol.MsgCopyFail {
				return
			}
		}
	}()
	for {
		select {
		case <-cancelled:
			f.writeCancelled(c)
			// the reader goroutine still takes the frames the client sends until
			// it ends its copy; wait for it
			for range frames {
			}
			return true
		case typ, ok := <-frames:
			if !ok {
				return false
			}
			switch typ {
			case protocol.MsgCopyDone:
				_ = writeCommandComplete(c, "COPY 1")
				_ = writeReadyForQuery(c, 'I')
				return true
			case protocol.MsgCopyFail:
				f.writeCancelled(c)
				return true
			}
		case <-f.done:
			return false
		}
	}
}

func markerRowDesc() []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, 1)
	b = append(b, "m\x00"...)
	b = binary.BigEndian.AppendUint32(b, 0)
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint32(b, 25) // text
	b = binary.BigEndian.AppendUint16(b, 0xffff)
	b = binary.BigEndian.AppendUint32(b, 0xffffffff)
	b = binary.BigEndian.AppendUint16(b, 0)
	return b
}

func markerDataRow(v string) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, 1)
	b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
	return append(b, v...)
}

// watchdog runs fn and fails the test, with the stacks of every goroutine, when
// it does not return within d.
func watchdog(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("watchdog: %s did not return within %v\n%s", what, d, buf)
	}
}

func openMarkerPool(t *testing.T, f *markerPG) *Pool {
	t.Helper()
	return openDirectPool(t, f.addr)
}

// marker runs "SELECT <want>" on a background context and returns the marker the
// backend answered with (the statement's own).
func marker(ctx context.Context, p *Pool, want string) (string, error) {
	rows, err := p.QueryContext(ctx, "SELECT "+want)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var got string
	n := 0
	for rows.Next() {
		if err := rows.Scan(&got); err != nil {
			return "", err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if n != 1 {
		return "", fmt.Errorf("%d rows, want 1", n)
	}
	return got, nil
}

// requireCleanOrReplaced is the postcondition of every cancel point: the conn is
// either CLEAN (the same session, and the next statement returns ITS OWN marker,
// with no deadline of the old context on the socket) or CLOSED AND REPLACED (a
// session more, the same marker check). Never a third state.
func requireCleanOrReplaced(t *testing.T, f *markerPG, p *Pool, sessionsBefore int, wantReplaced bool) {
	t.Helper()
	var got string
	var err error
	watchdog(t, 10*time.Second, "the statement after the cancel", func() {
		got, err = marker(context.Background(), p, "next-statement")
	})
	if err != nil {
		t.Fatalf("the statement after the cancelled one failed (a stale deadline, a crossed reply or a conn left half-drained): %v", err)
	}
	if got != "next-statement" {
		t.Fatalf("the next statement returned %q, not its own marker (a crossed reply)", got)
	}
	s := f.sessionCount()
	switch {
	case wantReplaced && s != sessionsBefore+1:
		t.Fatalf("sessions = %d, want %d: the conn was to be discarded and replaced (%v)", s, sessionsBefore+1, f.events())
	case !wantReplaced && s != sessionsBefore:
		t.Fatalf("sessions = %d, want %d: the conn was to be reused (%v)", s, sessionsBefore, f.events())
	}
	c, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.release(c)
	if c.cw.stop != nil {
		t.Fatal("a context watch is still armed on an idle conn")
	}
}

// A cancel at each point of a statement's life, on a direct Pool, by Exec and by
// Query: the call ends with the context's error (or, for the points after the
// statement's end, with a result) and the conn is clean or replaced.
func TestDirectCancelAtEachPointOfAStatement(t *testing.T) {
	for _, stmt := range []string{"exec", "query"} {
		run := func(ctx context.Context, p *Pool, q string) error {
			if stmt == "exec" {
				_, err := p.ExecContext(ctx, q)
				return err
			}
			rows, err := p.QueryContext(ctx, q)
			if err != nil {
				return err
			}
			for rows.Next() {
			}
			err = rows.Err()
			_ = rows.Close()
			return err
		}
		t.Run(stmt+"/before-send", func(t *testing.T) {
			f := startMarkerPG(t)
			p := openMarkerPool(t, f)
			if _, err := marker(context.Background(), p, "warm"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := run(ctx, p, "SELECT slow")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if f.has("query:SELECT slow") {
				t.Fatalf("a statement of a context that had ended was sent: %v", f.events())
			}
			requireCleanOrReplaced(t, f, p, 1, false)
		})
		t.Run(stmt+"/while-the-server-works", func(t *testing.T) {
			f := startMarkerPG(t)
			p := openMarkerPool(t, f)
			if _, err := marker(context.Background(), p, "warm"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(80*time.Millisecond, cancel)
			var err error
			start := time.Now()
			watchdog(t, 5*time.Second, "the cancelled statement", func() { err = run(ctx, p, "SELECT slow") })
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if el := time.Since(start); el < 60*time.Millisecond {
				t.Fatalf("returned after %v, before the cancel", el)
			}
			if !f.has("cancel-request") {
				t.Fatalf("no CancelRequest reached the backend: the statement is still running there (%v)", f.events())
			}
			requireCleanOrReplaced(t, f, p, 1, false)
		})
		t.Run(stmt+"/mid-result", func(t *testing.T) {
			f := startMarkerPG(t)
			p := openMarkerPool(t, f)
			if _, err := marker(context.Background(), p, "warm"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// the backend has sent the first row when it cancels the context
			h := func(string) { time.Sleep(20 * time.Millisecond); cancel() }
			f.afterReply.Store(&h)
			var err error
			watchdog(t, 5*time.Second, "the cancelled statement", func() { err = run(ctx, p, "SELECT drip") })
			f.afterReply.Store(nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			requireCleanOrReplaced(t, f, p, 1, false)
		})
		t.Run(stmt+"/during-the-drain", func(t *testing.T) {
			// The deadline ends the statement; the backend takes 300 ms to answer
			// the cancel; cancel() of the same context at 100 ms, in the middle of
			// the drain, must not touch the drain.
			f := startMarkerPG(t)
			f.drainDelay = 300 * time.Millisecond
			p := openMarkerPool(t, f)
			if _, err := marker(context.Background(), p, "warm"); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			time.AfterFunc(100*time.Millisecond, cancel)
			var err error
			start := time.Now()
			watchdog(t, 5*time.Second, "the cancelled statement", func() { err = run(ctx, p, "SELECT draindelay") })
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want context.DeadlineExceeded", err)
			}
			if el := time.Since(start); el < 250*time.Millisecond {
				t.Fatalf("returned after %v: the drain (300 ms) was cut short", el)
			}
			requireCleanOrReplaced(t, f, p, 1, false)
		})
		t.Run(stmt+"/after-ReadyForQuery", func(t *testing.T) {
			// The backend answers and the context ends right after: the call
			// returns a result or the context's error, and either way the conn is
			// clean for the next statement.
			f := startMarkerPG(t)
			p := openMarkerPool(t, f)
			if _, err := marker(context.Background(), p, "warm"); err != nil {
				t.Fatal(err)
			}
			for i := range 200 {
				ctx, cancel := context.WithCancel(context.Background())
				h := func(q string) {
					if q == fmt.Sprintf("SELECT late%d", i) {
						cancel()
					}
				}
				f.afterReply.Store(&h)
				err := run(ctx, p, fmt.Sprintf("SELECT late%d", i))
				f.afterReply.Store(nil)
				cancel()
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("iteration %d: err = %v", i, err)
				}
				if got, err := marker(context.Background(), p, fmt.Sprintf("n%d", i)); err != nil || got != fmt.Sprintf("n%d", i) {
					t.Fatalf("iteration %d: the next statement returned %q, %v", i, got, err)
				}
			}
			requireCleanOrReplaced(t, f, p, f.sessionCount(), false)
		})
	}
}

// A callback of the context watch held in flight (it sleeps 100 ms before it sets
// the deadline) across the end of the statement: the call does not return before
// the callback has run to its end, and the deadline it set is cleared, so the next
// statement on the conn is not poisoned. The held callback is the one case in
// which stopping the watch fails (the context ended first), so this is the test
// of the wait.
func TestDirectCancelWatchCallbackInFlightAcrossTheStatementsEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		// when the context ends, relative to the statement
		at func(f *markerPG, cancel context.CancelFunc)
	}{
		{"cancel-before-the-reply", func(f *markerPG, cancel context.CancelFunc) {
			h := func(q string) {
				if q == "SELECT held" {
					cancel()
				}
			}
			f.onQuery.Store(&h)
		}},
		{"cancel-after-the-reply", func(f *markerPG, cancel context.CancelFunc) {
			h := func(q string) {
				if q == "SELECT held" {
					cancel()
				}
			}
			f.afterReply.Store(&h)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startMarkerPG(t)
			p := openMarkerPool(t, f)
			if _, err := marker(context.Background(), p, "warm"); err != nil {
				t.Fatal(err)
			}
			var ran atomic.Int64
			hold := func() { ran.Add(1); time.Sleep(100 * time.Millisecond) }
			testHookCtxWatch.Store(&hold)
			t.Cleanup(func() { testHookCtxWatch.Store(nil) })
			// A cancel that lands after the statement ended finds the watch stopped
			// and no callback runs (nothing to wait for): such an attempt is only
			// checked for a clean conn, and the case is tried again until a callback
			// was held in flight (the first attempt, for a cancel before the reply).
			for attempt := 0; ; attempt++ {
				if attempt == 200 {
					t.Fatal("no attempt put the callback in flight across the end of the statement")
				}
				ctx, cancel := context.WithCancel(context.Background())
				tc.at(f, cancel)
				before := ran.Load()
				var err error
				start := time.Now()
				watchdog(t, 5*time.Second, "the statement", func() { _, err = p.ExecContext(ctx, "SELECT held") })
				el := time.Since(start)
				cancel()
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v", err)
				}
				requireCleanOrReplaced(t, f, p, 1, false)
				if ran.Load() == before {
					continue
				}
				if el < 90*time.Millisecond {
					t.Fatalf("the call returned after %v, before the held callback (100 ms) had run to its end: it could run during the next statement", el)
				}
				return
			}
		})
	}
}

// The callback of the watch is in flight (it sleeps 100 ms) when the loop of
// driveDirect first sees the ended context, and the cancel drain begins: the
// context ends 40 ms into a statement whose second packet arrives at 80 ms, so
// the read returns at 80 ms with the callback still asleep until about 140 ms.
// settleCtxWatch at the top of cancelDrainDirect waits for it BEFORE the drain
// sets its own read deadline; a callback that ran after that would replace the
// drain's deadline with "now", the drain would fail and the conn would be
// discarded (sessions would be 2).
func TestDirectCancelWatchCallbackInFlightWhenTheDrainBegins(t *testing.T) {
	f := startMarkerPG(t)
	f.drainDelay = 200 * time.Millisecond // the drain outlasts the callback's remaining sleep
	p := openMarkerPool(t, f)
	if _, err := marker(context.Background(), p, "warm"); err != nil {
		t.Fatal(err)
	}
	var ran atomic.Int64
	hold := func() { ran.Add(1); time.Sleep(100 * time.Millisecond) }
	testHookCtxWatch.Store(&hold)
	t.Cleanup(func() { testHookCtxWatch.Store(nil) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := func(q string) {
		if q == "SELECT drip2" {
			time.AfterFunc(40*time.Millisecond, cancel)
		}
	}
	f.onQuery.Store(&h)
	var err error
	start := time.Now()
	watchdog(t, 5*time.Second, "the cancelled statement", func() { _, err = p.ExecContext(ctx, "SELECT drip2") })
	el := time.Since(start)
	f.onQuery.Store(nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran.Load() != 1 {
		t.Fatalf("the callback ran %d times, want 1", ran.Load())
	}
	if el < 130*time.Millisecond {
		t.Fatalf("returned after %v: the call did not wait for the callback (held until ~140 ms)", el)
	}
	requireCleanOrReplaced(t, f, p, 1, false)
}

// The context ends at the instant the reply arrives, 20,000 times, so that the
// callback of the watch, the read and the end of the statement race. After each
// statement the next one, on a background context, returns ITS OWN marker; the
// conn is clean (reused) or replaced, never poisoned; no statement fails but with
// the context's error. Every callback that ran is a pass through the wait of
// settleCtxWatch (stopping it had failed), and the test requires that a large
// share of the iterations went through it, so that it cannot pass without
// racing what it is about.
func TestDirectStaleCancelNeverPoisonsTheNextStatement(t *testing.T) {
	iters := 20000
	if testing.Short() {
		iters = 2000
	}
	f := startMarkerPG(t)
	p := openMarkerPool(t, f)
	var ran atomic.Int64
	count := func() { ran.Add(1) }
	testHookCtxWatch.Store(&count)
	t.Cleanup(func() { testHookCtxWatch.Store(nil) })
	var cur atomic.Pointer[context.CancelFunc]
	var mode atomic.Int64
	onQuery := func(q string) {
		if mode.Load() == 0 {
			if cf := cur.Load(); cf != nil {
				(*cf)()
			}
		}
	}
	afterReply := func(q string) {
		if mode.Load() == 1 {
			if cf := cur.Load(); cf != nil {
				(*cf)()
			}
		} else if mode.Load() == 2 {
			spin(10 * time.Microsecond)
			if cf := cur.Load(); cf != nil {
				(*cf)()
			}
		}
	}
	f.onQuery.Store(&onQuery)
	f.afterReply.Store(&afterReply)
	var cancelled, succeeded, other int
	var firstOther error
	watchdog(t, 280*time.Second, "the stale-cancel loop", func() {
		for i := range iters {
			mode.Store(int64(i % 3))
			ctx, cancel := context.WithCancel(context.Background())
			cur.Store(&cancel)
			want := fmt.Sprintf("a%d", i)
			got, err := marker(ctx, p, want)
			cancel()
			switch {
			case err == nil && got == want:
				succeeded++
			case errors.Is(err, context.Canceled):
				cancelled++
			default:
				other++
				if firstOther == nil {
					firstOther = fmt.Errorf("iteration %d: got %q, %v", i, got, err)
				}
			}
			cur.Store(nil)
			// the innocent statement
			want = fmt.Sprintf("b%d", i)
			got, err = marker(context.Background(), p, want)
			if err != nil || got != want {
				other++
				if firstOther == nil {
					firstOther = fmt.Errorf("iteration %d: the next statement returned %q, %v (want %q)", i, got, err, want)
				}
				break
			}
		}
	})
	if other > 0 {
		t.Fatalf("%d statements failed or returned a wrong marker; the first: %v", other, firstOther)
	}
	fired := ran.Load()
	t.Logf("%d iterations: %d returned a result, %d the context's error; the callback of the watch ran in %d; sessions %d, cancel requests %d",
		iters, succeeded, cancelled, fired, f.sessionCount(), f.cancelCount())
	if fired < int64(iters)/2 {
		t.Fatalf("the callback ran in %d of %d iterations: the test does not exercise the wait of settleCtxWatch", fired, iters)
	}
}

func spin(d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); {
	}
}

// Nothing is armed for a context that can never end; a context that can end arms
// one watch per statement and leaves none behind. Each case has a pool, and so a
// conn, of its own: the callback is built once per conn.
func TestDirectCtxWatchIsArmedOnlyForAContextThatCanEnd(t *testing.T) {
	type kv struct{}
	for _, tc := range []struct {
		name  string
		mk    func() (context.Context, context.CancelFunc)
		armed bool
	}{
		{"background", func() (context.Context, context.CancelFunc) { return context.Background(), func() {} }, false},
		{"background with a value", func() (context.Context, context.CancelFunc) {
			return context.WithValue(context.Background(), kv{}, 1), func() {}
		}, false},
		{"cancel only", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, true},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Hour)
		}, true},
		{"foreign with a deadline and no Done", func() (context.Context, context.CancelFunc) {
			return &noDoneCtx{dl: time.Now().Add(time.Hour)}, func() {}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startMarkerPG(t)
			p := openMarkerPool(t, f)
			ctx, cancel := tc.mk()
			defer cancel()
			if _, err := marker(ctx, p, "x"); err != nil {
				t.Fatal(err)
			}
			c, err := p.acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer p.release(c)
			if got := c.cw.fn != nil; got != tc.armed {
				t.Fatalf("watch built = %v, want %v", got, tc.armed)
			}
			if c.cw.stop != nil {
				t.Fatal("a watch is still armed after the statement")
			}
		})
	}
}

// noDoneCtx has a deadline and no Done channel: nothing can be registered with it.
type noDoneCtx struct{ dl time.Time }

func (c *noDoneCtx) Deadline() (time.Time, bool) { return c.dl, true }
func (c *noDoneCtx) Done() <-chan struct{}       { return nil }
func (c *noDoneCtx) Err() error                  { return nil }
func (c *noDoneCtx) Value(any) any               { return nil }

// COPY OUT: the context ends while the rows are being sent (the backend waits for
// the cancel after three rows). The call ends with the context's error, the
// backend got a CancelRequest, and the conn is clean for the next statement.
func TestDirectCancelDuringCopyTo(t *testing.T) {
	f := startMarkerPG(t)
	p := openMarkerPool(t, f)
	if _, err := marker(context.Background(), p, "warm"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rows atomic.Int64
	h := func(string) { time.Sleep(30 * time.Millisecond); cancel() }
	f.afterReply.Store(&h)
	var err error
	watchdog(t, 10*time.Second, "CopyTo", func() {
		err = p.CopyTo(ctx, "SELECT copyout", func([]byte) error { rows.Add(1); return nil })
	})
	f.afterReply.Store(nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rows.Load() != 3 {
		t.Fatalf("dest ran %d times, want 3", rows.Load())
	}
	if !f.has("cancel-request") {
		t.Fatalf("no CancelRequest reached the backend: %v", f.events())
	}
	requireCleanOrReplaced(t, f, p, 1, false)
}

// iterSource is a CopyFromSource of n rows, or of rows without end (n < 0), that
// calls onRow(i) before it returns row i.
type iterSource struct {
	n, i  int
	onRow func(i int)
}

func (s *iterSource) Next() bool {
	if s.n >= 0 && s.i >= s.n {
		return false
	}
	s.i++
	if s.onRow != nil {
		s.onRow(s.i)
	}
	return true
}
func (s *iterSource) Values() ([]any, error) { return []any{int64(s.i)}, nil }
func (s *iterSource) Err() error             { return nil }

// COPY IN: the context ends while the rows are being sent. A source without end
// must not keep the call running after the context ended: the call ends with the
// context's error, and the conn is clean or replaced.
func TestDirectCancelDuringCopyFrom(t *testing.T) {
	f := startMarkerPG(t)
	p := openMarkerPool(t, f)
	if _, err := marker(context.Background(), p, "warm"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &iterSource{n: -1, onRow: func(i int) {
		if i == 50 {
			cancel()
		}
		if i > 5_000_000 {
			panic("the source was asked for rows long after the context ended")
		}
	}}
	var err error
	watchdog(t, 20*time.Second, "CopyFrom", func() { _, err = p.CopyFrom(ctx, "t", nil, src) })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if src.i > 1000 {
		t.Fatalf("the source produced %d rows: the call kept streaming long after the context ended", src.i)
	}
	requireCleanOrReplaced(t, f, p, f.sessionCount(), false)
}

// COPY IN, the context ends before the rows start: the backend has sent
// CopyInResponse and waits.
func TestDirectCancelBeforeTheRowsOfCopyFrom(t *testing.T) {
	f := startMarkerPG(t)
	p := openMarkerPool(t, f)
	if _, err := marker(context.Background(), p, "warm"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var err error
	// the context ends 20 ms after CopyInResponse; the source needs 100 ms for its
	// ten rows, so the per-row check sees the end before the rows are all sent
	h := func(string) { time.AfterFunc(20*time.Millisecond, cancel) }
	f.afterReply.Store(&h)
	watchdog(t, 20*time.Second, "CopyFrom", func() {
		src := &iterSource{n: 10, onRow: func(int) { time.Sleep(10 * time.Millisecond) }}
		_, err = p.CopyFrom(ctx, "t", nil, src)
	})
	f.afterReply.Store(nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	requireCleanOrReplaced(t, f, p, f.sessionCount(), false)
}

// A cancel exchange that is not confirmed replaces the conn: the cancel socket
// stays open, the exchange times out, the conn is failed and the next statement
// runs on a session of its own.
func TestDirectCancelWithoutConfirmationReplacesTheConn(t *testing.T) {
	old := cancelExchangeTimeout
	cancelExchangeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { cancelExchangeTimeout = old })
	f := startMarkerPG(t)
	f.cancelHold = 10 * time.Second
	p := openMarkerPool(t, f)
	if _, err := marker(context.Background(), p, "warm"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	var err error
	watchdog(t, 5*time.Second, "the cancelled statement", func() { _, err = p.ExecContext(ctx, "SELECT slow") })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrBadConn) {
		t.Fatalf("err = %v, want the context's error and not ErrBadConn", err)
	}
	requireCleanOrReplaced(t, f, p, 1, true)
}

// A COPY whose request is done wins over a context that has ended: the wait
// returns the request's result and sends no CancelRequest for a statement that is
// over. (select picks at random when both are ready.)
func TestDirectAwaitPrefersACompletedRequestOverAnEndedContext(t *testing.T) {
	f := startMarkerPG(t)
	p := openMarkerPool(t, f)
	c, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer p.release(c)
	for i := range 200 {
		req := &pgRequest{doneCh: make(chan struct{})}
		close(req.doneCh)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.awaitDirectWithCancel(ctx, req); err != nil {
			t.Fatalf("iteration %d: err = %v, want the (nil) result of the completed request", i, err)
		}
	}
	if n := f.cancelCount(); n != 0 {
		t.Fatalf("%d CancelRequests were sent for statements that were over", n)
	}
}
