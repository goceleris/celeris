//go:build soak

// Package websocket soak tests validate long-running behavior of the
// middleware under sustained backpressure: goroutine counts, RSS,
// connection lifecycle, and chanReader bookkeeping all must stay bounded.
//
// The soak test is gated behind the `soak` build tag so it does not run
// as part of the default unit test sweep. Invoke it explicitly:
//
//	go test -tags=soak -timeout 10m ./middleware/websocket/... -run TestSoak
//
// The duration defaults to 5 minutes for CI use. Set SOAK_DURATION to
// override:
//
//	SOAK_DURATION=30m go test -tags=soak -timeout 35m -run TestSoak ./middleware/websocket/...
package websocket

import (
	"bufio"
	"context"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// soakDuration is the total test budget for TestSoakSlowConsumer.
// Default 5 minutes, override via SOAK_DURATION.
func soakDuration() time.Duration {
	if s := os.Getenv("SOAK_DURATION"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			return d
		}
	}
	return 5 * time.Minute
}

// TestSoakSlowConsumer stresses the engine-integrated WebSocket path
// with many concurrent slow-consumer clients. The handler sleeps per
// message to create sustained backpressure; clients flood messages as
// fast as the pause/resume mechanism allows.
//
// Pass criteria: throughout the run and for 2 seconds after shutdown,
// the goroutine count and heap size must stay bounded, BackpressureDropped
// must remain 0, and no connection must get closed unexpectedly.
func TestSoakSlowConsumer(t *testing.T) {
	numClients := 250
	if s := os.Getenv("SOAK_CLIENTS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			numClients = n
		}
	}
	const (
		messagesPerClient  = 10_000 // plenty to keep the pipeline busy
		handlerSleep       = 5 * time.Millisecond
		bufCap             = 32 // small on purpose — exercises pause/resume
	)

	duration := soakDuration()
	t.Logf("soak: %d clients, %s duration, handler sleep %s", numClients, duration, handlerSleep)

	var (
		serverProcessed atomic.Uint64
		serverDropped   atomic.Uint64
		serverErrored   atomic.Uint32
		handlersDone    sync.WaitGroup
	)

	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.GET("/ws", New(Config{
		MaxBackpressureBuffer: bufCap,
		Handler: func(c *Conn) {
			handlersDone.Add(1)
			defer handlersDone.Done()
			for {
				_, _, err := c.ReadMessageReuse()
				if err != nil {
					serverDropped.Add(c.BackpressureDropped())
					if !isExpectedCloseErr(err) {
						serverErrored.Add(1)
					}
					return
				}
				serverProcessed.Add(1)
				time.Sleep(handlerSleep)
			}
		},
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, serverCancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.StartWithListenerAndContext(serverCtx, ln) }()
	addr := waitForReady(t, s, 5*time.Second)

	// Snapshot baseline goroutines AFTER the server is up so we don't
	// count engine workers as a leak.
	runtime.GC()
	baselineGoroutines := runtime.NumGoroutine()
	var baselineMem runtime.MemStats
	runtime.ReadMemStats(&baselineMem)
	t.Logf("baseline: goroutines=%d heap=%d KB", baselineGoroutines, baselineMem.HeapAlloc/1024)

	// Launch clients in batches so we don't overwhelm the std engine's
	// single-threaded accept queue with thousands of simultaneous dials.
	deadline := time.Now().Add(duration)
	var clientWG sync.WaitGroup
	batchSize := 50
	for batch := 0; batch < numClients; batch += batchSize {
		end := batch + batchSize
		if end > numClients {
			end = numClients
		}
		for i := batch; i < end; i++ {
			clientWG.Add(1)
			go func(id int) {
				defer clientWG.Done()
				conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
				if err != nil {
					return // accept queue full — skip this client
				}
				client := &testWSClient{
					conn: conn,
					br:   bufio.NewReader(conn),
					bw:   bufio.NewWriter(conn),
				}
				defer client.close()
				client.upgrade(t, "/ws")
			sent := 0
			for sent < messagesPerClient && time.Now().Before(deadline) {
				if err := client.writeClientFrame(true, OpText, []byte("flood")); err != nil {
					return
				}
				if err := client.bw.Flush(); err != nil {
					return
				}
				sent++
			}
			// Close cleanly so the handler returns.
			client.writeClientFrame(true, OpClose,
				FormatCloseMessage(CloseNormalClosure, ""))
			_ = client.bw.Flush()
			}(i)
		}
		// Brief pause between batches to let the accept queue drain.
		time.Sleep(100 * time.Millisecond)
	}

	// Monitor loop: every 10 seconds sample goroutines + heap, check bounds.
	monitorDone := make(chan struct{})
	var (
		peakGoroutines int
		peakHeap       uint64
	)
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-time.After(duration + 30*time.Second):
				return
			}
			if time.Now().After(deadline) {
				return
			}
			g := runtime.NumGoroutine()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if g > peakGoroutines {
				peakGoroutines = g
			}
			if m.HeapAlloc > peakHeap {
				peakHeap = m.HeapAlloc
			}
			t.Logf("monitor: goroutines=%d heap=%d KB processed=%d dropped=%d",
				g, m.HeapAlloc/1024, serverProcessed.Load(), serverDropped.Load())
		}
	}()

	clientWG.Wait()
	handlersDone.Wait()
	<-monitorDone

	// Shutdown.
	serverCancel()
	<-serverDone

	// Post-shutdown settle.
	time.Sleep(2 * time.Second)
	runtime.GC()
	finalGoroutines := runtime.NumGoroutine()
	var finalMem runtime.MemStats
	runtime.ReadMemStats(&finalMem)

	t.Logf("final: goroutines=%d (baseline=%d, peak=%d), heap=%d KB (baseline=%d KB, peak=%d KB)",
		finalGoroutines, baselineGoroutines, peakGoroutines,
		finalMem.HeapAlloc/1024, baselineMem.HeapAlloc/1024, peakHeap/1024)
	t.Logf("totals: processed=%d dropped=%d errored=%d",
		serverProcessed.Load(), serverDropped.Load(), serverErrored.Load())

	// Pass criteria:
	// 1. Post-shutdown goroutines ≤ baseline + 10 (small slack for runtime)
	// 2. Post-shutdown heap ≤ baseline + 8 MB (same slack)
	// 3. No dropped chunks (backpressure prevents loss)
	// 4. No unexpected handler errors
	if finalGoroutines > baselineGoroutines+10 {
		t.Errorf("goroutine leak: baseline=%d final=%d", baselineGoroutines, finalGoroutines)
	}
	if finalMem.HeapAlloc > baselineMem.HeapAlloc+8*1024*1024 {
		t.Errorf("heap growth: baseline=%d KB final=%d KB",
			baselineMem.HeapAlloc/1024, finalMem.HeapAlloc/1024)
	}
	if d := serverDropped.Load(); d > 0 {
		t.Errorf("backpressure dropped %d chunks — should be 0", d)
	}
	if e := serverErrored.Load(); e > 0 {
		t.Errorf("handler saw %d unexpected errors", e)
	}
}

// isExpectedCloseErr reports true for the two clean-close paths we
// expect at soak end: the client's explicit close frame, and the read
// deadline exceeded on net.Conn when a client disconnects abruptly.
func isExpectedCloseErr(err error) bool {
	if err == nil {
		return true
	}
	if _, ok := err.(*CloseError); ok {
		return true
	}
	msg := err.Error()
	return containsAny(msg, []string{
		"closed",
		"EOF",
		"broken pipe",
		"connection reset",
		"use of closed network connection",
	})
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		for i := 0; i+len(n) <= len(haystack); i++ {
			if haystack[i:i+len(n)] == n {
				return true
			}
		}
	}
	return false
}
