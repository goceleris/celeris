//go:build linux

package adaptive

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// celeris#890 item 98: s0Bind (and newBoundAdaptiveH) gave Listen 3 s to
// publish its address and failed "adaptive engine never bound"; under CPU
// starvation that deadline was missed 1 time in 400 runs. awaitBind890 is the
// wait that replaced it; these tests drive it with a fake address source.

func fakeAddr890() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4242} }

// TestAwaitBindOutwaitsTheOldThreeSeconds890: the limit the tests use is well
// past the 3 s that was missed. It pins the constant; that the wait waits is
// what the four tests below show.
func TestAwaitBindOutwaitsTheOldThreeSeconds890(t *testing.T) {
	if bindLimit890 < 15*time.Second {
		t.Errorf("bindLimit890 = %v, want at least 15 s (it was 3 s when the flake hit)", bindLimit890)
	}
}

func TestAwaitBindReturnsTheAddressAndHowLongItTook890(t *testing.T) {
	var published atomic.Pointer[net.Addr]
	go func() {
		time.Sleep(120 * time.Millisecond)
		a := fakeAddr890()
		published.Store(&a)
	}()
	addr := func() net.Addr {
		if p := published.Load(); p != nil {
			return *p
		}
		return nil
	}
	a, took, err := awaitBind890(addr, make(chan struct{}), 10*time.Second)
	if err != nil || a == nil {
		t.Fatalf("awaitBind890 = %v, %v, %v for an address published after 120 ms", a, took, err)
	}
	if took < 100*time.Millisecond || took > 5*time.Second {
		t.Errorf("took = %v, want about 120 ms", took)
	}
}

func TestAwaitBindStopsWhenListenReturns890(t *testing.T) {
	ended := make(chan struct{})
	close(ended)
	start := time.Now()
	a, _, err := awaitBind890(func() net.Addr { return nil }, ended, 10*time.Second)
	if err == nil || a != nil {
		t.Fatalf("awaitBind890 = %v, %v after Listen returned, want an error", a, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v to notice that Listen had returned", time.Since(start))
	}
}

func TestAwaitBindGivesUpAtTheLimit890(t *testing.T) {
	a, took, err := awaitBind890(func() net.Addr { return nil }, make(chan struct{}), 150*time.Millisecond)
	if err == nil || a != nil {
		t.Fatalf("awaitBind890 = %v, %v with no address, want an error at the limit", a, err)
	}
	if took < 150*time.Millisecond || took > 5*time.Second {
		t.Errorf("took = %v, want about the 150 ms limit", took)
	}
}

// TestAwaitBindSeesAnAddressPublishedAsListenReturns890: an address that is
// there when Listen has returned is an address.
func TestAwaitBindSeesAnAddressPublishedAsListenReturns890(t *testing.T) {
	ended := make(chan struct{})
	close(ended)
	a, _, err := awaitBind890(fakeAddr890, ended, 10*time.Second)
	if err != nil || a == nil {
		t.Fatalf("awaitBind890 = %v, %v, want the address", a, err)
	}
}
