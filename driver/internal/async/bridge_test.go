package async

import (
	"context"
	"errors"
	"sync"
	"testing"
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
