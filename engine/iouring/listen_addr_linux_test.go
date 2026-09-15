//go:build linux

package iouring

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// celeris#639. Listen used to publish getsockname(workers[0].listenFD) after
// every worker had signalled ready. A worker whose context is already
// cancelled runs shutdown straight after ready, and shutdown closes listenFD
// without resetting it, so the published address was nil (a closed descriptor)
// or, had the number been reused, another socket's address.

func freeAddr639(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// ioUringUnavailable639 reports a Listen error that means the runner cannot
// create rings at all, which is not what these tests judge.
func ioUringUnavailable639(err error) bool {
	s := err.Error()
	return strings.Contains(s, "cannot allocate memory") || strings.Contains(s, "io_uring_setup") ||
		strings.Contains(s, "tier")
}

// TestListenWithCancelledContextPublishesTheBoundAddress is the regression
// assertion: a Listen that reports success must have published the address it
// bound, even when its context was cancelled before the workers started.
func TestListenWithCancelledContextPublishesTheBoundAddress(t *testing.T) {
	const attempts = 20
	var succeeded, nilAddr, wrongAddr int
	for i := range attempts {
		addr := freeAddr639(t)
		e, err := New(resource.Config{
			Addr:      addr,
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: 4},
		}, respondingHandler{})
		if err != nil {
			t.Skipf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		done := make(chan error, 1)
		go func() { done <- e.Listen(ctx) }()
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: Listen with a cancelled context did not return within 10s", i)
		}
		if err != nil {
			if ioUringUnavailable639(err) {
				t.Skipf("io_uring unavailable on this runner: %v", err)
			}
			// Refusing to start is an honest outcome; reporting success with
			// no address, or the wrong one, is not.
			t.Logf("attempt %d: Listen returned %v", i, err)
			continue
		}
		succeeded++
		switch got := e.Addr(); {
		case got == nil:
			nilAddr++
		case got.String() != addr:
			wrongAddr++
			t.Logf("attempt %d: Addr() = %s, want %s", i, got, addr)
		}
	}
	t.Logf("%d attempts, %d reported success: nil address %d, wrong address %d", attempts, succeeded, nilAddr, wrongAddr)
	if succeeded == 0 {
		t.Fatal("no attempt reported success, so nothing was checked -- this guard is vacuous")
	}
	if nilAddr > 0 || wrongAddr > 0 {
		t.Fatalf("Listen reported success but published a nil address %d time(s) and a wrong one %d time(s) in %d successful starts",
			nilAddr, wrongAddr, succeeded)
	}
}

// TestListenRefusesToStartWhenNoWorkerReportsItsAddress: when no worker can
// say what it bound, Listen must return an error instead of logging
// "listening" and running with Addr() == nil forever.
func TestListenRefusesToStartWhenNoWorkerReportsItsAddress(t *testing.T) {
	orig := listenAddrOf
	listenAddrOf = func(int) net.Addr { return nil }
	t.Cleanup(func() { listenAddrOf = orig })

	e, err := New(resource.Config{
		Addr:      freeAddr639(t),
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, respondingHandler{})
	if err != nil {
		t.Skipf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	select {
	case err := <-done:
		if err != nil && ioUringUnavailable639(err) {
			t.Skipf("io_uring unavailable on this runner: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "no worker could report") {
			t.Fatalf("Listen = %v, want the no-address error", err)
		}
		t.Logf("Listen refused to start: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatalf("Listen kept running with no address to publish (Addr() = %v)", e.Addr())
	}
	if a := e.Addr(); a != nil {
		t.Fatalf("Addr() = %v after a refused start, want nil", a)
	}
}
