package postgres

// celeris#887 item 20: failAll closes a streaming request's rowCh, but
// dispatch's OnRow can still be sending on it from another goroutine (a Close
// racing a delivery; on the portable worker also a Loop.Close). A send on a
// closed channel panics, and nothing recovers it on the delivery path, so the
// process dies. failAll's recover guard covers only a second close. The
// failure is "panic: send on closed channel", so a fixed tree must not panic
// in either order of the two calls.

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// c887Stream is a pgConn with no socket and one streaming request queued on
// it, the shape promoteToStreaming leaves: rowCh with room for capacity rows.
func c887Stream(capacity int) (*pgConn, *pgRequest) {
	c := &pgConn{bridge: async.NewBridge()}
	req := &pgRequest{
		ctx:    context.Background(),
		kind:   reqSimple,
		rowCh:  make(chan [][]byte, capacity),
		colsCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	c.enqueue(req)
	return c, req
}

// c887OnRow calls req.OnRow once on its own goroutine and reports whether it
// panicked (the value) and, on done, that it returned.
func c887OnRow(req *pgRequest, row [][]byte) (panicked chan any) {
	panicked = make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		req.OnRow(row)
	}()
	return panicked
}

func TestFailAllWhileOnRowIsBlockedOnAFullRowCh887(t *testing.T) {
	c, req := c887Stream(1)
	row := [][]byte{[]byte("a")}
	req.OnRow(row) // fills the channel
	panicked := c887OnRow(req, row)
	select {
	case p := <-panicked:
		t.Fatalf("the second OnRow returned (%v) before failAll: it was not blocked on the full channel", p)
	case <-time.After(100 * time.Millisecond): // OnRow is blocked in its send
	}

	boom := errors.New("conn closed")
	failed := make(chan struct{})
	go func() { c.failAll(boom); close(failed) }()
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("failAll did not return within 5 s: it waits for an OnRow that nothing releases")
	}

	select {
	case p := <-panicked:
		if p != nil {
			t.Fatalf("OnRow, blocked on a full rowCh while failAll ran, panicked: %v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnRow stayed blocked after failAll: the consumer is released but the producer is not")
	}
	// The consumer sees what was sent before the close, then the close and the error.
	got := 0
	for range req.rowCh {
		got++
	}
	if got < 1 || got > 2 {
		t.Errorf("the consumer read %d rows, want the 1 or 2 that were queued", got)
	}
	if ep := req.streamErr.Load(); ep == nil || !errors.Is(*ep, boom) {
		t.Errorf("streamErr = %v, want the failAll error", ep)
	}
	select {
	case <-req.doneCh:
	default:
		t.Error("failAll did not signal the request done")
	}
}

func TestOnRowAfterFailAllDropsTheRowAndDoesNotPanic887(t *testing.T) {
	c, req := c887Stream(4)
	c.failAll(errors.New("conn closed"))
	if p := <-c887OnRow(req, [][]byte{[]byte("late")}); p != nil {
		t.Fatalf("OnRow after failAll panicked: %v", p)
	}
	if _, ok := <-req.rowCh; ok {
		t.Error("a row was delivered after failAll closed rowCh")
	}
}

