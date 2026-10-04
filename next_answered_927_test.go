package celeris_test

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestNextStopsOnceAHandlerAnswered927 pins the chain rule of celeris#927 on
// a bare Context: a handler that answers the request (writes it, or has a
// buffering middleware capture it) ends the chain although it returned
// without Next and without Abort, and the middleware above gets nil from its
// Next. A handler that returns without answering still lets the chain
// continue (the controls).
func TestNextStopsOnceAHandlerAnswered927(t *testing.T) {
	var routeRuns atomic.Int32
	route := func(c *celeris.Context) error {
		routeRuns.Add(1)
		return c.String(200, "route")
	}
	answers := func(c *celeris.Context) error { return c.String(200, "middleware") }
	passes := func(*celeris.Context) error { return nil } // neither answers nor calls Next
	buffering := func(c *celeris.Context) error {
		c.BufferResponse()
		err := c.Next()
		if ferr := c.FlushResponse(); ferr != nil && err == nil {
			err = ferr
		}
		return err
	}
	// discarding captures the response below it, drops it and returns
	// without writing, then calls Next again: the chain stays ended.
	discarding := func(c *celeris.Context) error {
		c.BufferResponse()
		_ = c.Next()
		c.DiscardBufferedResponse()
		if err := c.Next(); err != nil {
			return err
		}
		return c.String(200, "middleware")
	}
	for _, tc := range []struct {
		name      string
		chain     []celeris.HandlerFunc
		body      string
		routeRuns int32
	}{
		{"ended-chain-stays-ended", []celeris.HandlerFunc{discarding, answers, route}, "middleware", 0},
		{"write-ends-the-chain", []celeris.HandlerFunc{answers, route}, "middleware", 0},
		{"buffered-write-ends-the-chain", []celeris.HandlerFunc{buffering, answers, route}, "middleware", 0},
		{"route-chain-first-handler-answers", []celeris.HandlerFunc{answers, route, route}, "middleware", 0},
		{"control/pass-through-continues", []celeris.HandlerFunc{passes, route}, "route", 1},
		{"control/buffered-pass-through-continues", []celeris.HandlerFunc{buffering, passes, route}, "route", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routeRuns.Store(0)
			var outerErr error
			outer := func(c *celeris.Context) error {
				outerErr = c.Next()
				return outerErr
			}
			c, rec := celeristest.NewContextT(t, "GET", "/x", celeristest.WithHandlers(append([]celeris.HandlerFunc{outer}, tc.chain...)...))
			if err := c.Next(); err != nil {
				t.Fatalf("chain returned %v", err)
			}
			if outerErr != nil {
				t.Errorf("the outer middleware's Next returned %v, want nil", outerErr)
			}
			if got := string(rec.Body); got != tc.body || rec.StatusCode != http.StatusOK {
				t.Errorf("response %d %q, want 200 %q", rec.StatusCode, got, tc.body)
			}
			if r := routeRuns.Load(); r != tc.routeRuns {
				t.Errorf("the route ran %d times, want %d", r, tc.routeRuns)
			}
		})
	}
}

// TestNextStillReturnsAnswerersError927: an error from the handler that
// answered still reaches the middleware above, and ends the chain as before.
func TestNextStillReturnsAnswerersError927(t *testing.T) {
	boom := errors.New("boom")
	ran := false
	c, _ := celeristest.NewContextT(t, "GET", "/x", celeristest.WithHandlers(
		func(c *celeris.Context) error { _ = c.String(200, "partial"); return boom },
		func(*celeris.Context) error { ran = true; return nil },
	))
	if err := c.Next(); !errors.Is(err, boom) {
		t.Errorf("Next returned %v, want the answering handler's error", err)
	}
	if ran {
		t.Error("the handler after the error ran")
	}
}

// TestPreMiddlewareThatAnswersSkipsRouting927: a pre-middleware that answers
// the request without Abort skips routing, as one that aborts does.
func TestPreMiddlewareThatAnswersSkipsRouting927(t *testing.T) {
	var routeRuns atomic.Int32
	addr := startServer852(t, celeris.Std, func(s *celeris.Server) {
		s.Pre(func(c *celeris.Context) error {
			if c.Path() == "/maintenance" {
				return c.String(503, "down for maintenance")
			}
			return c.Next()
		})
		s.GET("/*filepath", func(c *celeris.Context) error {
			routeRuns.Add(1)
			return c.String(200, routeBody927)
		})
	})
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for _, rq := range []struct {
		path   string
		status int
		runs   int32
	}{{"/maintenance", 503, 0}, {"/other", 200, 1}} {
		routeRuns.Store(0)
		resp, err := cl.Get("http://" + addr + rq.path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != rq.status || routeRuns.Load() != rq.runs {
			t.Errorf("%s: %d, route ran %d times; want %d and %d", rq.path, resp.StatusCode, routeRuns.Load(), rq.status, rq.runs)
		}
	}
}
