//go:build linux

package epoll

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// celeris#639. Listen used to publish getsockname(loops[0].listenFD) after
// every loop had signalled ready. A loop whose context is already cancelled
// runs shutdown straight after ready, and shutdown closes listenFD without
// resetting it, so the published address was nil (a closed descriptor) or,
// had the number been reused, another socket's address. Every caller that
// polls Addr() reads nil as "not bound yet".

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

// TestListenWithCancelledContextPublishesTheBoundAddress is the regression
// assertion: a Listen that reports success must have published the address it
// bound, even when its context was cancelled before the loops started.
func TestListenWithCancelledContextPublishesTheBoundAddress(t *testing.T) {
	const attempts = 20
	var succeeded, nilAddr, wrongAddr int
	for i := range attempts {
		addr := freeAddr639(t)
		eng, err := New(resource.Config{
			Addr:      addr,
			Protocol:  celerisengine.HTTP1,
			Resources: resource.Resources{Workers: 4},
		}, respondingHandler{})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		done := make(chan error, 1)
		go func() { done <- eng.Listen(ctx) }()
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: Listen with a cancelled context did not return within 10s", i)
		}
		if err != nil {
			// Refusing to start is an honest outcome; reporting success with
			// no address, or the wrong one, is not.
			t.Logf("attempt %d: Listen returned %v", i, err)
			continue
		}
		succeeded++
		switch got := eng.Addr(); {
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

// TestListenRefusesToStartWhenNoLoopReportsItsAddress: when no loop can say
// what it bound, Listen must return an error instead of logging "listening"
// and running with Addr() == nil forever.
func TestListenRefusesToStartWhenNoLoopReportsItsAddress(t *testing.T) {
	orig := listenAddrOf
	listenAddrOf = func(int) net.Addr { return nil }
	t.Cleanup(func() { listenAddrOf = orig })

	eng, err := New(resource.Config{
		Addr:      freeAddr639(t),
		Protocol:  celerisengine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- eng.Listen(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no loop could report") {
			t.Fatalf("Listen = %v, want the no-address error", err)
		}
		t.Logf("Listen refused to start: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatalf("Listen kept running with no address to publish (Addr() = %v)", eng.Addr())
	}
	if a := eng.Addr(); a != nil {
		t.Fatalf("Addr() = %v after a refused start, want nil", a)
	}
}
