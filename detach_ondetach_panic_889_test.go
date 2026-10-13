package celeris

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestDetachOnDetachPanicReleasesTheContext889 pins that a panic out of the
// engine's OnDetach hook inside Context.Detach does not leave the Context and
// a goroutine retained for ever. Detach had set c.detached but assigned
// c.detachDone only after OnDetach, so recoverAndRelease started a goroutine
// that received from a nil channel (#889 item 33).
func TestDetachOnDetachPanicReleasesTheContext889(t *testing.T) {
	cases := []struct {
		name string
		// handlerRecovers: the handler recovers the panic itself, as
		// middleware/recovery does, so the adapter's own recover() sees nothing.
		handlerRecovers bool
	}{
		{"adapter recovers", false},
		{"handler recovers", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(Config{})
			released := make(chan struct{}, 1)
			srv.GET("/d", func(c *Context) error {
				c.OnRelease(func() { released <- struct{}{} })
				if tc.handlerRecovers {
					defer func() { _ = recover() }()
				}
				c.Detach()
				t.Error("Detach returned although OnDetach panicked")
				return nil
			})
			adapter := &routerAdapter{server: srv}

			runtime.GC()
			baseline := runtime.NumGoroutine()

			s, _ := newTestStream("GET", "/d")
			s.OnDetach = func() { panic("engine fault in OnDetach") }
			if err := adapter.HandleStream(context.Background(), s); err != nil {
				t.Fatalf("HandleStream: %v", err)
			}

			select {
			case <-released:
			case <-time.After(3 * time.Second):
				t.Fatal("the Context was not released after Detach's OnDetach panicked: " +
					"recoverAndRelease waits on a detachDone nobody closes")
			}
			deadline := time.Now().Add(3 * time.Second)
			for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if n := runtime.NumGoroutine(); n > baseline {
				t.Errorf("%d goroutines after the request, %d before: the release waiter is parked", n, baseline)
			}
			s.Release()
		})
	}
}

// TestDetachDoneStillReleasesAfterTheFix889 is the control of the test above:
// with no panic the release still waits for the done func Detach returned, and
// follows it.
func TestDetachDoneStillReleasesAfterTheFix889(t *testing.T) {
	srv := New(Config{})
	released := make(chan struct{}, 1)
	doneCh := make(chan func(), 1)
	srv.GET("/d", func(c *Context) error {
		c.OnRelease(func() { released <- struct{}{} })
		doneCh <- c.Detach()
		return nil
	})
	adapter := &routerAdapter{server: srv}
	s, _ := newTestStream("GET", "/d")
	defer s.Release()
	if err := adapter.HandleStream(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	done := <-doneCh
	select {
	case <-released:
		t.Fatal("the Context was released before done was called")
	case <-time.After(100 * time.Millisecond):
	}
	done()
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("the Context was not released after done")
	}
}
