package singleflight

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// concurrentTest runs a leader + N-1 waiter pattern deterministically.
//
// Synchronization protocol uses a two-phase key function:
//  1. Leader's KeyFunc call passes through immediately. Leader enters
//     handler and blocks on gate.
//  2. Each waiter's KeyFunc call signals (waiterArrived) from inside
//     the middleware, then blocks on waiterGate.
//  3. The test waits for all N-1 waiterArrived signals, then closes
//     waiterGate to release waiters. Waiters proceed to the group lock
//     and map check. Since the leader is still blocked in the handler,
//     the key is in the map and waiters take the waiter path (wg.Wait).
//  4. After a brief yield to let waiters reach wg.Wait (only a lock
//     acquire + map lookup separates KeyFunc from wg.Wait), the test
//     closes gate to release the leader.
type concurrentTest struct {
	handler celeris.HandlerFunc
	n       int
	method  string
	path    string
	key     string

	entered       chan struct{}
	gate          chan struct{}
	enterOnce     sync.Once
	wg            sync.WaitGroup
	waiterArrived sync.WaitGroup
	waiterGate    chan struct{}
	recs          []*celeristest.ResponseRecorder
	errs          []error
	panics        []any
	ctxs          []*celeris.Context
}

func (ct *concurrentTest) init() {
	if ct.key == "" {
		ct.key = ct.method + "\x00" + ct.path
	}
	if ct.waiterGate == nil {
		ct.waiterGate = make(chan struct{})
	}
	ct.recs = make([]*celeristest.ResponseRecorder, ct.n)
	ct.errs = make([]error, ct.n)
	ct.ctxs = make([]*celeris.Context, ct.n)
	ct.wg.Add(ct.n)
	ct.waiterArrived.Add(ct.n - 1)
}

func (ct *concurrentTest) cleanup(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for _, ctx := range ct.ctxs {
			if ctx != nil {
				celeristest.ReleaseContext(ctx)
			}
		}
	})
}

func (ct *concurrentTest) makeMW() celeris.HandlerFunc {
	var leaderOnce sync.Once
	return New(Config{
		KeyFunc: func(c *celeris.Context) string {
			isLeader := false
			leaderOnce.Do(func() { isLeader = true })
			if !isLeader {
				ct.waiterArrived.Done()
				<-ct.waiterGate
			}
			return ct.key
		},
	})
}

func (ct *concurrentTest) run(t *testing.T) {
	t.Helper()
	ct.init()
	mw := ct.makeMW()

	go func() {
		defer ct.wg.Done()
		chain := []celeris.HandlerFunc{mw, ct.handler}
		opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
		ctx, rec := celeristest.NewContext(ct.method, ct.path, opts...)
		ct.ctxs[0] = ctx
		ct.recs[0] = rec
		ct.errs[0] = ctx.Next()
	}()

	<-ct.entered

	for i := 1; i < ct.n; i++ {
		go func(idx int) {
			defer ct.wg.Done()
			chain := []celeris.HandlerFunc{mw, ct.handler}
			opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
			ctx, rec := celeristest.NewContext(ct.method, ct.path, opts...)
			ct.ctxs[idx] = ctx
			ct.recs[idx] = rec
			ct.errs[idx] = ctx.Next()
		}(i)
	}

	// All waiters are blocked inside KeyFunc inside the middleware.
	ct.waiterArrived.Wait()
	// Release waiters. They proceed to the group lock + map check.
	// The leader is still blocked in the handler, so the key is in the map.
	close(ct.waiterGate)
	// Brief yield: waiters need only a lock acquire + map lookup to reach
	// wg.Wait(). 1ms is orders of magnitude more than needed.
	time.Sleep(time.Millisecond)
	// Release the leader handler.
	close(ct.gate)
	ct.wg.Wait()
	ct.cleanup(t)
}

func (ct *concurrentTest) runWithPanics(t *testing.T) {
	t.Helper()
	ct.init()
	ct.panics = make([]any, ct.n)
	mw := ct.makeMW()

	go func() {
		defer ct.wg.Done()
		defer func() { ct.panics[0] = recover() }()
		chain := []celeris.HandlerFunc{mw, ct.handler}
		opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
		ctx, rec := celeristest.NewContext(ct.method, ct.path, opts...)
		ct.ctxs[0] = ctx
		ct.recs[0] = rec
		ct.errs[0] = ctx.Next()
	}()

	<-ct.entered

	for i := 1; i < ct.n; i++ {
		go func(idx int) {
			defer ct.wg.Done()
			defer func() { ct.panics[idx] = recover() }()
			chain := []celeris.HandlerFunc{mw, ct.handler}
			opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
			ctx, rec := celeristest.NewContext(ct.method, ct.path, opts...)
			ct.ctxs[idx] = ctx
			ct.recs[idx] = rec
			ct.errs[idx] = ctx.Next()
		}(i)
	}

	ct.waiterArrived.Wait()
	close(ct.waiterGate)
	time.Sleep(time.Millisecond)
	close(ct.gate)
	ct.wg.Wait()
	ct.cleanup(t)
}

