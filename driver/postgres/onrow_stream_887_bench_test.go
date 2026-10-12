package postgres

// celeris#887 item 20 moved the close of a streaming request's rowCh behind a
// lock that OnRow holds across its send. These benchmarks run the same code on
// main and on the fix, so benchstat compares the two directly (the A/B and its
// A/A floor are in the PR that made the change):
//
//	OnRowStreaming887   one streamed row per op: copyRow, the send, a consumer that keeps up
//	OnRowStreamSolo887  the same row with no consumer goroutine: the channel is drained by the
//	                    benchmark itself every 4096 rows, so the figure is the producer's cost alone
//	OnRowBuffered887    one buffered row per op: the path the change does not touch
//	StreamLifecycle887  one request per op: 16 streamed rows, then dispatch's done close

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

func BenchmarkOnRowStreaming887(b *testing.B) {
	req := &pgRequest{rowCh: make(chan [][]byte, 64), doneCh: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range req.rowCh { //revive:disable-line:empty-block drain until closed
		}
	}()
	row := [][]byte{[]byte("0123456789"), []byte("abcdefghij")}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req.OnRow(row)
	}
	b.StopTimer()
	close(req.rowCh)
	<-done
}

func BenchmarkOnRowBuffered887(b *testing.B) {
	req := &pgRequest{doneCh: make(chan struct{})}
	row := [][]byte{[]byte("0123456789"), []byte("abcdefghij")}
	b.ReportAllocs()
	n := 0
	for b.Loop() {
		req.OnRow(row)
		if n++; n == 1024 { // keep the slab bounded
			n = 0
			req.rows, req.rowFields, req.rowSlab = req.rows[:0], req.rowFields[:0], req.rowSlab[:0]
		}
	}
}

func BenchmarkStreamLifecycle887(b *testing.B) {
	c := &pgConn{bridge: async.NewBridge()}
	row := [][]byte{[]byte("0123456789"), []byte("abcdefghij")}
	b.ReportAllocs()
	for b.Loop() {
		req := &pgRequest{
			ctx:    context.Background(),
			kind:   reqSimple,
			rowCh:  make(chan [][]byte, 16),
			colsCh: make(chan struct{}),
			doneCh: make(chan struct{}),
		}
		c.enqueue(req)
		for i := 0; i < 16; i++ {
			req.OnRow(row)
		}
		if err := c.dispatch(protocol.BackendCommandComplete, []byte("SELECT 16\x00")); err != nil {
			b.Fatal(err)
		}
		if err := c.dispatch(protocol.BackendReadyForQuery, []byte{'I'}); err != nil {
			b.Fatal(err)
		}
		for range req.rowCh { //revive:disable-line:empty-block drain until closed
		}
	}
}

func BenchmarkOnRowStreamSolo887(b *testing.B) {
	const capacity = 4096
	req := &pgRequest{rowCh: make(chan [][]byte, capacity), doneCh: make(chan struct{})}
	row := [][]byte{[]byte("0123456789"), []byte("abcdefghij")}
	b.ReportAllocs()
	n := 0
	for b.Loop() {
		req.OnRow(row)
		if n++; n == capacity-1 {
			for ; n > 0; n-- {
				<-req.rowCh
			}
		}
	}
}
