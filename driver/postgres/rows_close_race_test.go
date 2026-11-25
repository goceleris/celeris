package postgres

import (
	"database/sql/driver"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Close on one goroutine while another is in Next or Scan of the same Rows
// (a watchdog that closes the rows on a timeout, as database/sql supports).
// Close recycles the pooled pgRows and releases the conn: a Next that is past its
// closed check must not touch them after that, or it advances the cursor of the
// next query that got the same pgRows.

// blockingRows is a driver.Rows whose Next blocks until released, and which notes
// a Close that runs while a Next is in flight.
type blockingRows struct {
	entered, release chan struct{}
	inNext           atomic.Bool
	closedInNext     atomic.Bool
	closed           chan struct{}
}

func (b *blockingRows) Columns() []string { return []string{"n"} }
func (b *blockingRows) Close() error {
	if b.inNext.Load() {
		b.closedInNext.Store(true)
	}
	close(b.closed)
	return nil
}
func (b *blockingRows) Next(dest []driver.Value) error {
	b.inNext.Store(true)
	close(b.entered)
	<-b.release
	b.inNext.Store(false)
	dest[0] = int64(1)
	return nil
}

var _ driver.Rows = (*blockingRows)(nil)

// TestRowsCloseWaitsForNextInFlight is deterministic: while a Next is inside the
// inner rows, Close must not release anything (database/sql holds closemu for read
// in Next and for write in Close).
func TestRowsCloseWaitsForNextInFlight(t *testing.T) {
	b := &blockingRows{entered: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	rows := acquireRows(b, nil, nil)
	nextDone := make(chan bool, 1)
	go func() { nextDone <- rows.Next() }()
	<-b.entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- rows.Close() }()
	select {
	case <-b.closed:
		t.Error("Close released the inner rows while a Next was still in it")
	case <-time.After(150 * time.Millisecond):
	}
	close(b.release)
	<-nextDone
	if err := <-closeDone; err != nil {
		t.Errorf("Close = %v", err)
	}
	if b.closedInNext.Load() {
		t.Error("the inner rows were closed during Next")
	}
	if rows.Next() {
		t.Error("Next after Close = true")
	}
	if cols := rows.Columns(); cols != nil {
		t.Errorf("Columns after Close = %v", cols)
	}
}

// TestRowsCloseRacesNext is the free-running form: one goroutine closes a Rows
// while another reads it, workers query markers of their own meanwhile. It cannot
// see the window without -race (a few instructions wide), where the detector
// reports the unsynchronized fields; without the fix it fails there.
func TestRowsCloseRacesNext(t *testing.T) {
	iters := 4000
	if testing.Short() {
		iters = 500
	}
	p := openEchoPool(t, 4)
	ctx := tctx(t)
	var foreign, stolen, panics atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r, err := p.QueryContext(ctx, "SELECT 9")
				if err != nil {
					continue
				}
				runtime.Gosched()
				var v int64
				if !r.Next() {
					if r.Err() == nil {
						stolen.Add(1)
					}
				} else if err := r.Scan(&v); err == nil && v != 9 {
					foreign.Add(1)
				}
				_ = r.Close()
			}
		}()
	}
	for i := 0; i < iters; i++ {
		r, err := p.QueryContext(ctx, "SELECT 7")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = r.Close(); close(done) }()
		func() {
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()
			var v int64
			if r.Next() {
				if err := r.Scan(&v); err == nil && v != 7 {
					foreign.Add(1)
				}
			}
			_ = r.Columns()
		}()
		<-done
	}
	close(stop)
	wg.Wait()
	t.Logf("foreign=%d stolen=%d panics=%d stats=%+v", foreign.Load(), stolen.Load(), panics.Load(), p.Stats())
	if foreign.Load() != 0 || stolen.Load() != 0 || panics.Load() != 0 {
		t.Errorf("a Close racing Next/Scan/Columns reached another query: foreign=%d stolen=%d panics=%d", foreign.Load(), stolen.Load(), panics.Load())
	}
	if st := p.Stats(); st.InUse != 0 || st.Idle != st.Open {
		t.Errorf("pool after the run: %+v", st)
	}
}
