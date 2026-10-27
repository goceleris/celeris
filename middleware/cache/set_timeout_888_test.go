package cache

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

// Config.SetTimeout (celeris#888 item 30). PENDING the maintainer's decision
// on the default: nothing here names its value except
// TestDefaultSetTimeoutIsInTheDoc888 and TestSetTimeoutZeroSelectsTheDefault888,
// which go through defaultSetTimeout.

// A Set that hangs no longer holds the leader past SetTimeout, with a store
// that ignores its context and with one that honours it, coalesced or not.
func TestSetTimeoutFreesTheLeaderFromAHungSet888(t *testing.T) {
	const bound = 150 * time.Millisecond
	for _, honour := range []bool{false, true} {
		for _, noSF := range []bool{false, true} {
			name := "ignores-ctx"
			if honour {
				name = "honours-ctx"
			}
			if noSF {
				name += "/no-singleflight"
			} else {
				name += "/singleflight"
			}
			t.Run(name, func(t *testing.T) {
				kv := newHung888(honour)
				t.Cleanup(func() {
					select {
					case <-kv.release:
					default:
						close(kv.release)
					}
				})
				mw := New(Config{Store: kv, TTL: time.Minute, SetTimeout: bound, DisableSingleflight: noSF})
				began := time.Now()
				ch := get888("/k", mw, okHandler888("v"))
				waitClosed888(t, "the Set to start", kv.entered, 5*time.Second)
				var a answer921
				select {
				case a = <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("the leader is still held by the hung Set 5 s after it started (SetTimeout 150ms)")
				}
				el := time.Since(began)
				if a.body != "v" || a.xcache != "MISS" || a.err != nil || a.panicked != nil {
					t.Errorf("leader answered %+v, want 200 body v x-cache MISS", a)
				}
				if el < bound {
					t.Errorf("the leader was answered after %v, before the bound %v", el, bound)
				}
				if d := kv.deadline.Load(); d == nil {
					t.Errorf("the store's Set was given a context without a deadline")
				} else if until := time.Until(*d); until > bound {
					t.Errorf("the Set's context ends in %v, more than SetTimeout %v", until, bound)
				}
				if honour {
					waitClosed888(t, "the context-honouring Set to return at the deadline", kv.finished, 2*time.Second)
				} else {
					select {
					case <-kv.finished:
						t.Error("the ctx-ignoring Set returned on its own")
					default:
					}
					close(kv.release)
					waitClosed888(t, "the released Set to return", kv.finished, 2*time.Second)
				}
				// The service is intact: the next miss for the key runs the
				// handler and stores normally.
				a = <-get888("/k", mw, okHandler888("v2"))
				if a.xcache == "" || a.err != nil {
					t.Errorf("the request after the hung Set: %+v", a)
				}
			})
		}
	}
}

// Followers of a hung leader are not held either way (they were not before
// the field: celeris#921); with the leader freed at the bound the key is
// released and the response stored by a later Set is served.
func TestSetTimeoutLeaderFillIsServedAfterwards888(t *testing.T) {
	kv := newHung888(false)
	t.Cleanup(func() { close(kv.release) })
	mw := New(Config{Store: kv, TTL: time.Minute, SetTimeout: 100 * time.Millisecond})
	ch := get888("/k", mw, okHandler888("v1"))
	waitClosed888(t, "the Set to start", kv.entered, 5*time.Second)
	if a := <-ch; a.body != "v1" {
		t.Fatalf("leader: %+v", a)
	}
	// The hung Set never stored v1; a later request misses, runs, and its Set
	// (the store's second) works.
	a := <-get888("/k", mw, okHandler888("v2"))
	if a.body != "v2" || a.xcache != "MISS" {
		t.Errorf("after the leader gave up on its Set: %+v, want v2 MISS", a)
	}
	a = <-get888("/k", mw, okHandler888("v3"))
	if a.body != "v2" || a.xcache != "HIT" {
		t.Errorf("then: %+v, want the stored v2 HIT", a)
	}
}

// A negative SetTimeout is no bound: the Set gets the request's own context
// and the leader waits for it, as before the field.
func TestSetTimeoutNegativeIsNoBound888(t *testing.T) {
	kv := newHung888(false)
	t.Cleanup(func() {
		select {
		case <-kv.release:
		default:
			close(kv.release)
		}
	})
	mw := New(Config{Store: kv, TTL: time.Minute, SetTimeout: -1})
	ch := get888("/k", mw, okHandler888("v"))
	waitClosed888(t, "the Set to start", kv.entered, 5*time.Second)
	select {
	case a := <-ch:
		t.Fatalf("answered %+v while the Set hangs and the bound is off", a)
	case <-time.After(400 * time.Millisecond):
	}
	if kv.deadline.Load() != nil {
		t.Error("the Set was given a deadline although SetTimeout is negative")
	}
	close(kv.release)
	if a := <-ch; a.body != "v" {
		t.Errorf("after the Set returned: %+v", a)
	}
}

