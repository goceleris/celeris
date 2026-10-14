//go:build linux

package celeris_test

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#848: more than a route's handler runs on the event loop. The docs
// (HandlerFunc, Route.Sync, Config.OnConnect, OnDisconnect, OnExpectContinue)
// say that for a request no route matches, the global middleware and the
// NotFound or MethodNotAllowed handler run inline whatever the routes are
// marked, and the three connection callbacks run on the loop; a Goexit in any
// of them ends the loop and is reported like a handler's. The negative arms
// pin the other half of those sentences: the global middleware of a request
// that matches an async route, and OnExpectContinue on a connection already
// promoted to async dispatch, do not run on the loop.

// do848 is get with a method.
func (a *goexitConn791) do848(method, path string) (status int, body string, err error) {
	_ = a.c.SetDeadline(time.Now().Add(goexitBudget791))
	if _, err := fmt.Fprintf(a.c, "%s %s HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", method, path); err != nil {
		return 0, "", err
	}
	resp, err := http.ReadResponse(a.br, nil)
	if err != nil {
		return 0, "", err
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b), err
}

// waitExits848 waits for the first loop-exit record, then a moment for a second.
func waitExits848(r *rig848) []rec848 {
	var recs []rec848
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
		if recs = r.logs.exits(); len(recs) > 0 {
			break
		}
	}
	time.Sleep(300 * time.Millisecond) // a second record, if the defect doubled it
	return r.logs.exits()
}

// judgeLoopDied848 checks the record of a loop that died and that the loop
// that died is the one the record names: the conns of that worker no longer
// answer and the conns of every other worker do. It returns the dead worker's
// id. conns maps "w=<id>" to a conn held on that worker, as place returns it.
func judgeLoopDied848(t *testing.T, r *rig848, a arm848, recs []rec848, conns map[string]*goexitConn791, wantID, wantStack string) {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("celeris#848: the Goexit produced %d %q records, want exactly 1", len(recs), loopExitMsg848)
	}
	rc := recs[0]
	if rc.level != slog.LevelError {
		t.Errorf("record level = %s, want ERROR", rc.level)
	}
	eng := attrStr848(rc.attrs["engine"])
	if !slices.Contains(a.engine, eng) {
		t.Errorf("engine attr = %q, want one of %v", eng, a.engine)
	}
	key := "loop"
	if eng == "io_uring" {
		key = "worker"
	}
	id := attrStr848(rc.attrs[key])
	if wantID != "" && id != wantID {
		t.Errorf("%s attr = %q, want the faulting worker's id %q (attrs %v)", key, id, wantID, rc.attrs)
	}
	if n, err := strconv.Atoi(attrStr848(rc.attrs["conns"])); err != nil || n < 1 {
		t.Errorf("conns attr = %q, want >= 1", attrStr848(rc.attrs["conns"]))
	}
	for _, w := range []string{"will not be served", "SO_REUSEPORT listener will not accept", "runtime.Goexit"} {
		if !strings.Contains(rc.msg, w) {
			t.Errorf("record text lacks %q: %s", w, rc.msg)
		}
	}
	for _, w := range []string{"runtime.Goexit", wantStack} {
		if st := attrStr848(rc.attrs["stack"]); !strings.Contains(st, w) {
			t.Errorf("stack attr lacks %q:\n%s", w, st)
		}
	}
	// The record means what it says: the conns of the named worker are not
	// answered, and the conns of every other worker are.
	for k, c := range conns {
		_, body, err := c.get("/ok")
		answered := err == nil && body == k
		if k == "w="+id && answered {
			t.Errorf("PREMISE: a conn on %s, the worker the record names, was answered: the loop did not die, so the record would be false", k)
		}
		if k != "w="+id && !answered {
			t.Errorf("a conn on %s (the record names w=%s) was not answered (%s): more than one loop died", k, id, goexitClass791(err))
		}
	}
	stopped := r.stopWithin(loopExitStop848)
	if !stopped {
		t.Errorf("the server did not stop within %v of cancel after the fault", loopExitStop848)
	}
	if after := r.logs.exits(); len(after) != 1 {
		t.Errorf("after the stop there are %d records, want still 1", len(after))
	}
}

// asyncRouteSetup848 gives the server an async route, so the async dispatch
// infrastructure is wired and "the routes are marked .Async()" is true of it.
func asyncRouteSetup848(r *rig848) {
	r.s.GET("/async", func(c *celeris.Context) error { return c.String(200, "async") }).Async()
}