// failAll and the dispatch-side closes (a request that finishes, an error)
// can both run for one request: the second close must stay quiet. The four
// sites of dispatch that close rowCh are driven through dispatch itself.
func TestFailAllAfterDispatchClosedTheStream887(t *testing.T) {
	errorResponse := []byte("SERROR\x00C42000\x00Mboom\x00\x00")
	for _, tc := range []struct {
		name string
		kind requestKind
		ext  bool
		msgs []struct {
			typ     byte
			payload []byte
		}
		wantErr bool
	}{
		{"simple done", reqSimple, false, []struct {
			typ     byte
			payload []byte
		}{{protocol.BackendCommandComplete, []byte("SELECT 0\x00")}, {protocol.BackendReadyForQuery, []byte{'I'}}}, false},
		{"simple error", reqSimple, false, []struct {
			typ     byte
			payload []byte
		}{{protocol.BackendErrorResponse, errorResponse}, {protocol.BackendReadyForQuery, []byte{'I'}}}, true},
		{"extended done", reqExtended, true, []struct {
			typ     byte
			payload []byte
		}{{protocol.BackendBindComplete, nil}, {protocol.BackendCommandComplete, []byte("SELECT 0\x00")}, {protocol.BackendReadyForQuery, []byte{'I'}}}, false},
		{"extended error", reqExtended, true, []struct {
			typ     byte
			payload []byte
		}{{protocol.BackendErrorResponse, errorResponse}, {protocol.BackendReadyForQuery, []byte{'I'}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, req := c887Stream(4)
			req.kind = tc.kind
			if tc.ext {
				req.extended.SkipParse = true
			}
			req.rowCh <- [][]byte{[]byte("a")}
			for _, m := range tc.msgs {
				if err := c.dispatch(m.typ, m.payload); err != nil {
					t.Fatalf("dispatch(%q): %v", m.typ, err)
				}
			}
			select {
			case <-req.doneCh:
			default:
				t.Fatal("dispatch did not finish the request: the test did not drive the close site")
			}
			if (req.err != nil) != tc.wantErr {
				t.Fatalf("request error = %v, want an error: %v", req.err, tc.wantErr)
			}
			// rowCh is closed by dispatch now: reading it ends.
			rows := 0
			for range req.rowCh {
				rows++
			}
			if rows != 1 {
				t.Fatalf("read %d rows, want the 1 sent", rows)
			}
			// failAll takes its queue before dispatch pops the head, so it can
			// hold a request dispatch has just finished: put it back.
			c.pendingMu.Lock()
			c.pending = append(c.pending, req)
			c.pendingMu.Unlock()
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("failAll after the stream ended panicked: %v", p)
					}
				}()
				c.failAll(errors.New("first"))
				c.failAll(errors.New("second"))
			}()
		})
	}
}

// Many rounds of failAll against producers that keep sending into a consumer
// that keeps up, so most sends do not block and are in flight when the close
// comes: no round may panic, and every round must end with rowCh closed. Run
// under -race it also checks the guarded close against the send.
func TestFailAllRacingProducersNeverPanics887(t *testing.T) {
	const rounds, producers = 1500, 3
	for r := 0; r < rounds; r++ {
		c, req := c887Stream(2)
		var stop atomic.Bool
		var wg sync.WaitGroup
		panics := make(chan any, producers)
		for p := 0; p < producers; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if v := recover(); v != nil {
						panics <- v
					}
				}()
				row := [][]byte{[]byte("x")}
				for !stop.Load() {
					req.OnRow(row)
				}
			}()
		}
		consumed := make(chan struct{})
		go func() {
			defer close(consumed)
			for range req.rowCh { //revive:disable-line:empty-block drain until closed
			}
		}()
		time.Sleep(time.Duration(r%6) * 10 * time.Microsecond)
		failed := make(chan struct{})
		go func() { c.failAll(errors.New("conn closed")); close(failed) }()
		select {
		case <-failed:
		case <-time.After(5 * time.Second):
			stop.Store(true)
			t.Fatalf("round %d: failAll did not return within 5 s", r)
		}
		stop.Store(true)
		wg.Wait()
		select {
		case v := <-panics:
			t.Fatalf("round %d: OnRow panicked while failAll ran: %v", r, v)
		default:
		}
		select {
		case <-consumed: // rowCh was closed
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: rowCh was not closed", r)
		}
	}
}

