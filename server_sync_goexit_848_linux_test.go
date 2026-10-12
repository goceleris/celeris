//go:build linux

package celeris_test

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
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
//     with engine=<epoll|io_uring>, loop|worker=<that worker's id, chosen not
//     to be 0 when the engine has more than one worker> and
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
	start  string   // CELERIS_ADAPTIVE_START for the adaptive arms ("" = the default, epoll)
}

func arms848() []arm848 {
	return []arm848{
		{"epoll", celeris.Epoll, []string{"epoll"}, ""},
		{"io_uring", celeris.IOUring, []string{"io_uring"}, ""},
		// Adaptive starts on epoll, so this arm covers the epoll call site only.
		{"adaptive", celeris.Adaptive, []string{"epoll", "io_uring"}, ""},
		// Adaptive started on io_uring: the only CI coverage of the io_uring
		// call site through the adaptive engine. The record must say io_uring.
		{"adaptive-iouring", celeris.Adaptive, []string{"io_uring"}, "iouring"},
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
	return start848With(t, cfg, nil)
}

// start848With is start848 with setup, which runs on the new server before
// any route is registered (so it can call Use, NotFound and so on).
func start848With(t *testing.T, cfg celeris.Config, setup func(r *rig848)) *rig848 {
	t.Helper()
	// A start that fails only with ENOMEM (io_uring ring memory still charged
	// to RLIMIT_MEMLOCK, see keptProbeIOUring) is retried with a new server.
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := tryStart848(t, cfg, setup)
		if err == nil {
			return r
		}
		if strings.Contains(err.Error(), "cannot allocate memory") && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		t.Fatalf("server did not start: %v", err)
	}
}

