package eventloop

// The order of the last callbacks when Loop.Close tears down a registered
// conn whose peer keeps sending. Loop.Close on Linux runs worker.shutdown
// only after the worker goroutine has stopped, so no onRecv runs during or
// after onClose(ErrLoopClosed), and the portable worker keeps that: onClose
// is the last callback of a conn, it carries ErrLoopClosed, and it has
// returned when Loop.Close does.

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func runPortableCloseOrder(t *testing.T, rounds int, hold time.Duration) (errLoopClosed, during, after, unfinished int) {
	t.Helper()
	srv := newLoopbackServer(t)
	for round := 0; round < rounds; round++ {
		l, err := NewPortable(1)
		if err != nil {
			t.Fatalf("NewPortable: %v", err)
		}
		client, peer := srv.dial(t)
		stop := make(chan struct{})
		go func() {
			buf := make([]byte, 4096)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := peer.Write(buf); err != nil {
					return
				}
			}
		}()
		var phase atomic.Int32 // 0 before onClose, 1 inside, 2 returned
		var inside, afterN atomic.Int64
		var got atomic.Value
		first := make(chan struct{}, 1)
		fired := make(chan struct{})
		onRecv := func([]byte) {
			switch phase.Load() {
			case 1:
				inside.Add(1)
			case 2:
				afterN.Add(1)
			}
			select {
			case first <- struct{}{}:
			default:
			}
		}
		onClose := func(e error) {
			phase.Store(1)
			got.Store(&e)
			if hold > 0 { // stands in for a driver draining its pending requests
				for deadline := time.Now().Add(hold); time.Now().Before(deadline); {
				}
			}
			phase.Store(2)
			close(fired)
		}
		if _, err := registrar(t, l, 0).RegisterNetConn(client, onRecv, onClose); err != nil {
			t.Fatalf("RegisterNetConn: %v", err)
		}
		select {
		case <-first:
		case <-time.After(portableWait):
			t.Fatal("onRecv never ran")
		}
		_ = l.Close()
		if phase.Load() != 2 {
			unfinished++
		}
		select {
		case <-fired:
		case <-time.After(portableWait):
			t.Fatal("onClose never ran")
		}
		time.Sleep(5 * time.Millisecond) // a straggling onRecv would land here
		close(stop)
		if p, _ := got.Load().(*error); p != nil && errors.Is(*p, ErrLoopClosed) {
			errLoopClosed++
		}
		if inside.Load() > 0 {
			during++
		}
		if afterN.Load() > 0 {
			after++
		}
	}
	return
}

func TestPortableLoopCloseOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold time.Duration
	}{{"hold0", 0}, {"hold1ms", time.Millisecond}} {
		t.Run(tc.name, func(t *testing.T) {
			const rounds = 50
			e, d, a, u := runPortableCloseOrder(t, rounds, tc.hold)
			t.Logf("TALLY rounds=%d onClose(ErrLoopClosed)=%d onRecv-during-onClose=%d onRecv-after-onClose=%d onClose-unfinished-at-Close-return=%d", rounds, e, d, a, u)
			if e != rounds || d != 0 || a != 0 || u != 0 {
				t.Errorf("want ErrLoopClosed in every round, no onRecv during or after onClose, onClose returned before Loop.Close did; got %d/%d, during %d, after %d, unfinished %d", e, rounds, d, a, u)
			}
		})
	}
}

// An onClose that writes (RULE 10): it writes to its own conn while Loop.Close
// runs it, and must get ErrLoopClosed, not hang.
func TestPortableOnCloseWritingDuringLoopCloseDoesNotHang(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	client, _ := srv.dial(t)
	var id int
	fired := make(chan error, 1)
	id, err := registrar(t, l, 0).RegisterNetConn(client, nil, func(e error) {
		fired <- l.WorkerLoop(0).Write(id, make([]byte, 64<<20))
	})
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = l.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(portableWait):
		t.Fatal("Loop.Close hung on an onClose that writes")
	}
	if err := <-fired; !errors.Is(err, ErrLoopClosed) {
		t.Fatalf("Write from onClose during Loop.Close: %v, want ErrLoopClosed", err)
	}
}
