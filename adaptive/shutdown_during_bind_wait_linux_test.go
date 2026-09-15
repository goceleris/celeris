//go:build linux

package adaptive

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// celeris#638. celeris#595 pinned that Server.Start returns after
// Server.Shutdown on every engine. The adaptive engine has one path that fix did not reach: Listen
// parks in a 20-second poll waiting for the ACTIVE sub-engine to publish
// Addr(), and that poll selects only on the sub-engine error channel, its own
// 20s timer, and a 5 ms ticker — never on the context. Server.Shutdown
// cancels that context and then joins Listen, so while the wait is running
// Shutdown cannot end it: Shutdown burns its whole deadline and Start stays
// parked for the remainder of the 20 seconds.
//
// The trigger in the wild is an active sub-engine that never publishes an
// address. epoll produces exactly that shape: Listen stores whatever
// boundAddr() returns for loop 0's listen fd — including nil when
// getsockname fails — logs "epoll engine listening" regardless, and serves
// traffic on its other loops. Addr() then reads nil forever, so the wait
// cannot finish on its own either. That nil-address handling is a separate
// defect, filed as celeris#639; this test is about the bind wait, which is
// wrong whatever delays the address.
//
// nilAddrEngine reproduces that shape without needing the rare trigger: a
// sub-engine that starts, parks on the context like every real engine, and
// never publishes an address.
type nilAddrEngine struct {
	*mockEngine
}

func (n *nilAddrEngine) Addr() net.Addr { return nil }

// TestShutdownEndsListenParkedInBindWait is the assertion: Shutdown must end
// a Listen that is still waiting for the active sub-engine to bind.
func TestShutdownEndsListenParkedInBindWait(t *testing.T) {
	primary := &nilAddrEngine{mockEngine: newMockEngine(engine.Epoll)}
	secondary := newMockEngine(engine.IOUring)
	e := newFromEngines(primary, secondary, newSyntheticSampler(), resource.Config{})

	listenDone := make(chan error, 1)
	go func() { listenDone <- e.Listen(context.Background()) }()

	// The active sub-engine's Listen goroutine is up, so the adaptive Listen
	// is past wg.Go and inside the bind wait.
	select {
	case <-primary.listenStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("active sub-engine Listen never started")
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutCancel()
	t0 := time.Now()
	if err := e.Shutdown(shutCtx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	shutElapsed := time.Since(t0)

	select {
	case err := <-listenDone:
		elapsed := time.Since(t0)
		if err != nil {
			t.Errorf("Listen returned error %v, want nil (an ordinary shutdown)", err)
		}
		if elapsed > 3*time.Second {
			t.Errorf("Listen returned %v after Shutdown, want <= 3s", elapsed)
		}
		if shutElapsed > 1*time.Second {
			t.Errorf("Shutdown itself took %v, want well under its 2s deadline "+
				"(it burned the deadline waiting for Listen)", shutElapsed)
		}
		t.Logf("Shutdown took %v; Listen returned %v after Shutdown", shutElapsed, elapsed)
	case <-time.After(25 * time.Second):
		t.Fatal("Listen never returned; it waited out the 20s bind deadline " +
			"instead of observing the cancelled context")
	}
}

// TestShutdownEndsListenAfterBindWaitSucceeded is the control: with a
// sub-engine that DOES publish an address, the same rig must pass both before
// and after the fix. If this one ever fails, the assertion above is measuring
// the rig rather than the defect.
func TestShutdownEndsListenAfterBindWaitSucceeded(t *testing.T) {
	primary := newMockEngine(engine.Epoll)
	secondary := newMockEngine(engine.IOUring)
	e := newFromEngines(primary, secondary, newSyntheticSampler(), resource.Config{})

	listenDone := make(chan error, 1)
	go func() { listenDone <- e.Listen(context.Background()) }()

	select {
	case <-primary.listenStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("active sub-engine Listen never started")
	}
	// Let Listen get past the bind wait and into its final select.
	deadline := time.Now().Add(5 * time.Second)
	for e.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("adaptive engine never published its address")
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutCancel()
	t0 := time.Now()
	if err := e.Shutdown(shutCtx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	select {
	case err := <-listenDone:
		if err != nil {
			t.Errorf("Listen returned error %v, want nil", err)
		}
		if elapsed := time.Since(t0); elapsed > 3*time.Second {
			t.Errorf("Listen returned %v after Shutdown, want <= 3s", elapsed)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("control: Listen never returned after a normal start")
	}
}