func tryStart848(t *testing.T, cfg celeris.Config, setup func(r *rig848)) (*rig848, error) {
	t.Helper()
	r := &rig848{t: t, logs: &capture848{}}
	cfg.Logger = slog.New(r.logs)
	if cfg.Workers == 0 {
		cfg.Workers = 2
	}
	r.s = celeris.New(cfg)
	if setup != nil {
		setup(r)
	}
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
	r.done = make(chan error, 1)
	var ln net.Listener
	if cfg.Addr == "" {
		var err error
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		go func() { r.done <- r.s.StartWithListenerAndContext(ctx, ln) }()
	} else {
		go func() { r.done <- r.s.StartWithContext(ctx) }()
	}
	addr, err := c714WaitReady(r.s, r.done)
	if err != nil {
		cancel()
		if ln != nil {
			_ = ln.Close()
		}
		return nil, err
	}
	t.Cleanup(cancel)
	r.addr = addr
	if r.logs.total() == 0 {
		t.Fatalf("PREMISE: the server's logger received nothing by the time it was ready")
	}
	return r, nil
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
//
// With avoidZero it keeps dialing until a worker whose id is not 0 holds two:
// a fault on worker 0 cannot tell a loop id that is reported right from one
// that is hard-wired to 0. An engine that runs a single worker has no such
// worker (io_uring at the CI step's 8 MiB RLIMIT_MEMLOCK funds one), so it
// falls back to worker 0 and returns vacuous=true: the id check proves
// nothing there, and the result line says so.
func (r *rig848) place(avoidZero bool) (id string, boom, witness *goexitConn791, others map[string]*goexitConn791, vacuous bool) {
	r.t.Helper()
	held := map[string]*goexitConn791{}
	others = map[string]*goexitConn791{}
	var zero [2]*goexitConn791 // the first two conns on worker 0, kept as the fallback
	for i := range 128 {
		if id != "" && (!avoidZero || len(held) > 1) {
			// A pair is placed (and, when asked to avoid worker 0, a conn
			// sits on a second worker, so the survivors can be checked).
			return id, boom, witness, others, false
		}
		if avoidZero && i >= 24 && len(held) == 1 {
			break // 24 dials all on one worker: a single worker (2^-24 otherwise)
		}
		a := r.dial()
		status, body, err := a.get("/ok")
		if err != nil || status != 200 || !strings.HasPrefix(body, "w=") {
			r.t.Fatalf("PREMISE: /ok before the fault: status %d body %q err %v", status, body, err)
		}
		if avoidZero && body == "w=0" {
			if zero[0] == nil {
				zero[0] = a
			} else if zero[1] == nil {
				zero[1] = a
			}
		} else if prev := held[body]; prev != nil && id == "" {
			id, boom, witness = strings.TrimPrefix(body, "w="), prev, a
		}
		held[body] = a
		others[body] = a
	}
	if id != "" && (!avoidZero || len(held) > 1) {
		return id, boom, witness, others, false
	}
	if avoidZero && len(held) == 1 && zero[1] != nil {
		return "0", zero[0], zero[1], others, true
	}
	r.t.Fatalf("PREMISE: 128 dials found no usable placement (workers seen %d, avoidZero %v)", len(held), avoidZero)
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
			if !child848(t) {
				return
			}
			need848(t, a.eng)
			if a.start != "" {
				t.Setenv("CELERIS_ADAPTIVE_START", a.start)
			}
			r := start848(t, celeris.Config{Engine: a.eng})
			id, boom, witness, others, vacuous := r.place(true)
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
			t.Logf("celeris848 RESULT arm=%s worker=%s boom=%s records=%d records_after_stop=%d witness_on_dead_worker=%s other_workers_answered=%d/%d id_check_vacuous=%v stopped=%v logs=%s",
				a.name, id, goexitClass791(boomErr), len(recs), len(after), witnessClass, otherAnswered, len(others), vacuous, stopped, r.logs.errorsSummary())

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
			// The stack names the call that ended the goroutine, so an operator can find the handler.
			for _, w := range []string{"runtime.Goexit", "tryStart848"} {
				if st := attrStr848(rc.attrs["stack"]); !strings.Contains(st, w) {
					t.Errorf("stack attr lacks %q:\n%s", w, st)
				}
			}
			// Only one loop died: when the engine runs more than one worker, the
			// others still answer (the docs say a loop dies, not the server).
			if !vacuous && len(others) == 0 {
				t.Errorf("PREMISE: no conn was placed on a worker other than the faulted one (%s), so 'the others keep serving' is untested", id)
			}
			if otherAnswered != len(others) {
				t.Errorf("only %d of %d conns on the other workers answered after the fault: more than one loop died", otherAnswered, len(others))
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
			if a.start != "" {
				t.Setenv("CELERIS_ADAPTIVE_START", a.start)
			}
			t.Run("graceful-stop", func(t *testing.T) {
				need848(t, a.eng)
				r := start848(t, celeris.Config{Engine: a.eng})
				r.place(false) // two keep-alive conns stay open across the stop
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
				r.place(false)
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
					r.place(false)
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

// child848 runs a test that kills an event loop in a process of its own. A
// loop that exits on a Goexit never reaches shutdown, so its ring (io_uring
// ring memory is charged to RLIMIT_MEMLOCK per UID), its descriptors and its
// listen socket stay open until the process exits; left in the test binary
// they would starve every later test that starts an io_uring engine. The
// parent re-executes the test binary on this one test, in child mode, and
// judges the child's output; the child (CELERIS_848_CHILD=1) runs the body.
// It reports whether the caller is the child, which then runs the body.
func child848(t *testing.T) bool {
	t.Helper()
	if os.Getenv("CELERIS_848_CHILD") == "1" {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	parts := strings.Split(t.Name(), "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	cmd := exec.Command(exe, "-test.run="+strings.Join(parts, "/"), "-test.v", "-test.count=1", "-test.timeout=180s")
	cmd.Env = append(os.Environ(), "CELERIS_848_CHILD=1")
	out, err := cmd.CombinedOutput()
	for _, ln := range strings.Split(string(out), "\n") {
		if strings.Contains(ln, "celeris848 RESULT") {
			t.Log(strings.TrimSpace(ln))
		}
	}
	text := string(out)
	switch {
	case strings.Contains(text, "--- SKIP: "+t.Name()+" ("):
		t.Skipf("skipped in the child process:\n%s", text)
	case err != nil:
		t.Fatalf("the child process failed: %v\n%s", err, text)
	case !strings.Contains(text, "--- PASS: "+t.Name()+" ("):
		t.Fatalf("the child process did not report --- PASS for %s (not a pass):\n%s", t.Name(), text)
	case strings.Contains(text, "--- SKIP") || strings.Contains(text, "--- FAIL"):
		t.Fatalf("the child process reported a skipped subtest or a failure:\n%s", text)
	}
	return false
}