// failAll can run before dispatch has promoted a buffered request to
// streaming (promoteToStreaming publishes rowCh later, on the delivery
// goroutine). The error must be recorded anyway, so that a consumer that sees
// rowCh closed does not read a cut-off result as complete, and the promotion
// that follows must not publish a channel nothing will close.
func TestFailAllBeforePromotionKeepsTheErrorAndNothingToClose887(t *testing.T) {
	c := &pgConn{bridge: async.NewBridge()}
	req := &pgRequest{
		ctx:    context.Background(),
		kind:   reqSimple,
		colsCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	c.enqueue(req)
	req.OnRow([][]byte{[]byte("buffered")}) // buffered mode: no rowCh yet

	boom := errors.New("conn closed")
	c.failAll(boom)
	if ep := req.streamErr.Load(); ep == nil || !errors.Is(*ep, boom) {
		t.Errorf("streamErr = %v after failAll on a request that had not promoted, want the failAll error", ep)
	}
	promoteToStreaming(req) // the delivery goroutine, still running, reaches the threshold
	if req.rowCh != nil {
		t.Error("promoteToStreaming published a rowCh after failAll: nothing will close it")
	}
	// colsCh closed means "rowCh is published": a caller woken on it builds a
	// streamRows over rowCh, and a nil rowCh blocks Next and Close for good.
	select {
	case <-req.colsCh:
		t.Error("colsCh is closed after failAll and a declined promotion: a caller woken on it reads a nil rowCh")
	default:
	}
}

// A conn that dies before the request promoted to streaming must wake the
// caller parked in waitForQueryRows with the conn's error. Before this, failAll
// closed colsCh, the caller built a streamRows over the nil rowCh, and both
// Next and Close blocked for good (review r1 of #1002; main hangs the same).
func TestFailAllBeforePromotionWakesTheCallerWithTheError887(t *testing.T) {
	const rounds = 50
	boom := errors.New("conn closed")
	for i := 0; i < rounds; i++ {
		c := &pgConn{bridge: async.NewBridge()}
		req := acquirePgRequest()
		req.ctx = context.Background()
		req.kind = reqSimple
		req.colsCh = make(chan struct{})
		c.enqueue(req)
		type res struct {
			rows driver.Rows
			err  error
		}
		got := make(chan res, 1)
		go func() {
			r, err := c.waitForQueryRows(context.Background(), req, true)
			got <- res{r, err}
		}()
		time.Sleep(2 * time.Millisecond) // the caller is parked in the select
		c.failAll(boom)
		var r res
		select {
		case r = <-got:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: waitForQueryRows did not return after failAll", i)
		}
		if r.rows != nil {
			// What the old code returned: a streamRows over a nil rowCh.
			done := make(chan error, 1)
			go func() { done <- r.rows.Next(make([]driver.Value, 1)) }()
			select {
			case <-done:
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("round %d: waitForQueryRows returned rows whose Next blocks for good", i)
			}
		}
		if !errors.Is(r.err, boom) {
			t.Fatalf("round %d: waitForQueryRows error = %v, want the failAll error", i, r.err)
		}
	}
}

// The caller woken by failAll reads req.rows and req.columns while the
// delivery goroutine, still inside dispatch, reaches the streaming threshold
// and calls promoteToStreaming. A declined promotion must leave the request
// alone: under -race a write to req.rows there is a data race with the caller.
func TestDeclinedPromotionDoesNotTouchTheRequestTheCallerReads887(t *testing.T) {
	for _, kind := range []requestKind{reqSimple, reqExtended} {
		t.Run(map[requestKind]string{reqSimple: "simple", reqExtended: "extended"}[kind], func(t *testing.T) {
			declinedPromotionRounds887(t, kind)
		})
	}
}

func declinedPromotionRounds887(t *testing.T, kind requestKind) {
	const rounds = 300
	boom := errors.New("conn closed")
	for i := 0; i < rounds; i++ {
		c := &pgConn{bridge: async.NewBridge()}
		req := acquirePgRequest()
		req.ctx = context.Background()
		req.kind = kind
		req.colsCh = make(chan struct{})
		if kind == reqExtended {
			// The describe result promoteToStreaming would copy to req.columns.
			req.extended.Columns = []protocol.ColumnDesc{{Name: "a"}}
		}
		c.enqueue(req)
		for j := 0; j < 3; j++ {
			req.OnRow([][]byte{[]byte("buffered")}) // below the threshold: stays buffered
		}
		var wg sync.WaitGroup
		var rows driver.Rows
		var werr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, werr = c.waitForQueryRows(context.Background(), req, true)
		}()
		time.Sleep(time.Millisecond)
		c.failAll(boom)
		wg.Add(1)
		go func() { defer wg.Done(); promoteToStreaming(req) }()
		wg.Wait()
		if werr == nil && rows == nil {
			t.Fatalf("round %d: waitForQueryRows returned neither rows nor an error", i)
		}
		if rows != nil {
			closed := make(chan struct{})
			go func() { _ = rows.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatalf("round %d: rows.Close blocks for good", i)
			}
		}
	}
}
