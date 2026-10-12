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
	"errors"
	"sync"
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
	c.failAll(boom)

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

// Many rounds of failAll against a producer that keeps sending, with the
// consumer slow: no round may panic, and every round must end with rowCh
// closed. Run under -race it also checks the guarded close against the send.
func TestFailAllRacingAProducerNeverPanics887(t *testing.T) {
	const rounds = 300
	for r := 0; r < rounds; r++ {
		c, req := c887Stream(2)
		var wg sync.WaitGroup
		panics := make(chan any, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					select {
					case panics <- p:
					default:
					}
				}
			}()
			for i := 0; i < 64; i++ {
				req.OnRow([][]byte{[]byte("x")})
			}
		}()
		time.Sleep(time.Duration(r%5) * 20 * time.Microsecond)
		c.failAll(errors.New("conn closed"))
		wg.Wait()
		select {
		case p := <-panics:
			t.Fatalf("round %d: OnRow panicked while failAll ran: %v", r, p)
		default:
		}
		for range req.rowCh { //revive:disable-line:empty-block drain until closed
		}
	}
}
