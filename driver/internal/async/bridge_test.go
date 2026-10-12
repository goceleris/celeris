package async

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testReq struct {
	ctx context.Context
	id  int
}

func (r *testReq) Ctx() context.Context { return r.ctx }

func TestBridgeFIFO(t *testing.T) {
	b := NewBridge()
	if b.Len() != 0 {
		t.Fatalf("expected empty bridge, got len=%d", b.Len())
	}
	if b.Head() != nil {
		t.Fatalf("expected nil head on empty bridge")
	}
	if b.Pop() != nil {
		t.Fatalf("expected nil pop on empty bridge")
	}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		b.Enqueue(&testReq{ctx: ctx, id: i})
	}
	if b.Len() != 5 {
		t.Fatalf("expected len=5, got %d", b.Len())
	}
	if h, ok := b.Head().(*testReq); !ok || h.id != 0 {
		t.Fatalf("head mismatch: %+v", b.Head())
	}
	for i := 0; i < 5; i++ {
		r := b.Pop().(*testReq)
		if r.id != i {
			t.Fatalf("expected id=%d, got %d", i, r.id)
		}
	}
	if b.Len() != 0 {
		t.Fatalf("expected empty after drain, got len=%d", b.Len())
	}
}

func TestBridgeDrainWithError(t *testing.T) {
	b := NewBridge()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		b.Enqueue(&testReq{ctx: ctx, id: i})
	}
	sentinel := errors.New("closed")
	var got []int
	var gotErr error
	b.DrainWithError(sentinel, func(r PendingRequest, err error) {
		got = append(got, r.(*testReq).id)
		gotErr = err
	})
	if gotErr != sentinel {
		t.Fatalf("expected sentinel err, got %v", gotErr)
	}
	if len(got) != 3 || got[0] != 0 || got[2] != 2 {
		t.Fatalf("drain order wrong: %v", got)
	}
	if b.Len() != 0 {
		t.Fatalf("expected empty after drain, got %d", b.Len())
	}
}

func TestBridgeConcurrent(t *testing.T) {
	b := NewBridge()
	ctx := context.Background()
	const n = 1000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			b.Enqueue(&testReq{ctx: ctx, id: i})
		}
	}()
	go func() {
		defer wg.Done()
		popped := 0
		for popped < n {
			if r := b.Pop(); r != nil {
				popped++
			}
		}
	}()
	wg.Wait()
	if b.Len() != 0 {
		t.Fatalf("expected empty, got %d", b.Len())
	}
}

func TestBridgeDrainNilNotify(t *testing.T) {
	// Passing nil notify must not panic; queue should still empty.
	b := NewBridge()
	b.Enqueue(&testReq{ctx: context.Background(), id: 1})
	b.DrainWithError(errors.New("e"), nil)
	if b.Len() != 0 {
		t.Fatalf("expected empty after nil-notify drain, got %d", b.Len())
	}
}

// celeris#887 item 21: DrainWithError resets head and size under the lock and
// then walked the OLD ring outside it, but the ring is the same slice, so an
// Enqueue (the caller's exec) that runs meanwhile writes slot 0 of the ring
// the drain is still reading and clearing. The drain then notified the new
// request with the old requests' error and cleared it from the queue, and an
// old request it had not reached yet was overwritten: its waiter was never
// notified.
//
// The drained entries wrap around the ring (A in its last slot, B in slot 0),
// so the new request lands on B's slot. notify blocks on A until the other
// goroutine has enqueued C, which makes the interleaving deterministic.
func TestBridgeDrainKeepsAConcurrentEnqueue887(t *testing.T) {
	b := NewBridge()
	ctx := context.Background()
	for i := 0; i < 15; i++ { // head to the last slot of the 16-slot ring
		b.Enqueue(&testReq{ctx: ctx, id: 100 + i})
		b.Pop()
	}
	b.Enqueue(&testReq{ctx: ctx, id: 1}) // A, slot 15
	b.Enqueue(&testReq{ctx: ctx, id: 2}) // B, slot 0

	sentinel := errors.New("closed")
	started, gate, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var got []int
	go func() {
		defer close(done)
		b.DrainWithError(sentinel, func(r PendingRequest, err error) {
			if err != sentinel {
				t.Errorf("notify err = %v, want the sentinel", err)
			}
			got = append(got, r.(*testReq).id)
			if len(got) == 1 {
				close(started)
				<-gate
			}
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never notified A")
	}
	b.Enqueue(&testReq{ctx: ctx, id: 3}) // C, from another goroutine, mid-drain
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not finish")
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("the drain notified %v, want [1 2] (A and B, the requests queued when it started)", got)
	}
	if n := b.Len(); n != 1 {
		t.Fatalf("Len = %d after the drain, want 1 (C, enqueued after it started)", n)
	}
	if r := b.Pop(); r == nil || r.(*testReq).id != 3 {
		t.Errorf("Pop = %v, want C (id 3)", r)
	}
}

// The same race, found by the race detector: Enqueue, Pop and
// DrainWithError on one bridge from three goroutines (a conn closing under
// traffic). Every request that was enqueued must leave exactly once, popped
// or drained.
func TestBridgeDrainRacesNeitherEnqueueNorPop887(t *testing.T) {
	b := NewBridge()
	ctx := context.Background()
	const n = 4000
	var popped, drained atomic.Int64
	var producer, consumers sync.WaitGroup
	stop := make(chan struct{})
	producer.Add(1)
	go func() {
		defer producer.Done()
		for i := 0; i < n; i++ {
			b.Enqueue(&testReq{ctx: ctx, id: i})
			if i%8 == 0 {
				runtime.Gosched()
			}
		}
	}()
	consumers.Add(2)
	go func() {
		defer consumers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if r := b.Pop(); r != nil {
				popped.Add(1)
			}
		}
	}()
	go func() {
		defer consumers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			b.DrainWithError(io.EOF, func(PendingRequest, error) { drained.Add(1) })
			runtime.Gosched()
		}
	}()
	producer.Wait()
	close(stop)
	consumers.Wait()
	b.DrainWithError(io.EOF, func(PendingRequest, error) { drained.Add(1) }) // what is left
	if total := popped.Load() + drained.Load(); total != n {
		t.Errorf("%d requests enqueued, %d popped and %d drained: a request was lost or left twice", n, popped.Load(), drained.Load())
	}
}
