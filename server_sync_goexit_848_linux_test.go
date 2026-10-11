//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#848 through the public API. A route that is not .Async() runs its
// handler on the epoll loop's goroutine or the io_uring worker's, and a
// handler that calls runtime.Goexit (t.FailNow, t.Fatal or t.SkipNow in a
// test's handler, or a library that does) ends that goroutine: nothing can
// recover() a Goexit. The loop's connections and its SO_REUSEPORT listener are
// never served again. The decided contract is "a sync handler must not Goexit"
// plus a loud failure: the engine logs one Error record, naming the engine, the
// loop or worker id and the number of connections it owned.
//
// The positive test sends /boom (a sync route that calls runtime.Goexit) on a
// connection of one worker and requires, on epoll, io_uring and adaptive:
//   - exactly one Error record that says "goroutine ended without returning",
//     with engine=<epoll|io_uring>, loop|worker=<that worker's id> and
//     conns >= the connections held on it, whose text says the connections
//     are not served and the SO_REUSEPORT listener does not accept;
//   - the premise that the record means what it says: the witness conn on that
//     worker is not answered within the budget;
//   - the server still stops after cancel, and no second record appears.
//
// The negative tests require NO such record on the paths that end a loop or a
// worker normally: a graceful stop with a keep-alive conn open, a pause and a
// resume, a start that fails in setup, and a resume that cannot re-create its
// listen socket (the loop ends itself, on purpose, and counts it in
// ErrorListenerRecreate). Each negative test checks that its logger is wired
// (the engine's own Info record arrived), so a silent logger cannot pass it.

const (
	loopExitMsg848    = "goroutine ended without returning"
	loopExitBudget848 = 2 * time.Second
	loopExitStop848   = 15 * time.Second
)

// rec848 is one captured log record.
type rec848 struct {
	level slog.Level
	msg   string
	attrs map[string]slog.Value
}

// capture848 is a slog.Handler that keeps every record.
type capture848 struct {
	mu   sync.Mutex
	recs []rec848
}

func (c *capture848) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture848) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *capture848) WithGroup(string) slog.Handler            { return c }
func (c *capture848) Handle(_ context.Context, r slog.Record) error {
	rc := rec848{level: r.Level, msg: r.Message, attrs: map[string]slog.Value{}}
	r.Attrs(func(a slog.Attr) bool {
		rc.attrs[a.Key] = a.Value.Resolve()
		return true
	})
	c.mu.Lock()
	c.recs = append(c.recs, rc)
	c.mu.Unlock()
	return nil
}

// exits returns the loop-exit records.
func (c *capture848) exits() []rec848 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []rec848
	for _, r := range c.recs {
		if strings.Contains(r.msg, loopExitMsg848) {
			out = append(out, r)
		}
	}
	return out
}

func (c *capture848) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recs)
}

func (c *capture848) errorsSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, r := range c.recs {
		if r.level >= slog.LevelWarn {
			fmt.Fprintf(&b, "[%s %.60s] ", r.level, r.msg)
		}
	}
	return b.String()
}

type arm848 struct {
	name   string
	eng    celeris.EngineType
	engine []string // the engine attr values a record may carry
}

func arms848() []arm848 {
	return []arm848{
		{"epoll", celeris.Epoll, []string{"epoll"}},
		{"io_uring", celeris.IOUring, []string{"io_uring"}},
		{"adaptive", celeris.Adaptive, []string{"epoll", "io_uring"}},
	}
}

// need848 skips (or fails under CELERIS_REQUIRE_IOURING_WORKERS=1) an arm that
// needs io_uring when the kernel has none.
func need848(t *testing.T, eng celeris.EngineType) {
	t.Helper()
	if eng != celeris.IOUring && eng != celeris.Adaptive {
		return
	}
	if ok, p := keptProbeIOUring(); !ok {
		if os.Getenv("CELERIS_REQUIRE_IOURING_WORKERS") == "1" {
			t.Fatalf("io_uring tier=%s kernel=%s, and CELERIS_REQUIRE_IOURING_WORKERS=1 forbids skipping", p.IOUringTier, p.KernelVersion)
		}
		t.Skipf("io_uring tier=%s kernel=%s: no usable io_uring", p.IOUringTier, p.KernelVersion)
	}
}

