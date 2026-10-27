package cache

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/store"
)

// captureLog888 installs a slog default that sends each record's message and
// attributes (as text) to the returned channel, and restores the old one.
type captureLog888 struct {
	slog.Handler
	ch chan string
}

func (h captureLog888) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&b, " %s=%v", a.Key, a.Value); return true })
	select {
	case h.ch <- b.String():
	default:
	}
	return nil
}

func (h captureLog888) Enabled(context.Context, slog.Level) bool { return true }

func withLog888(t *testing.T) <-chan string {
	t.Helper()
	ch := make(chan string, 8)
	prev := slog.Default()
	slog.SetDefault(slog.New(captureLog888{Handler: slog.NewTextHandler(discard888{}, nil), ch: ch}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return ch
}

type discard888 struct{}

func (discard888) Write(p []byte) (int, error) { return len(p), nil }

// panicLateStore888 is a store whose first Set ignores its context, waits for
// release, and then panics.
type panicLateStore888 struct {
	store.KV
	mu       sync.Mutex
	n        int
	entered  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (s *panicLateStore888) Set(ctx context.Context, key string, v []byte, ttl time.Duration) error {
	s.mu.Lock()
	s.n++
	first := s.n == 1
	s.mu.Unlock()
	if !first {
		return s.KV.Set(ctx, key, v, ttl)
	}
	close(s.entered)
	defer close(s.finished)
	<-s.release
	panic("late boom")
}

// A panic in a Set the middleware had already stopped waiting for has nobody to
// be raised on. It is not dropped: it is logged at Error level with the store's
// type and the panic value, and the request that gave up on the Set is
// unaffected (it was answered at the bound).
func TestSetTimeoutPanicAfterTheBoundIsReported888(t *testing.T) {
	logs := withLog888(t)
	kv := &panicLateStore888{KV: store.NewMemoryKV(), entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	mw := New(Config{Store: kv, TTL: time.Minute, SetTimeout: 50 * time.Millisecond})
	a := <-get888("/k", mw, okHandler888("v"))
	if a.panicked != nil || a.body != "v" || a.xcache != "MISS" {
		t.Fatalf("the request that gave up on the Set: %+v, want 200 v MISS and no panic", a)
	}
	select {
	case m := <-logs:
		t.Fatalf("logged before the Set panicked: %s", m)
	default:
	}
	close(kv.release)
	waitClosed888(t, "the late Set to panic", kv.finished, 2*time.Second)
	select {
	case m := <-logs:
		for _, want := range []string{"late boom", "panicLateStore888"} {
			if !strings.Contains(m, want) {
				t.Errorf("log record %q does not contain %q", m, want)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a panic in a Set the middleware had stopped waiting for was dropped without a trace")
	}
}

// A panic that arrives while the request still waits is raised on it, not
// logged: the report is only for a panic nobody can receive.
func TestSetTimeoutPanicWithinTheBoundIsNotLogged888(t *testing.T) {
	logs := withLog888(t)
	kv := &hookStore{KV: store.NewMemoryKV(), set: func(int32) { panic("early boom") }}
	mw := New(Config{Store: kv, TTL: time.Minute, SetTimeout: time.Second})
	a := <-get888("/k", mw, okHandler888("v"))
	if a.panicked != "early boom" {
		t.Errorf("panic %v, want early boom raised on the request", a.panicked)
	}
	select {
	case m := <-logs:
		t.Errorf("logged a panic the request received: %s", m)
	case <-time.After(100 * time.Millisecond):
	}
}
