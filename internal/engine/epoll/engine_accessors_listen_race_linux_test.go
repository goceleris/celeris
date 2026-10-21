//go:build linux

package epoll

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// Engine.Listen publishes the loop slice (e.loops) under e.mu, and the
// engine's exported accessors can be called from any goroutine, including one
// started before the server is up (a monitoring goroutine polling Metrics, the
// adaptive engine adopting or starting a transplant). Metrics, AdoptConn,
// StartTransplant and StopTransplant used to read e.loops without e.mu, a data
// race with Listen that probatorium's -race tier reported in all 16 instrumented
// runs (120 reports of the pair Listen / Metrics). io_uring has had the same
// test since celeris#578 (metrics_listen_race_test.go); epoll never did.
//
// Each case below hammers one accessor from a goroutine that starts BEFORE
// Listen and keeps going after the loops are published, over several engines
// started and stopped in turn. The race detector needs no overlap in time, only
// that the read and the write are not ordered by a lock; the assertion is the
// detector's own, so these tests prove nothing without -race.

// stubTarget is a TransplantTarget that is never given a connection: no
// connection is established, so no loop reaches the hand-off.
type stubTarget struct{}

func (stubTarget) AdoptConn(int, engine.Carryover) error { return nil }

// listenRaceCycles runs iterations of: build an engine, start the reader
// returned by mk (it runs from before Listen until after Addr is published
// plus a short tail), stop everything and join it.
func listenRaceCycles(t *testing.T, iterations int, mk func(t *testing.T, e *Engine) (read func(), done func())) {
	t.Helper()
	for i := range iterations {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pick port: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()

		e, err := New(resource.Config{
			Addr:      addr,
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: 2},
		}, &reqCountHandler{})
		if err != nil {
			t.Skipf("epoll engine unavailable: %v", err)
		}
		read, done := mk(t, e)

		// started closes after the reader's FIRST call, and Listen waits
		// on it: otherwise Listen could publish the loops before the reader
		// was ever scheduled and the iteration would prove nothing.
		var stop atomic.Bool
		var once sync.Once
		started := make(chan struct{})
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			for !stop.Load() {
				read()
				once.Do(func() { close(started) })
			}
		}()
		<-started

		ctx, cancel := context.WithCancel(t.Context())
		errCh := make(chan error, 1)
		go func() { errCh <- e.Listen(ctx) }()

		deadline := time.Now().Add(15 * time.Second)
		var listenErr error
	wait:
		for e.Addr() == nil {
			select {
			case listenErr = <-errCh:
				break wait
			case <-time.After(time.Millisecond):
			}
			if time.Now().After(deadline) {
				break
			}
		}
		bound := e.Addr() != nil
		if bound {
			time.Sleep(5 * time.Millisecond) // reads after the publish, too
		}
		stop.Store(true)
		<-readerDone
		cancel()
		if listenErr == nil {
			select {
			case listenErr = <-errCh:
			case <-time.After(10 * time.Second):
				t.Errorf("iteration %d: Listen did not return after cancel", i)
			}
		}
		if done != nil {
			done()
		}
		if !bound {
			t.Fatalf("iteration %d: engine never bound (Listen: %v)", i, listenErr)
		}
	}
}

func TestEngineAccessorsDuringListenAreRaceFree(t *testing.T) {
	const iterations = 5

	t.Run("Metrics", func(t *testing.T) {
		listenRaceCycles(t, iterations, func(_ *testing.T, e *Engine) (func(), func()) {
			return func() { _ = e.Metrics() }, nil
		})
	})

	t.Run("NumWorkers", func(t *testing.T) {
		// Already under e.mu; the control that the harness is not what
		// reports the others.
		listenRaceCycles(t, iterations, func(_ *testing.T, e *Engine) (func(), func()) {
			return func() { _ = e.NumWorkers() }, nil
		})
	})

	t.Run("StartStopTransplant", func(t *testing.T) {
		listenRaceCycles(t, iterations, func(_ *testing.T, e *Engine) (func(), func()) {
			return func() {
				e.StartTransplant(stubTarget{})
				e.StopTransplant()
			}, nil
		})
	})

	t.Run("AdoptConn", func(t *testing.T) {
		// Each call that finds no loop yet returns an error and leaves fd
		// with the caller; the first that finds one hands fd over, and the
		// loop closes it at shutdown. After that the reader is idle, which
		// is enough: its one unlocked read of e.loops is what races.
		listenRaceCycles(t, iterations, func(t *testing.T, e *Engine) (func(), func()) {
			client, fd := adoptable543(t)
			var adopted atomic.Bool
			read := func() {
				if adopted.Load() {
					time.Sleep(50 * time.Microsecond)
					return
				}
				if err := e.AdoptConn(fd, engine.Carryover{RemoteAddr: client.LocalAddr().String()}); err == nil {
					adopted.Store(true)
				}
			}
			done := func() {
				if !adopted.Load() {
					_ = unix.Close(fd)
				}
			}
			return read, done
		})
	})
}