func TestUnmatchedChainGoexitIsLoud848(t *testing.T) {
	type tcase struct {
		name   string
		setup  func(r *rig848)
		method string
		path   string
	}
	cases := []tcase{
		{"global-middleware", func(r *rig848) {
			r.s.Use(func(c *celeris.Context) error {
				if c.Path() == "/nomatch" {
					r.hits.Add(1)
					runtime.Goexit()
				}
				return c.Next()
			})
			asyncRouteSetup848(r)
		}, "GET", "/nomatch"},
		{"notfound", func(r *rig848) {
			asyncRouteSetup848(r)
			r.s.NotFound(func(c *celeris.Context) error {
				r.hits.Add(1)
				runtime.Goexit()
				return nil
			})
		}, "GET", "/nomatch"},
		{"method-not-allowed", func(r *rig848) {
			asyncRouteSetup848(r)
			r.s.MethodNotAllowed(func(c *celeris.Context) error {
				r.hits.Add(1)
				runtime.Goexit()
				return nil
			})
		}, "DELETE", "/ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, a := range arms848()[:3] {
				t.Run(a.name, func(t *testing.T) {
					if !child848(t) {
						return
					}
					need848(t, a.eng)
					r := start848With(t, celeris.Config{Engine: a.eng}, tc.setup)
					id, boom, _, conns, vacuous := r.place(true)
					_, _, berr := boom.do848(tc.method, tc.path)
					if r.hits.Load() != 1 {
						t.Fatalf("INJECTION: the Goexit ran %d times, want 1 (err %v)", r.hits.Load(), berr)
					}
					recs := waitExits848(r)
					t.Logf("celeris848 RESULT UNMATCHED case=%s arm=%s worker=%s records=%d boom=%s id_check_vacuous=%v",
						tc.name, a.name, id, len(recs), goexitClass791(berr), vacuous)
					judgeLoopDied848(t, r, a, recs, conns, id, "TestUnmatchedChainGoexitIsLoud848")
				})
			}
		})
	}
}

// The global middleware of a request that matches an async route runs with
// that route's chain, off the loop: a Goexit there does not end the loop.
func TestGlobalMiddlewareOfAnAsyncRouteKeepsTheLoop848(t *testing.T) {
	for _, a := range arms848()[:3] {
		t.Run(a.name, func(t *testing.T) {
			if !child848(t) {
				return
			}
			need848(t, a.eng)
			r := start848With(t, celeris.Config{Engine: a.eng}, func(r *rig848) {
				r.s.Use(func(c *celeris.Context) error {
					if c.Path() == "/amw" {
						r.hits.Add(1)
						runtime.Goexit()
					}
					return c.Next()
				})
				r.s.GET("/amw", func(c *celeris.Context) error { return c.String(200, "no") }).Async()
			})
			id, boom, witness, conns, vacuous := r.place(true)
			_, _, berr := boom.get("/amw")
			if r.hits.Load() != 1 {
				t.Fatalf("INJECTION: the middleware's Goexit ran %d times, want 1 (err %v)", r.hits.Load(), berr)
			}
			time.Sleep(300 * time.Millisecond)
			_, wbody, werr := witness.get("/ok")
			answered := 0
			for k, c := range conns {
				if _, body, err := c.get("/ok"); err == nil && body == k {
					answered++
				}
			}
			stopped := r.stopWithin(loopExitStop848)
			exits := len(r.logs.exits())
			t.Logf("celeris848 RESULT MW-ASYNC arm=%s worker=%s boom=%s witness=%s(%q) answered=%d/%d loop_exit_records=%d id_check_vacuous=%v stopped=%v",
				a.name, id, goexitClass791(berr), goexitClass791(werr), wbody, answered, len(conns), exits, vacuous, stopped)
			if berr == nil {
				t.Errorf("PREMISE: the request whose middleware Goexited was answered")
			}
			if exits != 0 {
				t.Errorf("a Goexit in the global middleware of an async route produced %d loop-exit records, want 0: it ran on the loop", exits)
			}
			if werr != nil || wbody != "w="+id {
				t.Errorf("the witness conn on the worker of the faulting conn was not answered (%s %q): the loop died", goexitClass791(werr), wbody)
			}
			if answered != len(conns) {
				t.Errorf("only %d of %d conns answered after the fault", answered, len(conns))
			}
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel", loopExitStop848)
			}
		})
	}
}