type rig848 struct {
	t      *testing.T
	s      *celeris.Server
	addr   string
	logs   *capture848
	cancel context.CancelFunc
	done   chan error
	hits   atomic.Int64
}

// start848 serves /boom (sync, Goexit) and /ok (sync, answers with the
// serving worker's id) on a fresh loopback listener, or on addr if it is not
// empty (a fixed port), through cfg.
func start848(t *testing.T, cfg celeris.Config) *rig848 {
	t.Helper()
	r := &rig848{t: t, logs: &capture848{}}
	cfg.Logger = slog.New(r.logs)
	if cfg.Workers == 0 {
		cfg.Workers = 2
	}
	r.s = celeris.New(cfg)
	r.s.GET("/boom", func(c *celeris.Context) error {
		r.hits.Add(1)
		runtime.Goexit()
		return nil
	})
	r.s.GET("/ok", func(c *celeris.Context) error {
		return c.String(200, "w=%d", c.WorkerID())
	})
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	r.done = make(chan error, 1)
	if cfg.Addr == "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { r.done <- r.s.StartWithListenerAndContext(ctx, ln) }()
	} else {
		go func() { r.done <- r.s.StartWithContext(ctx) }()
	}
	addr, err := c714WaitReady(r.s, r.done)
	if err != nil {
		t.Fatalf("server did not start: %v", err)
	}
	r.addr = addr
	if r.logs.total() == 0 {
		t.Fatalf("PREMISE: the server's logger received nothing by the time it was ready")
	}
	return r
}

func (r *rig848) dial() *goexitConn791 {
	r.t.Helper()
	c, err := net.DialTimeout("tcp", r.addr, loopExitBudget848)
	if err != nil {
		r.t.Fatalf("dial: %v", err)
	}
	r.t.Cleanup(func() { _ = c.Close() })
	return &goexitConn791{c: c, br: bufio.NewReader(c)}
}

// place opens keep-alive conns until some worker holds two, a boom conn and a
// witness conn, each proven by /ok, and returns that worker's id with both.
func (r *rig848) place() (id string, boom, witness *goexitConn791, others map[string]*goexitConn791) {
	r.t.Helper()
	held := map[string]*goexitConn791{}
	others = map[string]*goexitConn791{}
	for range 64 {
		a := r.dial()
		status, body, err := a.get("/ok")
		if err != nil || status != 200 || !strings.HasPrefix(body, "w=") {
			r.t.Fatalf("PREMISE: /ok before the fault: status %d body %q err %v", status, body, err)
		}
		if prev := held[body]; prev != nil {
			return strings.TrimPrefix(body, "w="), prev, a, others
		}
		held[body] = a
		others[body] = a
	}
	r.t.Fatal("PREMISE: 64 dials put no worker on two conns")
	return
}

func (r *rig848) stopWithin(d time.Duration) bool {
	r.cancel()
	select {
	case <-r.done:
		return true
	case <-time.After(d):
		return false
	}
}

func attrStr848(v slog.Value) string {
	switch v.Kind() {
	case slog.KindInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(v.Uint64(), 10)
	}
	return v.String()
}

