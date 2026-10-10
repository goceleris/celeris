//go:build linux

package iouring

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkAcceptCloseChurn959 is the accept-heavy end-to-end measure of
// celeris#959's lock: connections are dialled and closed by the client, in
// parallel, and the engine's workers accept them (onAcceptedFD: the slot
// install) and close them on EOF (finishClose: the slot clear). One op is one
// connection; the clock runs until the engine has counted every close. The
// kernel's connect and close dominate each op, so the figure is read against
// an A/A floor (scripts/timing.sh). It compiles against the engine before and
// after the change, so one benchmark file is the instrument for both arms.
//
// The spin arm adds the reader the lock exists for at its worst: a driver
// goroutine per worker that calls RegisterConn in a loop on a number no
// connection ever has (4000: the lowest-free-number policy keeps the churn far
// below it), so every call reads a slot of the same table, and, after the
// change, takes the same lock the worker takes per accept and per close.
func BenchmarkAcceptCloseChurn959(b *testing.B) {
	// One engine for both arms and for every run of the b.N ramp: a ring that
	// was just closed still holds its memlock charge for a few milliseconds
	// and starting engines back to back can fail with ENOMEM.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:      addr,
		Engine:    engine.IOUring,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: 2},
	}, testHandler{})
	if err != nil {
		b.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			b.Error("engine did not stop within 10s")
		}
	}()
	for deadline := time.Now().Add(15 * time.Second); ; {
		select {
		case err := <-done:
			b.Fatalf("Listen returned: %v", err)
		default:
		}
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			if e.NumWorkers() > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			b.Fatal("engine did not start listening within 15s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Warm the engine: the first connections after an idle ring wait for the
	// worker's idle wake (up to a second each), which the b.N ramp would
	// otherwise fold into the first, small runs.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		before := e.Metrics().CloseCount
		for range 4000 {
			if c, err := net.Dial("tcp", addr); err == nil {
				_ = c.Close()
			}
		}
		for e.Metrics().CloseCount-before < 4000 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if e.Metrics().CloseCount-before >= 4000 {
			break
		}
	}
	b.Run("plain", func(b *testing.B) { benchAcceptCloseChurn959(b, e, addr, false) })
	b.Run("spin", func(b *testing.B) { benchAcceptCloseChurn959(b, e, addr, true) })
}

func benchAcceptCloseChurn959(b *testing.B, e *Engine, addr string, spin bool) {
	stopSpin := make(chan struct{})
	var spinWG sync.WaitGroup
	if spin {
		for i := 0; i < e.NumWorkers(); i++ {
			wl := e.WorkerLoop(i)
			spinWG.Add(1)
			go func() {
				defer spinWG.Done()
				for {
					select {
					case <-stopSpin:
						return
					default:
					}
					_ = wl.RegisterConn(4000, nil, nil) // EBADF after the slot read
				}
			}()
		}
	}
	defer func() { close(stopSpin); spinWG.Wait() }()

	base := e.Metrics().CloseCount
	b.ReportAllocs()
	b.ResetTimer()
	var wg sync.WaitGroup
	next := make(chan struct{}, 256)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range next {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					b.Errorf("dial: %v", err)
					return
				}
				_ = c.Close()
			}
		}()
	}
	for range b.N {
		next <- struct{}{}
	}
	close(next)
	wg.Wait()
	deadline := time.Now().Add(60 * time.Second)
	for e.Metrics().CloseCount-base < uint64(b.N) {
		if time.Now().After(deadline) {
			b.Fatalf("the engine closed %d of %d connections", e.Metrics().CloseCount-base, b.N)
		}
		time.Sleep(50 * time.Microsecond)
	}
	b.StopTimer()
}
