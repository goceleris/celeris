//go:build linux

package iouring

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// errBuckets pulls the celeris#645 breakdown out of a snapshot as a map, so a
// test can assert on the WHOLE partition rather than on the one bucket it
// expects — which is the only way to catch a branch that bumps two buckets or
// the wrong one.
func errBuckets(m engine.EngineMetrics) map[string]uint64 {
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

func bucketDeltas(before, after engine.EngineMetrics) map[string]uint64 {
	b, a := errBuckets(before), errBuckets(after)
	d := make(map[string]uint64, len(a))
	for k := range a {
		d[k] = a[k] - b[k]
	}
	return d
}

// TestPauseAcceptChargesItsTeardownToAcceptCancelled is the measurement
// celeris#645 asked the counter for and could not get.
//
// Pausing an io_uring engine cancels each worker's in-flight multishot accept
// and closes its listen descriptor. The kernel completes the cancelled accept
// with -ECANCELED, and the re-arm that raced the close completes with -EBADF;
// both reach handleAccept's c.Res < 0 branch, and both used to be a bare
// ErrorCount bump indistinguishable from an EMFILE drop or a conn-table
// overflow — the two causes that actually lose a connection.
//
// PauseAccept is not an incidental path on the adaptive engine: it runs on
// every switch, once per worker. So this is the per-switch floor under
// #645's adaptive column, and naming it is what lets the next nightly say
// whether its 421 errors were switch teardown or something that lost traffic.
//
// The assertion is on the SHAPE, not on a magic number: whatever the pause
// costs, all of it must land in AcceptCancelled and none of it anywhere else.
func TestPauseAcceptChargesItsTeardownToAcceptCancelled(t *testing.T) {
	eng, stop := startTestEngine(t)
	defer stop()

	addr := eng.Addr()
	if addr == nil {
		t.Skip("engine never bound")
	}
	// One real connection first, so the engine is demonstrably accepting and
	// the pause has a live multishot accept to cancel on at least one worker.
	c, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()

	before := eng.Metrics()
	if err := eng.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	// PauseAccept returns once the listen descriptors are closed; the -EBADF
	// completion for a re-arm that raced the close lands an iteration later.
	time.Sleep(300 * time.Millisecond)
	after := eng.Metrics()

	d := bucketDeltas(before, after)
	t.Logf("pause cost: ErrorCount +%d, buckets %v",
		after.ErrorCount-before.ErrorCount, d)

	for name, got := range d {
		if name == "AcceptCancelled" {
			continue
		}
		if got != 0 {
			t.Errorf("PauseAccept moved bucket %s by %d, want 0 — a deliberate "+
				"accept teardown must not be indistinguishable from %s", name, got, name)
		}
	}
	if total := after.ErrorCount - before.ErrorCount; total != d["AcceptCancelled"] {
		t.Errorf("ErrorCount moved by %d but AcceptCancelled by %d — the total must "+
			"equal the parts", total, d["AcceptCancelled"])
	}
}

// TestAdoptBeyondConnTableCapCountsAsConnTableCap drives io_uring's adoption
// bounds check and checks WHERE it is counted. It is the same branch as
// epoll's, and before the split both engines folded it into the same number
// as their accept failures.
func TestAdoptBeyondConnTableCapCountsAsConnTableCap(t *testing.T) {
	eng, stop := startTestEngine(t)
	defer stop()

	before := eng.Metrics()
	// AdoptConn itself rejects fd >= fixedFileTableSize up front, so reach
	// the worker-side branch through a worker directly: attachAdoptedFD is
	// the sibling of epoll's and runs on the worker thread.
	eng.mu.Lock()
	w := eng.workers[0]
	eng.mu.Unlock()
	w.attachAdoptedFD(len(w.conns)+1, engine.Carryover{RemoteAddr: "203.0.113.1:1"})

	after := eng.Metrics()
	d := bucketDeltas(before, after)
	if d["ConnTableCap"] != 1 {
		t.Errorf("ConnTableCap moved by %d, want 1", d["ConnTableCap"])
	}
	for name, got := range d {
		if name != "ConnTableCap" && got != 0 {
			t.Errorf("bucket %s moved by %d, want 0", name, got)
		}
	}
	if got := after.ErrorCount - before.ErrorCount; got != 1 {
		t.Errorf("ErrorCount moved by %d, want 1 — the total must equal the parts", got)
	}
}

// respondingHandler writes a real 200 so an abandoning client has a response
// in flight to abandon. testHandler writes nothing, which never reaches a
// send.
type respondingHandler struct{}

func (respondingHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

// startRespondingEngine is startTestEngine with a handler that actually
// replies, so the send path runs.
func startRespondingEngine(t *testing.T) (*Engine, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := resource.Config{
		Addr:      addr,
		Engine:    engine.IOUring,
		Protocol:  engine.HTTP1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: 2},
	}
	e, err := New(cfg, respondingHandler{})
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for dl := time.Now().Add(5 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		<-done
		t.Skip("engine never bound")
	}
	return e, func() { cancel(); <-done }
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

// TestAbandonedResponseCountsAsSendPeerGone is the celeris#645 root-cause
// measurement, pinned.
//
// io_uring counts ONE ErrorCount for every connection whose peer goes away
// before the response flushes — measured at 1:1 with accepts under pure
// abandon churn. epoll, under the identical load, counts ZERO: it has no
// send-failure site feeding ErrorCount at all (see epoll's
// TestAbandonedResponseIsNotAnEngineError).
//
// That asymmetry is most of #645's table. "epoll 0, io_uring 63" is not two
// engines suffering differently, it is one engine counting something the
// other never counts; and on the adaptive engine the count switches ON at the
// promotion, not because anything started failing but because the sub-engine
// that counts it started serving. Anything built on comparing ErrorCount
// across those three columns has to subtract this bucket first.
func TestAbandonedResponseCountsAsSendPeerGone(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping churn test in -short mode")
	}
	eng, stop := startRespondingEngine(t)
	defer stop()

	before := eng.Metrics()
	abandonChurn(t, eng.Addr().String(), time.Second)
	time.Sleep(300 * time.Millisecond)
	after := eng.Metrics()

	d := bucketDeltas(before, after)
	accepts := after.AcceptCount - before.AcceptCount
	t.Logf("abandon churn: accepts +%d, ErrorCount +%d, buckets %v",
		accepts, after.ErrorCount-before.ErrorCount, d)

	if accepts == 0 {
		t.Fatal("no connections were accepted — the load never reached the engine")
	}
	if d["SendPeerGone"] == 0 {
		t.Error("clients that abandoned their responses produced no SendPeerGone; " +
			"either the branch is wired to another bucket or the send never failed")
	}
	for _, name := range []string{"AcceptFDLimit", "ConnTableCap", "ConnRegister", "ListenerRecreate", "TransplantAdopt"} {
		if d[name] != 0 {
			t.Errorf("abandon churn moved %s by %d, want 0 — a client that left is "+
				"not a resource exhaustion or a lost hand-off", name, d[name])
		}
	}
}