func TestSyncRouteGoexitIsLoud848(t *testing.T) {
	for _, a := range arms848() {
		t.Run(a.name, func(t *testing.T) {
			need848(t, a.eng)
			r := start848(t, celeris.Config{Engine: a.eng})
			id, boom, witness, others := r.place()
			delete(others, "w="+id)

			// The fault: /boom on one of the two conns of worker id.
			_, _, boomErr := boom.get("/boom")
			if r.hits.Load() != 1 {
				t.Fatalf("INJECTION: /boom ran %d times, want 1 (boom err %v)", r.hits.Load(), boomErr)
			}
			var recs []rec848
			for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
				if recs = r.logs.exits(); len(recs) > 0 {
					break
				}
			}
			time.Sleep(300 * time.Millisecond) // a second record, if the defect doubled it
			recs = r.logs.exits()

			// The record means what it says: the loop is gone.
			_, _, werr := witness.get("/ok")
			witnessClass := goexitClass791(werr)
			var otherAnswered int
			for oid, c := range others {
				if st, body, err := c.get("/ok"); err == nil && st == 200 && body == oid {
					otherAnswered++
				}
			}
			stopped := r.stopWithin(loopExitStop848)
			after := r.logs.exits()
			t.Logf("celeris848 RESULT arm=%s worker=%s boom=%s records=%d records_after_stop=%d witness_on_dead_worker=%s other_workers_answered=%d/%d stopped=%v logs=%s",
				a.name, id, goexitClass791(boomErr), len(recs), len(after), witnessClass, otherAnswered, len(others), stopped, r.logs.errorsSummary())

			if len(recs) != 1 {
				t.Fatalf("celeris#848: a sync handler's Goexit produced %d %q Error records, want exactly 1", len(recs), loopExitMsg848)
			}
			rc := recs[0]
			if rc.level != slog.LevelError {
				t.Errorf("record level = %s, want ERROR", rc.level)
			}
			if got := attrStr848(rc.attrs["engine"]); !slices.Contains(a.engine, got) {
				t.Errorf("engine attr = %q, want one of %v", got, a.engine)
			}
			key := "loop"
			if attrStr848(rc.attrs["engine"]) == "io_uring" {
				key = "worker"
			}
			if got := attrStr848(rc.attrs[key]); got != id {
				t.Errorf("%s attr = %q, want the faulting worker's id %q (attrs %v)", key, got, id, rc.attrs)
			}
			if n, err := strconv.Atoi(attrStr848(rc.attrs["conns"])); err != nil || n < 2 {
				t.Errorf("conns attr = %q, want >= 2 (the boom and the witness conn)", attrStr848(rc.attrs["conns"]))
			}
			for _, w := range []string{"will not be served", "SO_REUSEPORT listener will not accept", "runtime.Goexit"} {
				if !strings.Contains(rc.msg, w) {
					t.Errorf("record text lacks %q: %s", w, rc.msg)
				}
			}
			if witnessClass != "timeout" && witnessClass != "closed" {
				t.Errorf("PREMISE: the witness conn on the faulted worker was answered (%s): the loop did not die, so the record would be false", witnessClass)
			}
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel after the fault", loopExitStop848)
			}
			if len(after) != 1 {
				t.Errorf("after the stop there are %d records, want still 1", len(after))
			}
		})
	}
}

// noExit848 judges a negative arm: traffic was served, and no loop-exit record.
func noExit848(t *testing.T, r *rig848, what string) {
	t.Helper()
	if recs := r.logs.exits(); len(recs) != 0 {
		t.Errorf("celeris#848: %s produced %d %q record(s): %v", what, len(recs), loopExitMsg848, recs[0].msg)
	}
}