func TestSingleRequestPassthrough(t *testing.T) {
	var called atomic.Int32
	handler := func(c *celeris.Context) error {
		called.Add(1)
		return c.Blob(200, "text/plain", []byte("ok"))
	}
	mw := New()
	rec, err := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/test")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
	if called.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", called.Load())
	}
}

func TestConcurrentIdenticalRequestsCoalesced(t *testing.T) {
	var callCount atomic.Int32
	ct := &concurrentTest{
		n:       10,
		method:  "GET",
		path:    "/same",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		callCount.Add(1)
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		return c.Blob(200, "text/plain", []byte("coalesced"))
	}
	ct.run(t)

	if callCount.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", callCount.Load())
	}
	for i := range ct.n {
		if ct.errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, ct.errs[i])
		}
		if string(ct.recs[i].Body) != "coalesced" {
			t.Fatalf("goroutine %d: body = %q, want %q", i, ct.recs[i].Body, "coalesced")
		}
	}
}

func TestDifferentKeysNotCoalesced(t *testing.T) {
	var callCount atomic.Int32
	handler := func(c *celeris.Context) error {
		callCount.Add(1)
		return c.Blob(200, "text/plain", []byte("ok"))
	}
	mw := New()

	rec1, err1 := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/a")
	testutil.AssertNoError(t, err1)
	testutil.AssertStatus(t, rec1, 200)

	rec2, err2 := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/b")
	testutil.AssertNoError(t, err2)
	testutil.AssertStatus(t, rec2, 200)

	if callCount.Load() != 2 {
		t.Fatalf("handler called %d times, want 2", callCount.Load())
	}
}

func TestErrorPropagationToWaiters(t *testing.T) {
	testErr := errors.New("handler failed")
	ct := &concurrentTest{
		n:       5,
		method:  "GET",
		path:    "/err",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		return testErr
	}
	ct.run(t)

	for i := 1; i < ct.n; i++ {
		if !errors.Is(ct.errs[i], testErr) {
			t.Fatalf("goroutine %d: err = %v, want %v", i, ct.errs[i], testErr)
		}
	}
}

func TestPanicPropagationToWaiters(t *testing.T) {
	ct := &concurrentTest{
		n:       5,
		method:  "GET",
		path:    "/panic",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		panic("boom")
	}
	ct.runWithPanics(t)

	for i := range ct.n {
		if ct.panics[i] != "boom" {
			t.Fatalf("goroutine %d: panic = %v, want %q", i, ct.panics[i], "boom")
		}
	}
}

func TestSkipPath(t *testing.T) {
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("ok"))
	}
	mw := New(Config{SkipPaths: []string{"/health"}})

	rec, err := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/health")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "x-singleflight")
}

func TestSkipFunction(t *testing.T) {
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("ok"))
	}
	mw := New(Config{
		Skip: func(c *celeris.Context) bool {
			return c.Method() == "POST"
		},
	})

	rec, err := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "POST", "/data")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertNoHeader(t, rec, "x-singleflight")
}

func TestCustomKeyFunc(t *testing.T) {
	var callCount atomic.Int32
	ct := &concurrentTest{
		n:       2,
		method:  "GET",
		path:    "/a",
		key:     "fixed-key",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		callCount.Add(1)
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		return c.Blob(200, "text/plain", []byte("custom"))
	}
	ct.run(t)

	if callCount.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", callCount.Load())
	}
}

func TestDefaultKeySamePathQuery(t *testing.T) {
	fn := defaultKeyFunc

	ctx1, _ := celeristest.NewContext("GET", "/path?a=1&b=2")
	defer celeristest.ReleaseContext(ctx1)
	ctx2, _ := celeristest.NewContext("GET", "/path?a=1&b=2")
	defer celeristest.ReleaseContext(ctx2)

	k1 := fn(ctx1)
	k2 := fn(ctx2)
	if k1 != k2 {
		t.Fatalf("same path+query: keys differ: %q vs %q", k1, k2)
	}
}

