//go:build linux

package adaptive

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// TestAdaptiveStartedOnIOUringBoundsDetection974 (celeris#974): the adaptive
// engine builds io_uring workers, so a conn that opens with "PRI " and then
// floods garbage must be closed there too, and one that stalls before its
// protocol is detected must be reaped by ReadHeaderTimeout. The engine is
// started on io_uring (CELERIS_ADAPTIVE_START) and the test fails, not
// passes, if it is not what is serving: the root package cannot see which
// engine an adaptive server started on.
func TestAdaptiveStartedOnIOUringBoundsDetection974(t *testing.T) {
	t.Setenv("CELERIS_ADAPTIVE_START", "iouring")

	// closedWithin reports whether the server closes c (EOF or reset) before
	// the limit; a read that times out is not a close.
	closedWithin := func(c net.Conn, limit time.Duration) (bool, error) {
		_ = c.SetReadDeadline(time.Now().Add(limit))
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				var ne net.Error
				return !errors.As(err, &ne) || !ne.Timeout(), err
			}
		}
	}
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	// heapPeak samples HeapAlloc every 10 ms until stopped: the bytes held
	// while a connection is open are gone once the server closes it, so a
	// server that closes late is caught only while it holds them.
	heapPeak := func() (stop func() uint64) {
		var peak atomic.Uint64
		done, finished := make(chan struct{}), make(chan struct{})
		sample := func() {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
		}
		go func() {
			defer close(finished)
			for {
				sample()
				select {
				case <-done:
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}()
		return func() uint64 {
			close(done)
			<-finished
			sample()
			return peak.Load()
		}
	}

	// The flood runs on an engine whose header deadline is out of reach, so
	// that only the engine's own verdict on the bytes can close the conn.
	t.Run("flood-after-PRI", func(t *testing.T) {
		addr := startOnIOUring974(t, 5*time.Minute)
		h0 := heap()
		stopPeak := heapPeak()
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "PRI ")
		time.Sleep(100 * time.Millisecond)
		chunk := []byte(strings.Repeat("Z", 1<<20))
		sent := 0
		for ; sent < 96; sent++ {
			_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if _, err := c.Write(chunk); err != nil {
				break
			}
		}
		closed, rerr := closedWithin(c, 5*time.Second)
		time.Sleep(300 * time.Millisecond)
		peak := stopPeak()
		h1 := heap()
		t.Logf("sent %d MiB, closed=%v (%v), heap %d -> %d KiB, peak delta %d KiB", sent, closed, rerr, h0>>10, h1>>10, (int64(peak)-int64(h0))>>10)
		if !closed {
			t.Errorf("the adaptive engine on io_uring did not close a conn that sent PRI and %d MiB of garbage: %v", sent, rerr)
		}
		if grew := int64(h1) - int64(h0); grew > 12<<20 {
			t.Errorf("heap grew by %d MiB for %d MiB sent after PRI", grew>>20, sent)
		}
		if grew := int64(peak) - int64(h0); grew > 12<<20 {
			t.Errorf("heap peaked %d MiB over its start while %d MiB were sent after PRI: the engine held the bytes before it closed", grew>>20, sent)
		}
	})
	t.Run("GE-then-stall", func(t *testing.T) {
		addr := startOnIOUring974(t, 300*time.Millisecond)
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		start := time.Now()
		_, _ = io.WriteString(c, "GE")
		closed, rerr := closedWithin(c, 5*time.Second)
		t.Logf("closed=%v (%v) after %v", closed, rerr, time.Since(start).Round(10*time.Millisecond))
		if !closed {
			t.Errorf("a conn that sent GE and stalled was not reaped within 5s (ReadHeaderTimeout 300ms): %v", rerr)
		}
	})
}

// startOnIOUring974 starts an adaptive engine (Protocol Auto, ReadTimeout and
// IdleTimeout out of reach) on io_uring and fails the test if it did not
// start there.
func startOnIOUring974(t *testing.T, readHeaderTimeout time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:              addr,
		Protocol:          engine.Auto,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       5 * time.Minute,
		Resources:         resource.Resources{Workers: 2},
	}, respHandler{}, nil)
	if err != nil {
		t.Fatalf("adaptive.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("adaptive engine did not stop within 10s")
		}
	})
	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("adaptive engine never bound")
	}
	if got := getOnce(t, addr); got != 200 {
		t.Fatalf("GET = %d, want 200", got)
	}
	if active := e.ActiveEngine().Type(); active != engine.IOUring {
		t.Fatalf("the adaptive engine started on %v, not io_uring: this test would not cover io_uring's detection", active)
	}
	return addr
}
