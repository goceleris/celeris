//go:build linux

package adaptive

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#886 item 26 (PR #1012 final review): Metrics reads the two sub-engine
// slots under slotsMu, a leaf lock, so performSwitch's two slot-publish blocks
// must write them under it. No test caught dropping slotsMu from the block that
// publishes the lazily built EPOLL standby (e.primary, on a server that starts
// on io_uring); dropping it from the io_uring block was caught only by the race
// detector, if a Metrics poll happened to overlap.
//
// This test needs neither -race nor an overlap. It holds slotsMu itself, runs a
// switch that has to build and publish a standby, and requires that
// performSwitch parks on the lock before it publishes or commits anything. A
// publish without the lock is not parked: the switch carries on and commits
// while the test still holds slotsMu.

// lazySwitch886 builds the lazy shapes of New: `active` is the eager start
// engine, the other slot is nil until the first switch builds it.
func lazySwitch886(t *testing.T, startOnIOUring bool) (e *Engine, built chan struct{}) {
	t.Helper()
	built = make(chan struct{})
	sampler := newSyntheticSampler()
	startType, standbyType := engine.Epoll, engine.IOUring
	if startOnIOUring {
		startType, standbyType = engine.IOUring, engine.Epoll
	}
	active := newMockEngine(startType)
	standby := newMockEngine(standbyType)
	e = &Engine{
		cfg:       resource.Config{Protocol: engine.HTTP1},
		logger:    testLogger(),
		startType: startType,
	}
	if startOnIOUring {
		e.secondary = active
	} else {
		e.primary = active
	}
	e.buildStandby = func() (engine.Engine, error) {
		close(built)
		return standby, nil
	}
	e.ctrl = newController(e.primary, e.secondary, sampler, e.logger)
	e.ctrl.state.activeIsPrimary = !startOnIOUring
	e.ctrl.cooldown = 0
	var ap engine.Engine = active
	e.active.Store(&ap)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(wg.Done)
	e.listenCtx = ctx
	e.listenWG = &wg
	return e, built
}

// performSwitchParkedOnAMutex886 reports whether a goroutine is inside
// performSwitch and parked in a sync.Mutex.Lock. The test holds only slotsMu,
// so that is the lock it is parked on.
func performSwitchParkedOnAMutex886() bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "(*Engine).performSwitch(") &&
			(strings.Contains(g, "sync.(*Mutex).Lock") || strings.Contains(g, "sync.(*Mutex).lockSlow") ||
				strings.Contains(g, "[sync.Mutex.Lock")) {
			return true
		}
	}
	return false
}

func TestPerformSwitchPublishesTheBuiltStandbyUnderSlotsMu886(t *testing.T) {
	for _, tc := range []struct {
		name           string
		startOnIOUring bool
		wantActive     engine.EngineType
	}{
		// e.secondary = the lazily built io_uring standby of an epoll start.
		{"io_uring standby of an epoll start (secondary block)", false, engine.IOUring},
		// e.primary = the lazily built epoll standby of an io_uring start.
		{"epoll standby of an io_uring start (primary block)", true, engine.Epoll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, built := lazySwitch886(t, tc.startOnIOUring)

			e.slotsMu.Lock()
			held := true
			defer func() {
				if held {
					e.slotsMu.Unlock()
				}
			}()
			done := make(chan struct{})
			go func() { defer close(done); e.performSwitch() }()

			select {
			case <-built:
			case <-time.After(10 * time.Second):
				t.Fatal("performSwitch never built the standby")
			}
			// The standby is built; the switch must now park on slotsMu to
			// publish it. A switch that publishes without the lock commits
			// instead (active changes) and never parks.
			deadline := time.Now().Add(10 * time.Second)
			for !performSwitchParkedOnAMutex886() {
				if now := (*e.active.Load()).Type(); now == tc.wantActive {
					t.Fatalf("performSwitch built the %v standby, published it and made it active while the test held slotsMu: "+
						"the slot-publish block does not take slotsMu", tc.wantActive)
				}
				if time.Now().After(deadline) {
					t.Fatal("performSwitch neither parked on slotsMu nor committed the switch within 10 s")
				}
				time.Sleep(time.Millisecond)
			}
			if got := (*e.active.Load()).Type(); got == tc.wantActive {
				t.Fatalf("the switch to %v committed while slotsMu was held", got)
			}

			e.slotsMu.Unlock()
			held = false
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("performSwitch did not finish after slotsMu was released")
			}
			if got := (*e.active.Load()).Type(); got != tc.wantActive {
				t.Errorf("active = %v after the switch, want %v", got, tc.wantActive)
			}
			// The slot is published and Metrics sees it.
			if tc.startOnIOUring && e.primary == nil || !tc.startOnIOUring && e.secondary == nil {
				t.Error("the built standby is in no slot after the switch")
			}
			e.Metrics()
		})
	}
}