// Zero selects the default.
func TestSetTimeoutZeroSelectsTheDefault888(t *testing.T) {
	kv := newHung888(false)
	t.Cleanup(func() { close(kv.release) })
	mw := New(Config{Store: kv, TTL: time.Minute})
	_ = get888("/k", mw, okHandler888("v"))
	waitClosed888(t, "the Set to start", kv.entered, 5*time.Second)
	d := kv.deadline.Load()
	if d == nil {
		t.Fatal("the Set was given a context without a deadline")
	}
	if until := time.Until(*d); until > defaultSetTimeout || until < defaultSetTimeout-2*time.Second {
		t.Errorf("the Set's context ends in %v, want about defaultSetTimeout (%v)", until, defaultSetTimeout)
	}
}

// The Set that was given up on keeps its bytes: the pooled encode buffer must
// not be recycled under it. The request runs on this goroutine, so the pool's
// next Get here would return the buffer if it had been put back (under -race
// the pool drops a quarter of the puts, hence several tries).
func TestSetTimeoutAbandonedSetKeepsItsBytes888(t *testing.T) {
	for try := range 16 {
		kv := newHung888(false)
		mw := New(Config{Store: kv, TTL: time.Minute, SetTimeout: 20 * time.Millisecond, DisableSingleflight: true})
		c, rec := celeristest.NewContext("GET", "/first", celeristest.WithHandlers(mw, okHandler888(strings.Repeat("A", 100))))
		if err := c.Next(); err != nil || rec.Header("x-cache") != "MISS" {
			t.Fatalf("try %d: %v, x-cache %q", try, err, rec.Header("x-cache"))
		}
		celeristest.ReleaseContext(c)
		held := kv.heldValue.Load()
		if held == nil {
			t.Fatalf("try %d: the hung Set did not start", try)
		}
		p := cacheBufPool.Get().(*[]byte)
		aliased := unsafe.SliceData(*p) == unsafe.SliceData(*held)
		cacheBufPool.Put(p)
		close(kv.release)
		waitClosed888(t, "the released Set to return", kv.finished, 2*time.Second)
		if aliased {
			t.Fatalf("try %d: the encode buffer the abandoned Set holds was put back in the pool", try)
		}
	}
}

// The key is copied for the Set that may outlive the request: a KeyGenerator
// can return a view of the request.
func TestSetTimeoutAbandonedSetKeepsItsKey888(t *testing.T) {
	kv := newHung888(false)
	t.Cleanup(func() {
		select {
		case <-kv.release:
		default:
			close(kv.release)
		}
	})
	buf := []byte("GET /view-of-a-reused-buffer")
	mw := New(Config{Store: kv, TTL: time.Minute, SetTimeout: 100 * time.Millisecond, DisableSingleflight: true,
		KeyGenerator: func(*celeris.Context) string { return unsafe.String(unsafe.SliceData(buf), len(buf)) }})
	if a := <-get888("/k", mw, okHandler888("v")); a.xcache != "MISS" {
		t.Fatalf("request: %+v", a)
	}
	for i := range buf {
		buf[i] = 'z'
	}
	close(kv.release)
	waitClosed888(t, "the released Set to return", kv.finished, 2*time.Second)
	if got := kv.lateOK.Load(); got != 1 {
		t.Errorf("the abandoned Set's key changed under it (lateOK=%d)", got)
	}
}

// A panic in Set reaches the request that made it, bounded or not.
func TestSetTimeoutSetPanicReachesTheLeader888(t *testing.T) {
	for _, bound := range []time.Duration{time.Second, -1} {
		var calls atomic.Int32
		kv := &hookStore{KV: store.NewMemoryKV(), set: func(int32) {
			if calls.Add(1) == 1 {
				panic("boom")
			}
		}}
		mw := New(Config{Store: kv, TTL: time.Minute, SetTimeout: bound})
		a := <-get888("/k", mw, okHandler888("v"))
		if a.panicked != "boom" {
			t.Errorf("SetTimeout %v: panic %v, want boom raised on the request", bound, a.panicked)
		}
	}
}

// The two in-process stores are not bounded (they only take a lock): no
// goroutine is started for them, so their fill is as cheap as before the field,
// and it has happened when fill returns. A type that embeds one is not exempt.
func TestSetTimeoutInProcessStoresRunInline888(t *testing.T) {
	ctx := context.Background()
	ms := NewMemoryStore()
	defer ms.Close()
	kv := store.NewMemoryKV()
	defer kv.Close()
	for name, s := range map[string]store.KV{"MemoryStore": ms, "MemoryKV": kv} {
		if abandoned := fill(ctx, s, "k", []byte("v"), time.Minute, genNow(), time.Nanosecond); abandoned {
			t.Errorf("%s: a fill was given up on", name)
		}
		if _, err := s.Get(ctx, "k"); err != nil {
			t.Errorf("%s: the fill was not stored when fill returned: %v", name, err)
		}
	}
}

// The default is stated in the field's doc, so changing the constant without
// the doc fails here.
func TestDefaultSetTimeoutIsInTheDoc888(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	want := "(defaultSetTimeout: " + secondsWord888(defaultSetTimeout) + ")"
	src = bytes.Join(bytes.Fields(src), []byte(" "))
	src = bytes.ReplaceAll(src, []byte(" // "), []byte(" "))
	if !strings.Contains(string(src), want) {
		t.Errorf("config.go's SetTimeout doc does not say %q: change the doc with the constant", want)
	}
}

func secondsWord888(d time.Duration) string {
	if d%time.Second == 0 {
		n := int(d / time.Second)
		if n == 1 {
			return "1 second"
		}
		return strconv.Itoa(n) + " seconds"
	}
	return d.String()
}