func TestDefaultKeyDifferentQuery(t *testing.T) {
	fn := defaultKeyFunc

	ctx1, _ := celeristest.NewContext("GET", "/path?a=1")
	defer celeristest.ReleaseContext(ctx1)
	ctx2, _ := celeristest.NewContext("GET", "/path?a=2")
	defer celeristest.ReleaseContext(ctx2)

	k1 := fn(ctx1)
	k2 := fn(ctx2)
	if k1 == k2 {
		t.Fatalf("different query: keys should differ: %q vs %q", k1, k2)
	}
}

func TestDefaultKeySameParamsDifferentOrder(t *testing.T) {
	fn := defaultKeyFunc

	ctx1, _ := celeristest.NewContext("GET", "/path?b=2&a=1")
	defer celeristest.ReleaseContext(ctx1)
	ctx2, _ := celeristest.NewContext("GET", "/path?a=1&b=2")
	defer celeristest.ReleaseContext(ctx2)

	k1 := fn(ctx1)
	k2 := fn(ctx2)
	if k1 != k2 {
		t.Fatalf("same params different order: keys differ: %q vs %q", k1, k2)
	}
}

func TestResponseHeadersPreservedForWaiters(t *testing.T) {
	ct := &concurrentTest{
		n:       2,
		method:  "GET",
		path:    "/headers",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		c.SetHeader("x-custom", "value123")
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		return c.Blob(200, "text/plain", []byte("headers"))
	}
	ct.run(t)

	if ct.recs[1].Header("x-singleflight") != "HIT" {
		t.Fatal("waiter missing x-singleflight: HIT header")
	}
	if ct.recs[1].Header("x-custom") != "value123" {
		t.Fatalf("waiter x-custom = %q, want %q", ct.recs[1].Header("x-custom"), "value123")
	}
}

func TestResponseBodyPreservedForWaiters(t *testing.T) {
	body := "response-body-preserved"
	ct := &concurrentTest{
		n:       3,
		method:  "GET",
		path:    "/body",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		return c.Blob(200, "application/json", []byte(body))
	}
	ct.run(t)

	for i := range ct.n {
		if string(ct.recs[i].Body) != body {
			t.Fatalf("goroutine %d: body = %q, want %q", i, ct.recs[i].Body, body)
		}
	}
}

func TestNon2xxResponsesCoalesced(t *testing.T) {
	ct := &concurrentTest{
		n:       3,
		method:  "GET",
		path:    "/missing",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		return c.Blob(404, "text/plain", []byte("not found"))
	}
	ct.run(t)

	for i := range ct.n {
		if ct.recs[i].StatusCode != 404 {
			t.Fatalf("goroutine %d: status = %d, want 404", i, ct.recs[i].StatusCode)
		}
	}
}

func TestSingleflightHITHeaderOnWaiters(t *testing.T) {
	ct := &concurrentTest{
		n:       3,
		method:  "GET",
		path:    "/hit",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		return c.Blob(200, "text/plain", []byte("ok"))
	}
	ct.run(t)

	hitCount := 0
	for i := range ct.n {
		if ct.recs[i].Header("x-singleflight") == "HIT" {
			hitCount++
		}
	}
	if hitCount != 2 {
		t.Fatalf("expected 2 HIT headers, got %d", hitCount)
	}
	if ct.recs[0].Header("x-singleflight") == "HIT" {
		t.Fatal("leader should not have x-singleflight: HIT header")
	}
}

func TestDifferentAuthNotCoalesced(t *testing.T) {
	fn := defaultKeyFunc

	ctx1, _ := celeristest.NewContext("GET", "/api/profile",
		celeristest.WithHeader("authorization", "Bearer token-user-a"))
	defer celeristest.ReleaseContext(ctx1)
	ctx2, _ := celeristest.NewContext("GET", "/api/profile",
		celeristest.WithHeader("authorization", "Bearer token-user-b"))
	defer celeristest.ReleaseContext(ctx2)

	k1 := fn(ctx1)
	k2 := fn(ctx2)
	if k1 == k2 {
		t.Fatalf("different Authorization: keys should differ: %q vs %q", k1, k2)
	}
}

func TestDifferentCookieNotCoalesced(t *testing.T) {
	fn := defaultKeyFunc

	ctx1, _ := celeristest.NewContext("GET", "/api/profile",
		celeristest.WithCookie("session", "aaa"))
	defer celeristest.ReleaseContext(ctx1)
	ctx2, _ := celeristest.NewContext("GET", "/api/profile",
		celeristest.WithCookie("session", "bbb"))
	defer celeristest.ReleaseContext(ctx2)

	k1 := fn(ctx1)
	k2 := fn(ctx2)
	if k1 == k2 {
		t.Fatalf("different Cookie: keys should differ: %q vs %q", k1, k2)
	}
}