func TestNormalPathsDoNotReportALoopExit848(t *testing.T) {
	for _, a := range arms848() {
		t.Run(a.name, func(t *testing.T) {
			t.Run("graceful-stop", func(t *testing.T) {
				need848(t, a.eng)
				r := start848(t, celeris.Config{Engine: a.eng})
				r.place() // two keep-alive conns stay open across the stop
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := r.s.Shutdown(ctx); err != nil {
					t.Logf("Shutdown: %v", err)
				}
				select {
				case <-r.done:
				case <-time.After(loopExitStop848):
					t.Fatal("the server did not stop")
				}
				noExit848(t, r, "a graceful Shutdown with keep-alive conns open")
			})
			t.Run("cancel", func(t *testing.T) {
				need848(t, a.eng)
				r := start848(t, celeris.Config{Engine: a.eng})
				r.place()
				if !r.stopWithin(loopExitStop848) {
					t.Fatal("the server did not stop")
				}
				noExit848(t, r, "a context cancel with keep-alive conns open")
			})
			// The adaptive engine has no accept control (PauseAccept returns
			// ErrAcceptControlNotSupported); its switches, which stop and start
			// loops, are TestAdaptiveSwitchIsNotALoopExit848 in internal/engine/iouring.
			if a.eng != celeris.Adaptive {
				t.Run("pause-resume", func(t *testing.T) {
					need848(t, a.eng)
					r := start848(t, celeris.Config{Engine: a.eng, DisableDeferAccept: true})
					r.place()
					if err := r.s.PauseAccept(); err != nil {
						t.Fatalf("PauseAccept: %v", err)
					}
					if err := r.s.ResumeAccept(); err != nil {
						t.Fatalf("ResumeAccept: %v", err)
					}
					// Served again after the resume (the listener was re-created).
					var served bool
					for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl) && !served; time.Sleep(20 * time.Millisecond) {
						c, err := net.DialTimeout("tcp", r.addr, time.Second)
						if err != nil {
							continue
						}
						g := &goexitConn791{c: c, br: bufio.NewReader(c)}
						if st, _, err := g.get("/ok"); err == nil && st == 200 {
							served = true
						}
						_ = c.Close()
					}
					if !served {
						t.Fatal("PREMISE: no request was served after ResumeAccept")
					}
					if !r.stopWithin(loopExitStop848) {
						t.Fatal("the server did not stop")
					}
					noExit848(t, r, "PauseAccept then ResumeAccept")
				})
			}
			t.Run("setup-failure", func(t *testing.T) {
				need848(t, a.eng)
				// A plain listener (no SO_REUSEPORT) holds the port: every
				// loop's socket fails in setup, reports it on ready and returns.
				hold, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = hold.Close() }()
				logs := &capture848{}
				s := celeris.New(celeris.Config{Engine: a.eng, Workers: 2, Addr: hold.Addr().String(), Logger: slog.New(logs)})
				s.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
				done := make(chan error, 1)
				go func() { done <- s.Start() }()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("PREMISE: Start succeeded on a port a plain listener holds")
					}
					t.Logf("celeris848 setup failure: %v", err)
				case <-time.After(loopExitStop848):
					t.Fatal("PREMISE: Start did not fail on a taken port")
				}
				// The loops were joined by the failed start; give a stray
				// record the budget a late one would need.
				time.Sleep(200 * time.Millisecond)
				if recs := logs.exits(); len(recs) != 0 {
					t.Errorf("celeris#848: a start that failed in setup produced %d %q record(s): %v", len(recs), loopExitMsg848, recs[0].msg)
				}
			})
			if a.eng == celeris.Adaptive {
				// The adaptive engine runs the same loops; its lost listener
				// is the epoll and io_uring arms' (same code).
				return
			}
			t.Run("lost-listener", func(t *testing.T) {
				need848(t, a.eng)
				tmp, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := tmp.Addr().String()
				_ = tmp.Close()
				r := start848(t, celeris.Config{Engine: a.eng, DisableDeferAccept: true, Addr: addr})
				if err := r.s.PauseAccept(); err != nil {
					t.Fatalf("PauseAccept: %v", err)
				}
				ln, err := net.Listen("tcp", addr)
				if err != nil {
					t.Fatalf("a fixed port has no hold: a plain listener should take it while paused: %v", err)
				}
				defer func() { _ = ln.Close() }()
				if err := r.s.ResumeAccept(); err != nil {
					t.Fatalf("ResumeAccept: %v", err)
				}
				var lost uint64
				for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
					if lost = r.s.EngineInfo().Metrics.ErrorListenerRecreate; lost > 0 {
						break
					}
				}
				if lost == 0 {
					t.Fatal("PREMISE: no loop reported a lost listener (ErrorListenerRecreate stayed 0)")
				}
				time.Sleep(200 * time.Millisecond)
				if !r.stopWithin(loopExitStop848) {
					t.Fatal("the server did not stop")
				}
				noExit848(t, r, fmt.Sprintf("a resume that could not re-create %d listener(s): the loop ends itself on purpose", lost))
			})
		})
	}
}
