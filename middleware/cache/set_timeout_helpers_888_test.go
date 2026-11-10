package cache

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/store"
)

// Helpers of the Store.Set bound tests (celeris#888 item 30). They use no API
// that main lacks, so the tests that need only them also compile there.

// get888 runs GET path through mw and the handler in a goroutine and returns
// its answer channel.
func get888(path string, chain ...celeris.HandlerFunc) <-chan answer921 {
	out := make(chan answer921, 1)
	go func() {
		c, rec := celeristest.NewContext("GET", path, celeristest.WithHandlers(chain...))
		var a answer921
		func() {
			defer func() { a.panicked = recover() }()
			a.err = c.Next()
		}()
		a.status, a.body, a.xcache, a.done = rec.StatusCode, string(rec.Body), rec.Header("x-cache"), true
		celeristest.ReleaseContext(c)
		out <- a
	}()
	return out
}

func okHandler888(body string) celeris.HandlerFunc {
	return func(c *celeris.Context) error { return c.String(200, "%s", body) }
}

// hungStore888 is a store whose first Set hangs: until release when it
// ignores its context, or until its context ends when it honours it. It records
// the context's deadline and what the Set was given.
type hungStore888 struct {
	store.KV
	honour    bool
	entered   chan struct{}
	release   chan struct{}
	finished  chan struct{} // closed when the hung Set has returned
	n         atomic.Int32
	deadline  atomic.Pointer[time.Time]
	heldValue atomic.Pointer[[]byte] // the value slice the first Set was given
	lateOK    atomic.Int32           // 1: the bytes and key the hung Set held were unchanged when it woke, 2: changed
	wantVal   []byte
	wantKey   string
}

func newHung888(honour bool) *hungStore888 {
	return &hungStore888{KV: store.NewMemoryKV(), honour: honour,
		entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
}

func (s *hungStore888) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if s.n.Add(1) != 1 {
		return s.KV.Set(ctx, key, value, ttl)
	}
	if d, ok := ctx.Deadline(); ok {
		s.deadline.Store(&d)
	}
	s.wantVal, s.wantKey = bytes.Clone(value), strings.Clone(key)
	s.heldValue.Store(&value)
	close(s.entered)
	defer close(s.finished)
	if s.honour {
		<-ctx.Done()
		return ctx.Err()
	}
	<-s.release
	if bytes.Equal(value, s.wantVal) && key == s.wantKey {
		s.lateOK.Store(1)
	} else {
		s.lateOK.Store(2)
	}
	return nil
}

func waitClosed888(t *testing.T, what string, ch <-chan struct{}, within time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("%s: did not happen within %v", what, within)
	}
}