func TestSameAuthCoalesced(t *testing.T) {
	fn := defaultKeyFunc

	ctx1, _ := celeristest.NewContext("GET", "/api/data",
		celeristest.WithHeader("authorization", "Bearer same-token"))
	defer celeristest.ReleaseContext(ctx1)
	ctx2, _ := celeristest.NewContext("GET", "/api/data",
		celeristest.WithHeader("authorization", "Bearer same-token"))
	defer celeristest.ReleaseContext(ctx2)

	k1 := fn(ctx1)
	k2 := fn(ctx2)
	if k1 != k2 {
		t.Fatalf("same Authorization: keys should match: %q vs %q", k1, k2)
	}
}

func TestNoAuthPublicCoalesced(t *testing.T) {
	fn := defaultKeyFunc

	ctx1, _ := celeristest.NewContext("GET", "/public")
	defer celeristest.ReleaseContext(ctx1)
	ctx2, _ := celeristest.NewContext("GET", "/public")
	defer celeristest.ReleaseContext(ctx2)

	k1 := fn(ctx1)
	k2 := fn(ctx2)
	if k1 != k2 {
		t.Fatalf("no auth (public): keys should match: %q vs %q", k1, k2)
	}
}

func TestDefaultKeyMultiValueQueryOrder(t *testing.T) {
	fn := defaultKeyFunc

	ctx1, _ := celeristest.NewContext("GET", "/path?a=2&a=1")
	defer celeristest.ReleaseContext(ctx1)
	ctx2, _ := celeristest.NewContext("GET", "/path?a=1&a=2")
	defer celeristest.ReleaseContext(ctx2)

	k1 := fn(ctx1)
	k2 := fn(ctx2)
	if k1 != k2 {
		t.Fatalf("same multi-value params different order: keys differ: %q vs %q", k1, k2)
	}
}

func TestMultiValueHeadersPreservedForWaiters(t *testing.T) {
	ct := &concurrentTest{
		n:       2,
		method:  "GET",
		path:    "/cookies",
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	ct.handler = func(c *celeris.Context) error {
		c.AddHeader("set-cookie", "a=1; Path=/")
		c.AddHeader("set-cookie", "b=2; Path=/")
		ct.enterOnce.Do(func() { close(ct.entered) })
		<-ct.gate
		return c.Blob(200, "text/plain", []byte("cookies"))
	}
	ct.run(t)

	// Verify waiter got both set-cookie headers.
	waiterRec := ct.recs[1]
	var setCookieValues []string
	for _, h := range waiterRec.Headers {
		if h[0] == "set-cookie" {
			setCookieValues = append(setCookieValues, h[1])
		}
	}
	if len(setCookieValues) != 2 {
		t.Fatalf("waiter set-cookie count = %d, want 2 (values: %v)", len(setCookieValues), setCookieValues)
	}
	if setCookieValues[0] != "a=1; Path=/" || setCookieValues[1] != "b=2; Path=/" {
		t.Fatalf("waiter set-cookie values = %v, want [a=1; Path=/ b=2; Path=/]", setCookieValues)
	}
}

func TestLeaderErrorPropagated(t *testing.T) {
	testErr := errors.New("handler error")
	handler := func(c *celeris.Context) error {
		_ = c.Blob(200, "text/plain", []byte("ok"))
		return testErr
	}
	mw := New()
	_, err := testutil.RunChain(t, []celeris.HandlerFunc{mw, handler}, "GET", "/err-leader")
	if !errors.Is(err, testErr) {
		t.Fatalf("leader err = %v, want %v", err, testErr)
	}
}

func TestConcurrentSafety(t *testing.T) {
	handler := func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("ok"))
	}
	mw := New()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	ready := make(chan struct{})

	for i := range n {
		go func(idx int) {
			defer wg.Done()
			chain := []celeris.HandlerFunc{mw, handler}
			path := "/race-a"
			if idx%2 == 1 {
				path = "/race-b"
			}
			opts := []celeristest.Option{celeristest.WithHandlers(chain...)}
			ctx, _ := celeristest.NewContext("GET", path, opts...)
			defer celeristest.ReleaseContext(ctx)
			<-ready
			_ = ctx.Next()
		}(i)
	}

	close(ready)
	wg.Wait()
}
