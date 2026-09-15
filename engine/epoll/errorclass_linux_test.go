//go:build linux

package epoll

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// errBuckets pulls the celeris#645 breakdown out of a snapshot as a map, so a
// test can assert on the WHOLE partition rather than on the one bucket it
// expects — which is the only way to catch a branch that bumps two buckets or
// the wrong one.
func errBuckets(m celerisengine.EngineMetrics) map[string]uint64 {
	return map[string]uint64{
		"AcceptFDLimit":    m.ErrorAcceptFDLimit,
		"AcceptCancelled":  m.ErrorAcceptCancelled,
		"AcceptOther":      m.ErrorAcceptOther,
		"ConnTableCap":     m.ErrorConnTableCap,
		"ConnRegister":     m.ErrorConnRegister,
		"ListenerRecreate": m.ErrorListenerRecreate,
		"TransplantAdopt":  m.ErrorTransplantAdopt,
		"SendPeerGone":     m.ErrorSendPeerGone,
		"Send":             m.ErrorSend,
		"RequestBody":      m.ErrorRequestBody,
		"Handler":          m.ErrorHandler,
	}
}

// assertOnlyBucket fails unless every unit of the ErrorCount delta between
// before and after landed in want.
func assertOnlyBucket(t *testing.T, before, after celerisengine.EngineMetrics, want string, wantDelta uint64) {
	t.Helper()
	b, a := errBuckets(before), errBuckets(after)
	for name := range a {
		got := a[name] - b[name]
		exp := uint64(0)
		if name == want {
			exp = wantDelta
		}
		if got != exp {
			t.Errorf("bucket %s moved by %d, want %d", name, got, exp)
		}
	}
	if got := after.ErrorCount - before.ErrorCount; got != wantDelta {
		t.Errorf("ErrorCount moved by %d, want %d — the total must equal the parts",
			got, wantDelta)
	}
}

// TestAdoptBeyondConnTableCapCountsAsConnTableCap drives the epoll adoption
// path's hard-cap branch and checks WHERE it is counted, not just that it is.
//
// Before celeris#645 this branch and the accept path's EMFILE branch and the
// epoll_ctl-registration branch all incremented the same atomic, so a nightly
// column reporting 421 errors could not distinguish "a worker is at its
// 64 K per-worker connection limit" — which loses connections permanently —
// from an accept the engine cancelled on purpose.
func TestAdoptBeyondConnTableCapCountsAsConnTableCap(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()

	before := eng.Metrics()
	// connTableSize is the hard cap; a descriptor at or above it can never be
	// indexed into the loop's conn table. AdoptConn only rejects fd < 0 up
	// front, so the cap branch runs on the loop thread.
	if err := eng.AdoptConn(connTableSize+1, celerisengine.Carryover{RemoteAddr: "203.0.113.1:1"}); err != nil {
		t.Fatalf("AdoptConn: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	after := eng.Metrics()
	for after.ErrorCount == before.ErrorCount && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		after = eng.Metrics()
	}
	if after.ErrorCount == before.ErrorCount {
		t.Fatal("the over-cap adoption was never counted at all")
	}
	assertOnlyBucket(t, before, after, "ConnTableCap", 1)
}

// TestEpollReportsNoSendOrRequestBuckets pins the cross-engine reading of the
// split, which is the reason celeris#645 could not interpret its own table.
// epoll published 0 errors next to io_uring's 63 on the same refapp, and that
// looked like an engine that suffered nothing — but epoll has no send-failure
// or handler-error site feeding ErrorCount at all, so those buckets are
// structurally zero here and the comparison was never like for like.
func TestEpollReportsNoSendOrRequestBuckets(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()

	m := eng.Metrics()
	for _, name := range []string{"Send", "SendPeerGone", "RequestBody", "Handler"} {
		if got := errBuckets(m)[name]; got != 0 {
			t.Errorf("epoll reported %s = %d; epoll has no site feeding that bucket, "+
				"so a nonzero value means a branch was wired to the wrong one", name, got)
		}
	}
}

// TestAbandonedResponseIsNotAnEngineError is the epoll half of celeris#645's
// root cause, and the control for io_uring's
// TestAbandonedResponseCountsAsSendPeerGone.
//
// The identical load — dial, send a request, close with SO_LINGER 0 so the
// close is an RST before the response can flush — costs io_uring one
// ErrorCount per connection and epoll none at all. epoll reports a dead peer
// through the handler's OnError and has no write-side site feeding
// ErrorCount, and its accept loop retries ECONNABORTED without counting it.
//
// So an epoll column reading 0 next to an io_uring column reading 63 on the
// same refapp is not evidence that epoll suffered less. This test is what
// makes that statement checkable rather than a reading of the source.
func TestAbandonedResponseIsNotAnEngineError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping churn test in -short mode")
	}
	eng, stop := newRespondingEngine(t)
	defer stop()

	before := eng.Metrics()
	abandonChurn(t, eng.Addr().String(), time.Second)
	time.Sleep(300 * time.Millisecond)
	after := eng.Metrics()

	accepts := after.AcceptCount - before.AcceptCount
	t.Logf("abandon churn: accepts +%d, ErrorCount +%d, buckets %v",
		accepts, after.ErrorCount-before.ErrorCount, bucketDeltas(before, after))
	if accepts == 0 {
		t.Fatal("no connections were accepted — the load never reached the engine")
	}
	if got := after.ErrorCount - before.ErrorCount; got != 0 {
		t.Errorf("epoll counted %d engine error(s) across %d abandoned connections, "+
			"want 0; if epoll has started counting these it is now comparable to "+
			"io_uring's SendPeerGone and celeris#645's table needs re-reading",
			got, accepts)
	}
}

func bucketDeltas(before, after celerisengine.EngineMetrics) map[string]uint64 {
	b, a := errBuckets(before), errBuckets(after)
	d := make(map[string]uint64, len(a))
	for k := range a {
		d[k] = a[k] - b[k]
	}
	return d
}

// respondingHandler writes a real 200 so an abandoning client has a response
// in flight to abandon.
type respondingHandler struct{}

func (respondingHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

// newRespondingEngine is newTestEngine with a handler that actually replies,
// so the write path runs.
func newRespondingEngine(t *testing.T) (*Engine, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	eng, err := New(resource.Config{
		Addr:      addr,
		Protocol:  celerisengine.HTTP1,
		Resources: resource.Resources{Workers: 2},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Listen(ctx) }()
	for dl := time.Now().Add(5 * time.Second); eng.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if eng.Addr() == nil {
		cancel()
		<-done
		t.Skip("engine never bound")
	}
	return eng, func() { cancel(); <-done }
}

// abandonChurn dials addr repeatedly for d, sends a request and closes with
// SO_LINGER 0 so the close is an RST — a client that stopped reading before
// its response arrived, which is what a walker that hit its own timeout does.
func abandonChurn(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	var wg sync.WaitGroup
	deadline := time.Now().Add(d)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				c, err := net.DialTimeout("tcp", addr, time.Second)
				if err != nil {
					continue
				}
				tc, ok := c.(*net.TCPConn)
				if !ok {
					_ = c.Close()
					continue
				}
				_ = tc.SetLinger(0)
				_, _ = tc.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
				_ = tc.Close()
			}
		}()
	}
	wg.Wait()
}
