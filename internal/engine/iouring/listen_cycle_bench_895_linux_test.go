//go:build linux

package iouring

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkListenCycle895 is the cost of what celeris#895 and #896 changed:
// the engine's start (Listen called until Addr() is published, which resolves
// a port 0) and its stop (the context cancelled until Listen has returned,
// which waits for the accept armed on the listen socket to end). Nothing on
// the request path was touched; this is the whole of the start/stop path.
// Run it at memlock unlimited with -benchtime=200x.
func BenchmarkListenCycle895(b *testing.B) {
	var startNs, stopNs int64
	for range b.N {
		e, err := New(resource.Config{
			Addr:      "127.0.0.1:0",
			Engine:    engine.IOUring,
			Protocol:  engine.HTTP1,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Resources: resource.Resources{Workers: 2},
		}, respondingHandler{})
		if err != nil {
			b.Skipf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		t0 := time.Now()
		go func() { done <- e.Listen(ctx) }()
		for e.Addr() == nil {
			select {
			case err := <-done:
				b.Fatalf("Listen returned before publishing an address: %v", err)
			default:
			}
			time.Sleep(20 * time.Microsecond)
		}
		t1 := time.Now()
		cancel()
		<-done
		t2 := time.Now()
		startNs += t1.Sub(t0).Nanoseconds()
		stopNs += t2.Sub(t1).Nanoseconds()
	}
	b.ReportMetric(float64(startNs)/float64(b.N)/1e3, "start-us/op")
	b.ReportMetric(float64(stopNs)/float64(b.N)/1e3, "stop-us/op")
}

// BenchmarkPauseResumeCycle896 is the cost of the other caller of cancelAccept,
// the pause's close (celeris#896 gave cancelAccept the shutdown drain's wait
// for the accept's end): one PauseAccept (returns once every worker has closed
// its listen socket; DisableDeferAccept, so at once) and one ResumeAccept
// (until a connect is accepted again, so every worker is listening). Two
// workers, memlock unlimited, -benchtime=200x.
func BenchmarkPauseResumeCycle896(b *testing.B) {
	e, err := New(resource.Config{
		Addr:               "127.0.0.1:0",
		Engine:             engine.IOUring,
		Protocol:           engine.HTTP1,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		DisableDeferAccept: true,
		Resources:          resource.Resources{Workers: 2},
	}, respondingHandler{})
	if err != nil {
		b.Skipf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() { cancel(); <-done }()
	for e.Addr() == nil {
		select {
		case err := <-done:
			b.Fatalf("Listen returned before publishing an address: %v", err)
		default:
		}
		time.Sleep(20 * time.Microsecond)
	}
	addr := e.Addr().String()
	var pauseNs, resumeNs int64
	b.ResetTimer()
	for range b.N {
		t0 := time.Now()
		if err := e.PauseAccept(); err != nil {
			b.Fatalf("PauseAccept: %v", err)
		}
		t1 := time.Now()
		if err := e.ResumeAccept(); err != nil {
			b.Fatalf("ResumeAccept: %v", err)
		}
		for {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				_ = c.Close()
				break
			}
			time.Sleep(20 * time.Microsecond)
		}
		t2 := time.Now()
		pauseNs += t1.Sub(t0).Nanoseconds()
		resumeNs += t2.Sub(t1).Nanoseconds()
	}
	b.ReportMetric(float64(pauseNs)/float64(b.N)/1e3, "pause-us/op")
	b.ReportMetric(float64(resumeNs)/float64(b.N)/1e3, "resume-us/op")
}