// The OnConnect and OnDisconnect callbacks run on the loop; so does
// OnExpectContinue on a connection that is not promoted to async dispatch.
func TestCallbackGoexitIsLoud848(t *testing.T) {
	type tcase struct {
		name    string
		trigger func(t *testing.T, r *rig848, id string, boom, witness *goexitConn791)
	}
	var armed atomic.Bool
	var calls atomic.Int64
	fire := func() {
		if armed.CompareAndSwap(true, false) {
			calls.Add(1)
			runtime.Goexit()
		}
	}
	cases := []tcase{
		{"OnConnect", func(t *testing.T, r *rig848, id string, boom, witness *goexitConn791) {
			// A new conn is accepted by whichever worker the kernel picks.
			c := r.dial()
			_ = c
		}},
		{"OnDisconnect", func(t *testing.T, r *rig848, id string, boom, witness *goexitConn791) {
			// The witness conn closes: its worker fires OnDisconnect.
			_ = witness.c.Close()
		}},
		{"OnExpectContinue", func(t *testing.T, r *rig848, id string, boom, witness *goexitConn791) {
			_, _ = fmt.Fprintf(boom.c, "POST /ok HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, a := range arms848()[:3] {
				t.Run(a.name, func(t *testing.T) {
					if !child848(t) {
						return
					}
					need848(t, a.eng)
					armed.Store(false)
					calls.Store(0)
					cfg := celeris.Config{Engine: a.eng}
					switch tc.name {
					case "OnConnect":
						cfg.OnConnect = func(string) { fire() }
					case "OnDisconnect":
						cfg.OnDisconnect = func(string) { fire() }
					case "OnExpectContinue":
						cfg.OnExpectContinue = func(string, string, [][2]string) bool { fire(); return true }
					}
					r := start848With(t, cfg, asyncRouteSetup848)
					id, boom, witness, conns, vacuous := r.place(true)
					armed.Store(true)
					tc.trigger(t, r, id, boom, witness)
					recs := waitExits848(r)
					t.Logf("celeris848 RESULT CALLBACK callback=%s arm=%s worker=%s calls=%d records=%d id_check_vacuous=%v",
						tc.name, a.name, id, calls.Load(), len(recs), vacuous)
					if calls.Load() != 1 {
						t.Fatalf("INJECTION: the callback's Goexit ran %d times, want 1", calls.Load())
					}
					want := id
					if tc.name == "OnConnect" {
						want = "" // the worker that accepted the new conn: the record names it
					}
					if tc.name == "OnDisconnect" {
						// the closed conn is dead for the check below; it was on worker id
						conns["w="+id] = boom
					}
					judgeLoopDied848(t, r, a, recs, conns, want, "TestCallbackGoexitIsLoud848")
				})
			}
		})
	}
}

// OnExpectContinue on a connection already promoted to async dispatch runs on
// that connection's goroutine, not on the loop.
func TestExpectContinueCallbackOfAPromotedConnKeepsTheLoop848(t *testing.T) {
	for _, a := range arms848()[:3] {
		t.Run(a.name, func(t *testing.T) {
			if !child848(t) {
				return
			}
			need848(t, a.eng)
			var calls atomic.Int64
			r := start848With(t, celeris.Config{Engine: a.eng, OnExpectContinue: func(string, string, [][2]string) bool {
				calls.Add(1)
				runtime.Goexit()
				return true
			}}, asyncRouteSetup848)
			id, boom, witness, conns, vacuous := r.place(true)
			// Promote the boom conn: a request to the async route is sticky.
			if st, body, err := boom.get("/async"); err != nil || st != 200 || body != "async" {
				t.Fatalf("PREMISE: GET /async on the boom conn: %d %q %v", st, body, err)
			}
			_, _ = fmt.Fprintf(boom.c, "POST /ok HTTP/1.1\r\nHost: x\r\nContent-Length: 4\r\nExpect: 100-continue\r\n\r\n")
			for dl := time.Now().Add(5 * time.Second); calls.Load() == 0 && time.Now().Before(dl); time.Sleep(10 * time.Millisecond) {
			}
			time.Sleep(300 * time.Millisecond)
			_, wbody, werr := witness.get("/ok")
			answered := 0
			for k, c := range conns {
				if k == "w="+id {
					continue // boom's and witness's worker: witness checked above
				}
				if _, body, err := c.get("/ok"); err == nil && body == k {
					answered++
				}
			}
			stopped := r.stopWithin(loopExitStop848)
			exits := len(r.logs.exits())
			t.Logf("celeris848 RESULT EXPECT-PROMOTED arm=%s worker=%s calls=%d witness=%s(%q) other_workers_answered=%d loop_exit_records=%d id_check_vacuous=%v stopped=%v",
				a.name, id, calls.Load(), goexitClass791(werr), wbody, answered, exits, vacuous, stopped)
			if calls.Load() != 1 {
				t.Fatalf("INJECTION: OnExpectContinue ran %d times, want 1", calls.Load())
			}
			if exits != 0 {
				t.Errorf("a Goexit in OnExpectContinue on a promoted conn produced %d loop-exit records, want 0: it ran on the loop", exits)
			}
			if werr != nil || wbody != "w="+id {
				t.Errorf("the witness conn on the same worker was not answered (%s %q): the loop died", goexitClass791(werr), wbody)
			}
			if !stopped {
				t.Errorf("the server did not stop within %v of cancel", loopExitStop848)
			}
		})
	}
}
