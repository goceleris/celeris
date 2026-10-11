//go:build linux

package iouring_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/adaptive"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#848: the adaptive engine stops and starts its sub-engines' loops as
// it switches (the standby is built lazily, the old engine is paused and its
// connections handed over). None of that is a loop that ended without
// returning, so none of it may log the Error that reports one. The test forces
// a switch each way with a keep-alive connection open and requires no record,
// and that a request is still served after each switch (the loops did their
// work, so the silence is not a loop that never ran).

type okHandler848 struct{}

func (okHandler848) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

type exitLog848 struct {
	mu    sync.Mutex
	exits int
	total int
}

func (c *exitLog848) Enabled(context.Context, slog.Level) bool { return true }
func (c *exitLog848) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *exitLog848) WithGroup(string) slog.Handler            { return c }
func (c *exitLog848) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	c.total++
	if strings.Contains(r.Message, "goroutine ended without returning") {
		c.exits++
	}
	c.mu.Unlock()
	return nil
}

func get848(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(b), "HTTP/1.1 200") {
		t.Fatalf("answered %q, want 200", b)
	}
}

func TestAdaptiveSwitchIsNotALoopExit848(t *testing.T) {
	logs := &exitLog848{}
	ae, err := adaptive.New(resource.Config{
		Addr: "127.0.0.1:0", Protocol: engine.HTTP1,
		Resources: resource.Resources{Workers: 2}, Logger: slog.New(logs),
	}, okHandler848{}, nil)
	if err != nil {
		t.Fatalf("adaptive.New: %v", err)
	}
	ae.FreezeSwitching()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ae.Listen(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	for dl := time.Now().Add(5 * time.Second); ae.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if ae.Addr() == nil {
		t.Fatal("adaptive engine never bound")
	}
	addr := ae.Addr().String()
	keep, err := net.DialTimeout("tcp", addr, 2*time.Second) // idle keep-alive across the switches
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = keep.Close() }()
	get848(t, addr)
	start := ae.ActiveEngine().Type()
	switches := 0
	for want, i := otherType848(start), 0; i < 2; want, i = otherType848(want), i+1 {
		// The standby is built lazily; at a small RLIMIT_MEMLOCK a build just
		// after another engine closed can fail on ring ENOMEM, so retry.
		for j := 0; j < 20 && ae.ActiveEngine().Type() != want; j++ {
			ae.ForceSwitch()
			if ae.ActiveEngine().Type() != want {
				time.Sleep(500 * time.Millisecond)
			}
		}
		if ae.ActiveEngine().Type() != want {
			iouring.SkipUnlessIOUringUnusable844(t, fmt.Errorf("the forced switch left %v active, want %v", ae.ActiveEngine().Type(), want))
		}
		switches++
		get848(t, addr) // served by the engine the switch made active
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		done <- err // the cleanup waits for it too
	case <-time.After(30 * time.Second):
		t.Fatal("Listen did not return within 30 s of cancel")
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	t.Logf("celeris848 RESULT adaptive switches=%d start=%v records=%d logged_total=%d", switches, start, logs.exits, logs.total)
	if logs.total == 0 {
		t.Fatal("PREMISE: the logger received nothing")
	}
	if logs.exits != 0 {
		t.Fatalf("celeris#848: %d forced switches produced %d %q record(s)", switches, logs.exits, "goroutine ended without returning")
	}
}

func otherType848(t engine.EngineType) engine.EngineType {
	if t == engine.Epoll {
		return engine.IOUring
	}
	return engine.Epoll
}
