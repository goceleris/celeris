//go:build linux

package celeris_test

import (
	"context"
	"log/slog"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#848 on HTTP/2 (h2c, prior knowledge). The docs state two outcomes of
// a runtime.Goexit in a handler, by where the stream runs:
//   - an inline stream (a GET on a route that is not async) runs on the event
//     loop's goroutine: the loop ends and the engine logs the loop-exit record;
//   - a stream on the h2 worker pool (here: an async route) does not run on the
//     loop: the loop keeps serving, and the engine does not report a loop exit.
//
// The second is a documented limit, not a promise of loudness: that stream gets
// no answer (the RESULT line logs the Error records it saw, 0 today). The test
// pins only that the loop survives and no loop-exit record is written.

type h2rig848 struct {
	s      *celeris.Server
	addr   string
	logs   *capture848
	cancel context.CancelFunc
	done   chan error
	hits   atomic.Int64
}

func (r *h2rig848) newServer(eng celeris.EngineType) {
	r.s = celeris.New(celeris.Config{Engine: eng, Protocol: celeris.H2C, Workers: 2, Logger: slog.New(r.logs)})
	r.s.GET("/boom", func(c *celeris.Context) error {
		r.hits.Add(1)
		runtime.Goexit()
		return nil
	})
	r.s.GET("/aboom", func(c *celeris.Context) error {
		r.hits.Add(1)
		runtime.Goexit()
		return nil
	}).Async()
	r.s.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
}

func startH2848(t *testing.T, eng celeris.EngineType) *h2rig848 {
	t.Helper()
	r := &h2rig848{logs: &capture848{}}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.newServer(eng)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		r.done = make(chan error, 1)
		go func() { r.done <- r.s.StartWithListenerAndContext(ctx, ln) }()
		if r.addr, err = c714WaitReady(r.s, r.done); err == nil {
			break
		}
		cancel()
		_ = ln.Close()
		// A start that fails only with ENOMEM (io_uring ring memory still
		// charged to RLIMIT_MEMLOCK) is retried with a new server.
		if !strings.Contains(err.Error(), "cannot allocate memory") || time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Cleanup(r.cancel)
	return r
}

func (r *h2rig848) stopWithin(d time.Duration) bool {
	r.cancel()
	select {
	case <-r.done:
		return true
	case <-time.After(d):
		return false
	}
}

// h2Probe848 sends GET /ok on fresh h2c connections, up to n, and returns how
// many were answered before the first one that was not (stopOnMiss), or in all.
// With two workers a new connection lands on either by the kernel's hash, so
// one probe cannot tell a dead loop from a live one; n of them can (all n on
// the live worker is 1 in 2^n).
func h2Probe848(addr string, n int, d time.Duration, stopOnMiss bool) (answered, tried int) {
	for range n {
		tried++
		if ok, _, _ := h2StreamEnds837(addr, "/ok", d); ok {
			answered++
		} else if stopOnMiss {
			return
		}
	}
	return
}

func h2Class848(ended bool, err error) string {
	if ended {
		return "ended"
	}
	return goexitClass791(err)
}

func errorRecords848(c *capture848) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if r.level >= slog.LevelError {
			n++
		}
	}
	return n
}

func TestH2InlineStreamGoexitIsLoud848(t *testing.T) {
	for _, a := range arms848()[:2] { // epoll, io_uring (adaptive runs the same loops)
		t.Run(a.name, func(t *testing.T) {
			if !child848(t) {
				return
			}
			need848(t, a.eng)
			r := startH2848(t, a.eng)
			okBefore, _, errB := h2StreamEnds837(r.addr, "/ok", 2*time.Second)
			ended, _, err := h2StreamEnds837(r.addr, "/boom", 2*time.Second)
			var recs []rec848
			for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
				if recs = r.logs.exits(); len(recs) > 0 {
					break
				}
			}
			time.Sleep(300 * time.Millisecond) // a second record, if the defect doubled it
			recs = r.logs.exits()
			answered, tried := h2Probe848(r.addr, 20, 500*time.Millisecond, true)
			stopped := r.stopWithin(loopExitStop848)
			t.Logf("celeris848 RESULT H2-INLINE arm=%s hits=%d ok_before=%s boom=%s records=%d probes_until_unanswered=%d/20 stopped=%v",
				a.name, r.hits.Load(), h2Class848(okBefore, errB), h2Class848(ended, err), len(recs), tried, stopped)
			if !okBefore {
				t.Fatalf("PREMISE: /ok before the fault was not answered: %s", h2Class848(okBefore, errB))
			}
			if r.hits.Load() != 1 {
				t.Fatalf("INJECTION: /boom ran %d times, want 1", r.hits.Load())
			}
			if len(recs) != 1 {
				t.Fatalf("celeris#848: an inline h2 stream's Goexit produced %d %q records, want exactly 1", len(recs), loopExitMsg848)
			}
			if got := attrStr848(recs[0].attrs["engine"]); !slices.Contains(a.engine, got) {
				t.Errorf("engine attr = %q, want one of %v", got, a.engine)
			}
			if answered == tried {
				t.Errorf("PREMISE: all %d fresh conns after the fault were answered, so the loop did not die and the record would be false", tried)
			}
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel after the fault", loopExitStop848)
			}
		})
	}
}

func TestH2AsyncStreamGoexitKeepsTheLoop848(t *testing.T) {
	for _, a := range arms848()[:2] {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := startH2848(t, a.eng)
			ended, _, err := h2StreamEnds837(r.addr, "/aboom", 2*time.Second)
			time.Sleep(300 * time.Millisecond)
			answered, tried := h2Probe848(r.addr, 20, time.Second, false)
			errs := errorRecords848(r.logs)
			stopped := r.stopWithin(loopExitStop848)
			exits := len(r.logs.exits())
			t.Logf("celeris848 RESULT H2-ASYNC arm=%s hits=%d aboom=%s answered_after=%d/%d error_records=%d loop_exit_records=%d stopped=%v",
				a.name, r.hits.Load(), h2Class848(ended, err), answered, tried, errs, exits, stopped)
			if r.hits.Load() != 1 {
				t.Fatalf("INJECTION: /aboom ran %d times, want 1", r.hits.Load())
			}
			if ended {
				t.Errorf("PREMISE: the stream whose handler Goexited was answered")
			}
			if answered != tried {
				t.Errorf("an async h2 route's Goexit ended a loop: %d of %d fresh conns were answered afterwards", answered, tried)
			}
			if exits != 0 {
				t.Errorf("an async h2 stream's Goexit produced %d loop-exit records, want 0 (the loop did not exit)", exits)
			}
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel", loopExitStop848)
			}
		})
	}
}
